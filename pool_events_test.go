// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// assistantLine is one transcript record that WaitForResponse would treat
// as a complete turn.
func assistantLine(text string) string {
	return `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` +
		text + `"}],"stop_reason":"end_turn"}}` + "\n"
}

// TestClassifyPoolWindow pins the sweep rules 🎯T78 depends on: a window
// with no recorded session id is unobservable and must never be adopted,
// while a held one is left to its holder even so.
func TestClassifyPoolWindow(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	past := "999999"
	future := "1000001"

	cases := []struct {
		name  string
		state poolWindowState
		want  poolDisposition
	}{
		{"warm and observable", poolWindowState{sessionID: "sid"}, poolIdle},
		{"held", poolWindowState{held: true, sessionID: "sid"}, poolHeld},
		{"no session id", poolWindowState{}, poolBlind},
		{"blank session id", poolWindowState{sessionID: "  "}, poolBlind},
		{"held without a session id is still its holder's",
			poolWindowState{held: true}, poolHeld},
		{"deadline passed",
			poolWindowState{sessionID: "sid", deadline: past, hasDeadline: true}, poolExpired},
		{"deadline passed while held",
			poolWindowState{held: true, sessionID: "sid", deadline: past, hasDeadline: true}, poolExpired},
		{"deadline in the future",
			poolWindowState{sessionID: "sid", deadline: future, hasDeadline: true}, poolIdle},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyPoolWindow(now, c.state); got != c.want {
				t.Errorf("classifyPoolWindow = %v, want %v", got, c.want)
			}
		})
	}
}

// TestAdoptWindowRefusesUnrecordedSession is the guard behind the sweep:
// even if a blind window reached adoption, it is refused rather than
// handed over as a seat that can never answer.
func TestAdoptWindowRefusesUnrecordedSession(t *testing.T) {
	_, err := adoptWindow(context.Background(), Config{}, t.TempDir(), "@7", "")
	if err == nil {
		t.Fatal("adoptWindow accepted a window with no recorded session id")
	}
	if !strings.Contains(err.Error(), poolSessionOption) {
		t.Errorf("error %q does not name @%s", err, poolSessionOption)
	}
}

// TestPoolTailOffset checks the measurement that decides where a holder's
// event stream begins.
func TestPoolTailOffset(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "never-written.jsonl")
	if got := poolTailOffset(missing); got != 0 {
		t.Errorf("offset for an absent transcript = %d, want 0", got)
	}

	path := filepath.Join(dir, "session.jsonl")
	body := assistantLine("previous holder")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := poolTailOffset(path), int64(len(body)); got != want {
		t.Errorf("offset = %d, want %d", got, want)
	}
}

// collectEvents subscribes to agent and returns a snapshot function.
func collectEvents(a *Agent) func() []Event {
	var (
		mu   sync.Mutex
		evs  []Event
		take = func() []Event {
			mu.Lock()
			defer mu.Unlock()
			return append([]Event(nil), evs...)
		}
	)
	a.SubscribeEvents(func(ev Event) {
		mu.Lock()
		evs = append(evs, ev)
		mu.Unlock()
	})
	return take
}

// tailFixture is a bare Agent wired to a transcript on disk — the part of
// a pooled seat that publishes Events, without a tmux server.
func tailFixture(t *testing.T, jsonlPath string) *Agent {
	t.Helper()
	a := &Agent{
		provider:  ProviderClaude,
		sessionID: "pool-fixture",
		jsonlPath: jsonlPath,
		alive:     true,
		ready:     make(chan struct{}),
		eventSubs: make(map[int64]EventFunc),
	}
	close(a.ready)
	t.Cleanup(func() {
		a.mu.Lock()
		a.alive = false
		a.mu.Unlock()
	})
	return a
}

// waitForEvent polls until want appears in the collected assistant text,
// and reports what did arrive when it never does.
func waitForEvent(t *testing.T, take func() []Event, want string) []Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs := take()
		for _, ev := range evs {
			if ev.Text == want {
				return evs
			}
		}
		if time.Now().After(deadline) {
			var got []string
			for _, ev := range evs {
				got = append(got, ev.Text)
			}
			t.Fatalf("event %q never arrived; saw %v", want, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTailJSONLFromSkipsPriorHolder is the 🎯T78 no-replay oracle: a
// second holder of the same transcript is told where its own stream
// begins, and never sees the turn the first holder had.
func TestTailJSONLFromSkipsPriorHolder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	first := assistantLine("first holder answer")
	if err := os.WriteFile(path, []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}

	offset := poolTailOffset(path)
	a := tailFixture(t, path)
	take := collectEvents(a)
	go a.tailJSONLFrom(offset)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(assistantLine("second holder answer")); err != nil {
		t.Fatal(err)
	}

	for _, ev := range waitForEvent(t, take, "second holder answer") {
		if ev.Text == "first holder answer" {
			t.Fatal("the previous holder's turn was replayed to the new holder")
		}
	}
}

// TestTailJSONLFromZeroReadsWholeTranscript is the other half: a freshly
// spawned pool window has nothing to skip, so nothing is skipped.
func TestTailJSONLFromZeroReadsWholeTranscript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	a := tailFixture(t, path)
	take := collectEvents(a)
	go a.tailJSONLFrom(0)

	// The file does not exist yet, exactly as it does not for a pool
	// window that has never been sent a prompt.
	if err := os.WriteFile(path, []byte(assistantLine("only turn")), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, take, "only turn")
}

// TestTailStartOffset covers a transcript rewritten under the tailer:
// the offset it measured no longer names the content it measured, so it
// resumes at the new end rather than replaying a conversation it never
// saw begin.
func TestTailStartOffset(t *testing.T) {
	cases := []struct{ size, want, expect int64 }{
		{size: 100, want: 40, expect: 40},   // ordinary resume
		{size: 100, want: 100, expect: 100}, // at the end
		{size: 40, want: 100, expect: 40},   // rewritten shorter
		{size: 0, want: 100, expect: 0},     // truncated to nothing
	}
	for _, c := range cases {
		if got := tailStartOffset(c.size, c.want); got != c.expect {
			t.Errorf("tailStartOffset(%d, %d) = %d, want %d", c.size, c.want, got, c.expect)
		}
	}
}

// TestStopPoolObserversRetiresTheTailer checks that a returned window
// stops publishing into the handle that gave it back — otherwise the next
// holder's turn arrives on the previous holder's subscribers.
func TestStopPoolObserversRetiresTheTailer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	a := tailFixture(t, path)
	take := collectEvents(a)
	go a.tailJSONLFrom(0)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(assistantLine("while held")); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, take, "while held")

	a.stopPoolObservers()
	// The tailer notices its generation changed on its next poll; give it
	// more than one poll interval before writing the next holder's turn.
	time.Sleep(500 * time.Millisecond)
	if _, err := f.WriteString(assistantLine("next holder")); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)
	for _, ev := range take() {
		if ev.Text == "next holder" {
			t.Fatal("a returned handle kept publishing the window's later turns")
		}
	}
}
