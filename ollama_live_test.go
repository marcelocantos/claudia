// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const ollamaLiveEnv = "CLAUDIA_OLLAMA_LIVE"

// TestOllamaTaskLiveSmoke hits a real local Ollama daemon. Skip unless
// CLAUDIA_OLLAMA_LIVE=1. Needs a model (TaskConfig.Model or
// CLAUDIA_OLLAMA_MODEL) and a reachable endpoint (default 127.0.0.1:11434).
func TestOllamaTaskLiveSmoke(t *testing.T) {
	if os.Getenv(ollamaLiveEnv) != "1" {
		t.Skipf("set %s=1 to run live Ollama /api/generate smoke", ollamaLiveEnv)
	}
	settings, err := resolveOllamaSettings(os.Getenv, "")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(settings.Endpoint + "/api/tags")
	if err != nil {
		t.Skipf("ollama not reachable at %s: %v", settings.Endpoint, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("ollama at %s returned %s", settings.Endpoint, resp.Status)
	}

	task := NewTask(TaskConfig{
		Provider: ProviderOllama,
		ID:       "ollama-live-smoke",
		Model:    settings.Model,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ch, err := task.Run(ctx, "Reply with exactly the single word pong and nothing else.")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var texts strings.Builder
	var sawResult bool
	var errMsg string
	for ev := range ch {
		switch ev.Type {
		case TaskEventText:
			texts.WriteString(ev.Content)
		case TaskEventResult:
			sawResult = true
			if ev.Content != "" {
				texts.Reset()
				texts.WriteString(ev.Content)
			}
		case TaskEventError:
			errMsg = ev.ErrorMsg
		}
	}
	if errMsg != "" {
		t.Fatalf("stream error: %s", errMsg)
	}
	if !sawResult {
		t.Fatal("no TaskEventResult")
	}
	got := strings.ToLower(strings.TrimSpace(texts.String()))
	if !strings.Contains(got, "pong") {
		t.Fatalf("assistant text %q does not contain pong", texts.String())
	}
	t.Logf("live ollama ok: %q", texts.String())
}
