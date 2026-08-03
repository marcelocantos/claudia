// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestBedrockTaskLiveSmoke is residual live verification against a work
// account. Skip unless CLAUDIA_BEDROCK_LIVE=1. Not used as sole T12 evidence.
func TestBedrockTaskLiveSmoke(t *testing.T) {
	if os.Getenv(bedrockLiveEnv) != "1" {
		t.Skipf("set %s=1 to run live Bedrock ConverseStream smoke", bedrockLiveEnv)
	}

	model := os.Getenv(bedrockModelEnv)
	if model == "" {
		t.Fatalf("%s must be set for live smoke (Bedrock model id or inference profile)", bedrockModelEnv)
	}
	if _, err := resolveBedrockSettings(os.Getenv, model); err != nil {
		t.Fatal(err)
	}

	task := NewTask(TaskConfig{
		Provider: ProviderBedrock,
		ID:       "bedrock-live-smoke",
		Model:    model,
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
	t.Logf("live bedrock ok: %q", texts.String())
}
