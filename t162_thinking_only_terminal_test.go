// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 🎯T162: a Claude resume turn wrote a thinking-only record and then the
// text record, both marked stop_reason end_turn (jevons T919, e5fd6206). The
// first was read as the turn's end, so a message queued behind the turn was
// drained into the middle of it.

const (
	t162Prompt   = `{"type":"user","sessionId":"t162","uuid":"turn-1","message":{"role":"user","content":"resume"}}`
	t162Thinking = `{"type":"assistant","sessionId":"t162","uuid":"rec-thinking","message":{"id":"msg-1","role":"assistant","stop_reason":"end_turn","content":[{"type":"thinking","thinking":"nothing to add","signature":"sig"}]}}`
	t162Text     = `{"type":"assistant","sessionId":"t162","uuid":"rec-text","message":{"id":"msg-1","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"[silent]"}]}}`
)

func TestT162ThinkingOnlyRecordIsNotATerminalStop(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want bool
	}{
		{"thinking only, end_turn", t162Thinking, false},
		{"redacted thinking only, end_turn", `{"type":"assistant","message":{"id":"m","stop_reason":"end_turn","content":[{"type":"redacted_thinking","data":"x"}]}}`, false},
		{"thinking only, max_tokens", `{"type":"assistant","message":{"id":"m","stop_reason":"max_tokens","content":[{"type":"thinking","thinking":"x"}]}}`, false},
		{"text record, end_turn", t162Text, true},
		{"thinking and text in one record", `{"type":"assistant","message":{"id":"m","stop_reason":"end_turn","content":[{"type":"thinking","thinking":"x"},{"type":"text","text":"ok"}]}}`, true},
		// A record with no blocks has no sibling left to complete it: it
		// keeps its terminal stop rather than leave a finished turn open.
		{"empty content, end_turn", `{"type":"assistant","message":{"id":"m","stop_reason":"end_turn","content":[]}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := parseEvent(tc.line)
			if got := ev.IsTerminalStop(); got != tc.want {
				t.Fatalf("IsTerminalStop = %v, want %v (StopReason %q)", got, tc.want, ev.StopReason)
			}
			// The broker relays the decoded Event, not the line: the verdict
			// must survive the wire, where Raw may even be elided.
			raw, err := EncodeEventWire(ev)
			if err != nil {
				t.Fatal(err)
			}
			relayed, err := DecodeEventWire(raw)
			if err != nil {
				t.Fatal(err)
			}
			relayed.Raw = nil
			if got := relayed.IsTerminalStop(); got != tc.want {
				t.Fatalf("relayed IsTerminalStop = %v, want %v", got, tc.want)
			}
		})
	}
	// An escalation waiting behind the turn is settled by its end, not by
	// the thinking record ahead of it.
	if absorbs(parseEvent(t162Thinking), "queued") {
		t.Fatal("a thinking-only record settled an escalation as if the turn had ended")
	}
}

// TestT162QueuedMessageWaitsForTheTextRecord drives the transcript through
// the Claude tailer, into a host that drains its queue on the first terminal
// stop — the shape jevons runs.
func TestT162QueuedMessageWaitsForTheTextRecord(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))
	workDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	sessionID := "t162-thinking-only"

	backend := &fakeAgentBackend{name: "fake-claude", tailJSONL: true}
	agent, err := startWithBackend(Config{WorkDir: workDir, SessionID: sessionID, TermLogPath: "-"}, backend)
	if err != nil {
		t.Fatalf("startWithBackend: %v", err)
	}
	defer agent.Stop()
	if err := agent.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	var (
		mu        sync.Mutex
		seen      []string // RecordIDs, in arrival order
		terminals []string // RecordIDs of terminal stops
		drainedAt string   // RecordID the queued message was drained on
	)
	agent.SubscribeEvents(func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, ev.RecordID)
		if !ev.IsTerminalStop() {
			return
		}
		terminals = append(terminals, ev.RecordID)
		if drainedAt == "" {
			drainedAt = ev.RecordID
		}
	})
	sawRecord := func(id string) func() bool {
		return func() bool {
			mu.Lock()
			defer mu.Unlock()
			for _, s := range seen {
				if s == id {
					return true
				}
			}
			return false
		}
	}

	jsonlPath := SessionJSONLPath(sessionID, workDir)
	if err := os.MkdirAll(filepath.Dir(jsonlPath), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(jsonlPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	write := func(line string) {
		t.Helper()
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}

	reply := make(chan string, 1)
	go func() {
		text, err := agent.WaitForResponse(t.Context())
		if err != nil {
			t.Errorf("WaitForResponse: %v", err)
		}
		reply <- text
	}()
	waitForEventSubscribers(t, agent, 2)

	write(t162Prompt)
	write(t162Thinking)
	waitForCond(t, "the thinking record published", sawRecord("rec-thinking"))

	// Longer than WaitForResponse's settle, so a reply armed by the
	// thinking record would have been emitted already.
	time.Sleep(4 * waitSettleDuration)
	mu.Lock()
	early := drainedAt
	mu.Unlock()
	if early != "" {
		t.Fatalf("queued message drained on %s, between the thinking record and the text record", early)
	}
	select {
	case got := <-reply:
		t.Fatalf("WaitForResponse returned %q on the thinking record, before the turn's text", got)
	default:
	}

	write(t162Text)
	waitForCond(t, "the text record published", sawRecord("rec-text"))
	if got := <-reply; got != "[silent]" {
		t.Errorf("reply = %q, want [silent]", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(terminals) != 1 || terminals[0] != "rec-text" {
		t.Fatalf("terminal stops = %v, want exactly one, on rec-text", terminals)
	}
	if drainedAt != "rec-text" {
		t.Fatalf("queued message drained on %q, want rec-text", drainedAt)
	}
}
