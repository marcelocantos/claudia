// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// DefaultOllamaEndpoint is where a local Ollama daemon listens.
const DefaultOllamaEndpoint = "http://127.0.0.1:11434"

// ollamaSettings resolves where to send a request and which model runs
// it.
type ollamaSettings struct {
	Endpoint string
	Model    string
}

func resolveOllamaSettings(getenv func(string) string, model string) (ollamaSettings, error) {
	s := ollamaSettings{
		Endpoint: strings.TrimSuffix(getenv("CLAUDIA_OLLAMA_ENDPOINT"), "/"),
		Model:    model,
	}
	if s.Endpoint == "" {
		s.Endpoint = DefaultOllamaEndpoint
	}
	if s.Model == "" {
		s.Model = getenv("CLAUDIA_OLLAMA_MODEL")
	}
	if s.Model == "" {
		return ollamaSettings{}, fmt.Errorf("claudia: ollama needs a model — set TaskConfig.Model or CLAUDIA_OLLAMA_MODEL")
	}
	return s, nil
}

// ollamaTaskBackend runs one-shot prompts against a local Ollama daemon.
//
// Local inference is a PROVIDER, not a special case: it satisfies the
// same task contract as every other backend, so a caller swaps to it by
// changing a config field. What differs is what it costs — latency
// rather than money — which is why CapabilityCost is unsupported here
// rather than reported as zero dollars.
type ollamaTaskBackend struct {
	// http is injectable so the backend is testable without a daemon.
	http *http.Client
}

func (ollamaTaskBackend) Capabilities() providerCapabilities {
	return providerCapabilities{Task: true}
}

type ollamaGenerateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

// ollamaChunk is one NDJSON frame from /api/generate.
type ollamaChunk struct {
	Model           string `json:"model"`
	Response        string `json:"response"`
	Done            bool   `json:"done"`
	Error           string `json:"error,omitempty"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	TotalDuration   int64  `json:"total_duration"`
}

func (b ollamaTaskBackend) RunTask(ctx context.Context, req taskRunRequest) (*taskRun, error) {
	settings, err := resolveOllamaSettings(os.Getenv, req.Model)
	if err != nil {
		return nil, err
	}
	if len(req.DisallowTools) > 0 {
		// Fail closed rather than silently ignoring a restriction the
		// caller asked for. Ollama's generate endpoint runs no tools at
		// all, so there is nothing to restrict and nothing to honour.
		return nil, &CapabilityError{
			Provider:   ProviderOllama,
			Capability: CapabilityToolRestrictions,
			Status:     CapabilityUnsupported,
			Reason:     providerCapabilityClaim(ProviderOllama, CapabilityToolRestrictions).reason,
		}
	}

	body, err := json.Marshal(ollamaGenerateRequest{Model: settings.Model, Prompt: req.Prompt, Stream: true})
	if err != nil {
		return nil, err
	}

	streamCtx, cancel := context.WithCancel(ctx)
	httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, settings.Endpoint+"/api/generate", bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := b.http
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("claudia: ollama: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("claudia: ollama: %s", resp.Status)
	}

	out := make(chan TaskEvent, 16)
	go func() {
		defer close(out)
		defer cancel()
		defer resp.Body.Close()

		send := func(ev TaskEvent) bool {
			if req.RawLog != nil {
				if raw, err := json.Marshal(ev); err == nil {
					req.RawLog(raw)
				}
			}
			select {
			case out <- ev:
				return true
			case <-streamCtx.Done():
				return false
			}
		}

		// The resolved model goes out first, so a caller can assert
		// requested against resolved exactly as with every other
		// provider.
		if !send(TaskEvent{Type: TaskEventInit, Model: settings.Model}) {
			return
		}

		var usage Usage
		var durationMs float64
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			var chunk ollamaChunk
			if err := json.Unmarshal(line, &chunk); err != nil {
				send(TaskEvent{Type: TaskEventError, IsError: true, ErrorMsg: fmt.Sprintf("decode ollama frame: %v", err)})
				return
			}
			if chunk.Error != "" {
				send(TaskEvent{Type: TaskEventError, IsError: true, ErrorMsg: chunk.Error})
				return
			}
			if chunk.Response != "" && !send(TaskEvent{Type: TaskEventText, Content: chunk.Response}) {
				return
			}
			if chunk.Done {
				usage = Usage{InputTokens: chunk.PromptEvalCount, OutputTokens: chunk.EvalCount}
				durationMs = float64(chunk.TotalDuration) / 1e6
				break
			}
		}
		if err := scanner.Err(); err != nil {
			send(TaskEvent{Type: TaskEventError, IsError: true, ErrorMsg: err.Error()})
			return
		}

		// CostUSD stays zero and means it: local inference has no price
		// per token, which is a different claim from "the price is
		// unknown". CapabilityCost is unsupported so a caller cannot
		// read this as a measured spend.
		send(TaskEvent{
			Type:       TaskEventResult,
			Model:      settings.Model,
			Usage:      usage,
			DurationMs: durationMs,
		})
	}()

	return &taskRun{
		events:    out,
		interrupt: func() error { cancel(); return nil },
	}, nil
}
