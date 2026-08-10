// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeOllamaDaemon streams NDJSON frames the way Ollama's generate
// endpoint does, so the backend is exercised without a daemon.
func fakeOllamaDaemon(t *testing.T, frames []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("path = %q, want /api/generate", r.URL.Path)
		}
		var req ollamaGenerateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if !req.Stream {
			t.Error("stream = false; the backend should stream")
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, f := range frames {
			_, _ = w.Write([]byte(f + "\n"))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOllamaTaskStreamsTextAndReportsTokens(t *testing.T) {
	srv := fakeOllamaDaemon(t, []string{
		`{"model":"gemma4:12b","response":"hello ","done":false}`,
		`{"model":"gemma4:12b","response":"world","done":false}`,
		`{"model":"gemma4:12b","done":true,"prompt_eval_count":12,"eval_count":5,"total_duration":2500000000}`,
	})
	t.Setenv("CLAUDIA_OLLAMA_ENDPOINT", srv.URL)

	backend := ollamaTaskBackend{http: srv.Client()}
	run, err := backend.RunTask(context.Background(), taskRunRequest{Model: "gemma4:12b", Prompt: "hi"})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	var text strings.Builder
	var init, result *TaskEvent
	for ev := range run.events {
		switch ev.Type {
		case TaskEventInit:
			e := ev
			init = &e
		case TaskEventText:
			text.WriteString(ev.Content)
		case TaskEventResult:
			e := ev
			result = &e
		case TaskEventError:
			t.Fatalf("unexpected error event: %s", ev.ErrorMsg)
		}
	}

	if init == nil || init.Model != "gemma4:12b" {
		t.Errorf("init = %+v; the resolved model must go out first", init)
	}
	if text.String() != "hello world" {
		t.Errorf("text = %q", text.String())
	}
	if result == nil {
		t.Fatal("no result event")
	}
	if result.Usage.InputTokens != 12 || result.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v, want 12 in / 5 out", result.Usage)
	}
	if result.DurationMs != 2500 {
		t.Errorf("duration = %v ms, want 2500", result.DurationMs)
	}
	// Zero dollars, and it MEANS zero rather than "unknown" — which is
	// exactly why the cost capability is unsupported rather than
	// claiming a measured spend.
	if result.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want 0", result.CostUSD)
	}
}

func TestOllamaSurfacesDaemonErrorsRatherThanTruncating(t *testing.T) {
	srv := fakeOllamaDaemon(t, []string{
		`{"response":"partial","done":false}`,
		`{"error":"model gemma4:99b not found"}`,
	})
	t.Setenv("CLAUDIA_OLLAMA_ENDPOINT", srv.URL)

	run, err := ollamaTaskBackend{http: srv.Client()}.RunTask(
		context.Background(), taskRunRequest{Model: "gemma4:99b", Prompt: "hi"})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	var sawError bool
	for ev := range run.events {
		if ev.Type == TaskEventError {
			sawError = true
			if !strings.Contains(ev.ErrorMsg, "not found") {
				t.Errorf("error = %q", ev.ErrorMsg)
			}
		}
	}
	if !sawError {
		t.Error("a daemon error ended the stream without an error event")
	}
}

func TestOllamaRefusesToolRestrictionsRatherThanIgnoringThem(t *testing.T) {
	t.Setenv("CLAUDIA_OLLAMA_ENDPOINT", "http://127.0.0.1:1")

	_, err := ollamaTaskBackend{}.RunTask(context.Background(), taskRunRequest{
		Model: "gemma4:12b", Prompt: "hi", DisallowTools: []string{"Bash"},
	})
	var capErr *CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %v, want a *CapabilityError", err)
	}
	if capErr.Provider != ProviderOllama || capErr.Capability != CapabilityToolRestrictions {
		t.Errorf("capability error = %+v", capErr)
	}
	if capErr.Reason == "" {
		t.Error("refusal carries no reason")
	}
}

func TestOllamaSettingsResolution(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	s, err := resolveOllamaSettings(env(nil), "gemma4:12b")
	if err != nil {
		t.Fatal(err)
	}
	if s.Endpoint != DefaultOllamaEndpoint {
		t.Errorf("endpoint = %q, want the default", s.Endpoint)
	}

	s, err = resolveOllamaSettings(env(map[string]string{
		"CLAUDIA_OLLAMA_ENDPOINT": "http://elsewhere:11434/",
		"CLAUDIA_OLLAMA_MODEL":    "from-env",
	}), "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Endpoint != "http://elsewhere:11434" {
		t.Errorf("endpoint = %q; the trailing slash should be trimmed", s.Endpoint)
	}
	if s.Model != "from-env" {
		t.Errorf("model = %q", s.Model)
	}

	// An unnamed model is an error rather than a silent default: which
	// model ran is not something to guess.
	if _, err := resolveOllamaSettings(env(nil), ""); err == nil {
		t.Error("a task with no model was accepted")
	}
}

// The matrix must state a position on every capability, and the claims
// must match what the backend actually wires.
func TestOllamaCapabilityMatrixMatchesTheBackend(t *testing.T) {
	matrix := ProviderCapabilityMatrix(ProviderOllama)
	if matrix[CapabilityTask] != CapabilitySupported {
		t.Error("task is not claimed as supported")
	}
	for _, c := range []Capability{
		CapabilitySession, CapabilityResume, CapabilityRewind, CapabilityCost,
		CapabilityTmuxAttach, CapabilityTerminalLog, CapabilityPermissionMode,
		CapabilityToolRestrictions, CapabilityImageInput, CapabilityWebSearch,
	} {
		if matrix[c] == CapabilitySupported {
			t.Errorf("%s is claimed as supported but nothing wires it", c)
		}
	}

	caps := ollamaTaskBackend{}.Capabilities()
	if !caps.Task {
		t.Error("backend does not claim Task")
	}
	if caps.Session || caps.Resume || caps.Rewind || caps.Cost || caps.Permissions || caps.TmuxAttach || caps.TerminalBytes {
		t.Errorf("backend claims more than the matrix does: %+v", caps)
	}

	// Cost in particular: unsupported is a different statement from
	// "zero", and the reason has to say which.
	if reason := ProviderCapabilityReason(ProviderOllama, CapabilityCost); !strings.Contains(reason, "latency") {
		t.Errorf("cost reason does not explain what local inference costs instead: %q", reason)
	}
}

func TestNewTaskSelectsTheOllamaBackend(t *testing.T) {
	task := NewTask(TaskConfig{Provider: ProviderOllama, Model: "gemma4:12b"})
	if _, ok := task.backend.(ollamaTaskBackend); !ok {
		t.Errorf("backend = %T, want ollamaTaskBackend", task.backend)
	}
}
