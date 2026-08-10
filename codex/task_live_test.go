// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveCodexTaskRun drives one real `codex exec --json` turn on the
// ChatGPT subscription. Gated on CLAUDIA_CODEX_LIVE=1 or CLAUDIA_LIVE=1.
func TestLiveCodexTaskRun(t *testing.T) {
	if os.Getenv("CLAUDIA_CODEX_LIVE") == "" && os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_CODEX_LIVE/CLAUDIA_LIVE not set (subscription / network)")
	}
	if _, err := resolveBin(nil); err != nil {
		t.Skipf("codex binary not found: %v", err)
	}
	if err := ensureSubscriptionAuth(nil); err != nil {
		t.Skipf("subscription auth not ready: %v", err)
	}

	workDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	task := NewCodexTask(Config{
		ID:             "codex-pkg-live",
		Name:           "codex-pkg-live",
		WorkDir:        workDir,
		SandboxMode:    "read-only",
		ApprovalPolicy: "never",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	events, err := task.Run(ctx, "Reply with exactly: T14.2-ok")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var sawResult bool
	for ev := range events {
		switch ev.Type {
		case EventResult:
			sawResult = true
			if ev.Content == "" {
				t.Error("empty result content")
			}
		case EventError:
			t.Fatalf("EventError: %v", ev.Error)
		}
	}
	if !sawResult {
		t.Error("never saw EventResult")
	}
	if task.SessionID() == "" {
		t.Error("SessionID empty after live run")
	}
}
