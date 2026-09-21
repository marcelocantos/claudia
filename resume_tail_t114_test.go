// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestResumedStartDoesNotRepublishTheResumedConversation is the 🎯T114
// oracle. A seat relaunched on an existing transcript — a rewind, a
// crash restart, a daemon reclaim — used to tail that transcript from
// byte zero, so every surviving turn was published again as if the new
// process had just said it. daemon.TestRewindLive saw the result: the
// surviving turn's "ok" answered the owner's next question.
//
// The verdict is Usage, not which reply WaitForResponse happens to catch.
// Once the tail has published the new line it has read every line before
// it, so a replay of the resumed turn is always in the totals, however
// the goroutines were scheduled.
func TestResumedStartDoesNotRepublishTheResumedConversation(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpHome, "state"))

	workDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	sessionID := "t114-resumed-session"
	jsonlPath := SessionJSONLPath(sessionID, workDir)
	if err := os.MkdirAll(filepath.Dir(jsonlPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// The conversation being resumed: the turn that survived a rewind.
	surviving := `{"type":"user","message":{"role":"user","content":"Remember this codeword: ALPHA. Reply with only: ok"}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"},"usage":{"input_tokens":100,"output_tokens":200}}` + "\n"
	if err := os.WriteFile(jsonlPath, []byte(surviving), 0o644); err != nil {
		t.Fatal(err)
	}

	backend := &fakeAgentBackend{name: "fake-claude", tailJSONL: true}
	agent, err := startWithBackend(Config{WorkDir: workDir, SessionID: sessionID, TermLogPath: "-"}, backend)
	if err != nil {
		t.Fatalf("startWithBackend: %v", err)
	}
	defer agent.Stop()
	if req := backend.request(t); !req.Resuming {
		t.Fatal("the fixture must be a resume: the transcript already exists")
	}
	if err := agent.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	done := make(chan string, 1)
	go func() {
		text, err := agent.WaitForResponse(t.Context())
		if err != nil {
			t.Errorf("WaitForResponse: %v", err)
		}
		done <- text
	}()
	waitForEventSubscribers(t, agent, 1)

	// The relaunched seat's own answer to the owner's next question.
	f, err := os.OpenFile(jsonlPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ALPHA"}],"stop_reason":"end_turn"},"usage":{"input_tokens":1,"output_tokens":2}}` + "\n"); err != nil {
		t.Fatal(err)
	}

	if got := <-done; got != "ALPHA" {
		t.Errorf("reply = %q, want ALPHA: the resumed conversation answered the new question", got)
	}
	if u := agent.Usage(); u.InputTokens != 1 || u.OutputTokens != 2 {
		t.Fatalf("usage = %d in / %d out, want 1 / 2: the resumed conversation was published again as this seat's own turns",
			u.InputTokens, u.OutputTokens)
	}
}
