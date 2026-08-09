// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"testing"
	"time"
)

// These oracles pin the "no silent model failure" contract: the resolved model
// is observable in both modes, and a model the CLI can't serve surfaces loudly.

func TestParseEventCapturesResolvedModel(t *testing.T) {
	// Session mode tails the transcript; the resolved model rides the
	// assistant event at message.model (the full id even for an alias request).
	line := `{"type":"assistant","message":{"model":"claude-opus-5","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}]}}`
	ev := parseEvent(line)
	if ev.Model != "claude-opus-5" {
		t.Fatalf("Event.Model = %q, want claude-opus-5", ev.Model)
	}
	// Non-assistant events carry no model.
	if m := parseEvent(`{"type":"system","subtype":"turn_duration"}`).Model; m != "" {
		t.Fatalf("non-assistant Event.Model = %q, want empty", m)
	}
}

func TestParseTaskSystemCapturesResolvedModel(t *testing.T) {
	// Task mode parses the stdout stream; the init event carries the model.
	evs := ParseTaskLine([]byte(`{"type":"system","subtype":"init","session_id":"abc","model":"claude-fable-5"}`))
	if len(evs) != 1 || evs[0].Type != TaskEventInit {
		t.Fatalf("want one init event, got %+v", evs)
	}
	if evs[0].Model != "claude-fable-5" {
		t.Fatalf("TaskEvent.Model = %q, want claude-fable-5", evs[0].Model)
	}
}

func TestParseTaskResultErrorIsLoud(t *testing.T) {
	// A model the CLI cannot serve must never be silent: a non-success result
	// becomes a TaskEventError with a non-empty message.
	evs := ParseTaskLine([]byte(`{"type":"result","subtype":"error","errors":[{"message":"model not found: bogus"}]}`))
	if len(evs) != 1 || evs[0].Type != TaskEventError || !evs[0].IsError {
		t.Fatalf("want a loud TaskEventError, got %+v", evs)
	}
	if evs[0].ErrorMsg == "" {
		t.Fatal("error must name the failure, got empty ErrorMsg")
	}
}

// TestModelObservableLive is the end-to-end contract: request the alias "fable"
// and confirm Task.Model() reports the resolved full id — catching exactly the
// silent-fallback failure that motivated this (asking for one model, getting
// another with no signal). Gated on CLAUDIA_LIVE (spends API credit).
func TestModelObservableLive(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test spends API credit)")
	}
	if _, err := resolveClaudeBin(); err != nil {
		t.Skip("claude binary not found")
	}
	task := NewTask(TaskConfig{ID: "model-obs", WorkDir: t.TempDir(), Model: "fable"})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	ch, err := task.Run(ctx, "reply with only: ok")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for range ch { // drain
	}
	if got := task.Model(); got != "claude-fable-5" {
		t.Fatalf("Task.Model() = %q, want claude-fable-5 — alias 'fable' must resolve, not silently fall back", got)
	}
}
