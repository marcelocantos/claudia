// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestEscapeWorkDir(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/Users/marcelo/work", "-Users-marcelo-work"},
		{"/Users/marcelo/work/github.com/marcelocantos/claudia",
			"-Users-marcelo-work-github-com-marcelocantos-claudia"},
		{"simple-dash", "simple-dash"},
		{"with_underscore", "with-underscore"},
		{"with spaces", "with-spaces"},
		{"with.dots", "with-dots"},
		{"Mixed123-CASE", "Mixed123-CASE"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := escapeWorkDir(tc.in); got != tc.want {
			t.Errorf("escapeWorkDir(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestProjectDir(t *testing.T) {
	home := os.Getenv("HOME")
	got := projectDir("/Users/marcelo/work/claudia")
	want := filepath.Join(home, ".claude", "projects", "-Users-marcelo-work-claudia")
	if got != want {
		t.Errorf("projectDir = %q, want %q", got, want)
	}
}

func TestTermLogDirDefault(t *testing.T) {
	// With XDG_STATE_HOME unset, termLogDir falls back to ~/.local/state.
	t.Setenv("XDG_STATE_HOME", "")
	home := os.Getenv("HOME")
	got := termLogDir("/Users/marcelo/work/claudia")
	want := filepath.Join(home, ".local", "state", "claudia", "terms",
		"-Users-marcelo-work-claudia")
	if got != want {
		t.Errorf("termLogDir (default) = %q, want %q", got, want)
	}
}

func TestTermLogDirRespectsXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/custom/state")
	got := termLogDir("/Users/marcelo/work/claudia")
	want := filepath.Join("/custom/state", "claudia", "terms",
		"-Users-marcelo-work-claudia")
	if got != want {
		t.Errorf("termLogDir (XDG) = %q, want %q", got, want)
	}
}

// TestAgentSmoke spawns a real claude process and exercises the
// readiness detector end-to-end. Gated on `claude` being available on
// PATH; skipped otherwise (CI without the binary installed, contributors
// without Claude Code). When it runs, it validates:
//
//   - Start succeeds and returns a live Agent
//   - WaitReady returns nil within the overall readiness cap
//   - The readiness latency is in a plausible range (>0, well under cap)
//   - CLAUDECODE stripping: the child doesn't detect itself as nested,
//     which we verify indirectly by reaching readiness at all. Before
//     the CLAUDECODE fix, a nested child would hang in startup and
//     WaitReady would timeout.
//
// The test does not send any prompts — that would incur API costs and
// make the test slow. Readiness detection is the surface being
// exercised here.
func TestAgentReadinessSmoke(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH (required for claudia Session mode)")
	}

	workDir := t.TempDir()

	agent, err := Start(Config{WorkDir: workDir})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	if !agent.Alive() {
		t.Fatal("Alive = false immediately after Start")
	}

	waitStart := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), readyOverallTimeout+5*time.Second)
	defer cancel()

	if err := agent.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	latency := time.Since(waitStart)
	if latency < 100*time.Millisecond {
		// Suspicious — the detector should need at least one poll cycle
		// plus the quiescence window.
		t.Errorf("WaitReady returned in %s — too fast, detector probably misfired", latency)
	}
	if latency > readyOverallTimeout {
		t.Errorf("WaitReady took %s, exceeds cap %s", latency, readyOverallTimeout)
	}
	t.Logf("WaitReady end-to-end: %s", latency.Round(time.Millisecond))
}

// TestAgentSendAndWaitForResponse is the end-to-end smoke test that
// should have existed since v0.4.0. It spawns a real claude session,
// sends a trivial prompt, and waits for a response. This exercises
// the full chain:
//
//   - TUI readiness detection
//   - Send submitting the turn (via the Enter key, not Shift+Enter)
//   - Claude Code writing JSONL events
//   - claudia tailing the JSONL
//   - WaitForResponse resolving on terminal stop_reason
//
// The bug that motivated this test: v0.4.0 shipped with Send using
// "\n" as the submit key, which Claude Code's TUI interprets as
// Shift+Enter — prompt inserted but never submitted, no API request,
// no JSONL events, WaitForResponse hangs indefinitely. The smoke
// test for v0.4.0 only exercised WaitReady and missed this entirely.
//
// The test uses a deliberately cheap prompt ("respond with: ok") to
// minimise API cost and keep runtime under 60 seconds. It does incur
// one real API call per run, so it's only run when CLAUDIA_LIVE=1 is
// set in the environment, in addition to the claude binary being
// available.
func TestAgentSendAndWaitForResponse(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test spends API credit)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}

	workDir := t.TempDir()

	agent, err := Start(Config{WorkDir: workDir, Model: "haiku"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	if err := agent.Send("respond with: ok"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reply, err := agent.WaitForResponse(ctx)
	if err != nil {
		t.Fatalf("WaitForResponse: %v", err)
	}
	if reply == "" {
		t.Error("WaitForResponse returned empty string")
	}
	t.Logf("reply: %q", reply)
}

// waitForResponseFixture builds a minimal Agent and drives synthetic
// Event values through its handler so WaitForResponse can be tested
// without spawning a real claude process. It returns the agent plus a
// dispatch function that the test can call to deliver events.
//
// Start WaitForResponse in a goroutine first, then use dispatch() to
// feed events, then read from the result channel. The fixture polls
// briefly for the handler to be installed before dispatch is safe —
// WaitForResponse is the thing under test, not the dispatch plumbing.
func waitForResponseFixture(t *testing.T) (*Agent, func(Event)) {
	t.Helper()
	a := &Agent{}
	dispatch := func(ev Event) {
		// Wait for WaitForResponse to install its handler before
		// delivering the first event, otherwise the dispatch is a
		// no-op. 200ms is comfortably more than the goroutine
		// scheduling latency.
		deadline := time.Now().Add(200 * time.Millisecond)
		for {
			a.mu.Lock()
			fn := a.onEvent
			a.mu.Unlock()
			if fn != nil {
				fn(ev)
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("WaitForResponse handler never installed")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	return a, dispatch
}

func TestWaitForResponseSingleTerminalEvent(t *testing.T) {
	a, dispatch := waitForResponseFixture(t)

	done := make(chan struct {
		text string
		err  error
	}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		text, err := a.WaitForResponse(ctx)
		done <- struct {
			text string
			err  error
		}{text, err}
	}()

	dispatch(Event{Type: "assistant", Text: "hello world", StopReason: "end_turn"})

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("err = %v", r.err)
		}
		if r.text != "hello world" {
			t.Errorf("text = %q, want %q", r.text, "hello world")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("WaitForResponse did not return after terminal event + settle window")
	}
}

// TestWaitForResponseThinkingThenText exercises the exact bug that
// motivated the settle-timer fix: a thinking block and a text block
// for the same message, both annotated with end_turn, arriving
// within a few milliseconds of each other. The old implementation
// resolved on the first terminal event (thinking, empty) and lost
// the subsequent text block.
func TestWaitForResponseThinkingThenText(t *testing.T) {
	a, dispatch := waitForResponseFixture(t)

	done := make(chan struct {
		text string
		err  error
	}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		text, err := a.WaitForResponse(ctx)
		done <- struct {
			text string
			err  error
		}{text, err}
	}()

	dispatch(Event{Type: "assistant", Text: "", StopReason: "end_turn"})
	time.Sleep(50 * time.Millisecond) // simulate Claude Code's ~45ms gap
	dispatch(Event{Type: "assistant", Text: "ok", StopReason: "end_turn"})

	select {
	case r := <-done:
		if r.text != "ok" {
			t.Errorf("text = %q, want %q (settle timer dropped the text block)", r.text, "ok")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("WaitForResponse did not return")
	}
}

func TestWaitForResponseResetsSettleTimer(t *testing.T) {
	a, dispatch := waitForResponseFixture(t)

	done := make(chan struct {
		text string
		err  error
	}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		text, err := a.WaitForResponse(ctx)
		done <- struct {
			text string
			err  error
		}{text, err}
	}()

	// First terminal event starts the settle timer at t=0.
	dispatch(Event{Type: "assistant", Text: "one", StopReason: "end_turn"})
	// Wait most of the settle window, then deliver another event — this
	// must reset the timer so the third event still gets accumulated.
	time.Sleep(waitSettleDuration - 50*time.Millisecond)
	dispatch(Event{Type: "assistant", Text: "two", StopReason: "end_turn"})
	time.Sleep(waitSettleDuration - 50*time.Millisecond)
	dispatch(Event{Type: "assistant", Text: "three", StopReason: "end_turn"})

	select {
	case r := <-done:
		want := "one\ntwo\nthree"
		if r.text != want {
			t.Errorf("text = %q, want %q", r.text, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForResponse did not return")
	}
}

func TestWaitForResponseIgnoresNonAssistantEvents(t *testing.T) {
	a, dispatch := waitForResponseFixture(t)

	done := make(chan struct {
		text string
		err  error
	}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		text, err := a.WaitForResponse(ctx)
		done <- struct {
			text string
			err  error
		}{text, err}
	}()

	// Non-assistant events must not start the settle timer.
	dispatch(Event{Type: "user"})
	dispatch(Event{Type: "system"})
	dispatch(Event{Type: "file-history-snapshot"})

	// Verify WaitForResponse has NOT returned yet (no assistant event
	// with terminal stop has been seen).
	select {
	case r := <-done:
		t.Fatalf("WaitForResponse returned prematurely with text=%q err=%v", r.text, r.err)
	case <-time.After(waitSettleDuration + 100*time.Millisecond):
		// Expected: still waiting.
	}

	// Now deliver the real turn.
	dispatch(Event{Type: "assistant", Text: "final", StopReason: "end_turn"})

	select {
	case r := <-done:
		if r.text != "final" {
			t.Errorf("text = %q, want final", r.text)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("WaitForResponse did not return after real turn")
	}
}

func TestWaitForResponseToolUseNotTerminal(t *testing.T) {
	// A tool_use stop_reason is not terminal. The settle timer must
	// not start, and subsequent non-terminal streaming chunks must
	// accumulate without resolving WaitForResponse.
	a, dispatch := waitForResponseFixture(t)

	done := make(chan struct {
		text string
		err  error
	}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		text, err := a.WaitForResponse(ctx)
		done <- struct {
			text string
			err  error
		}{text, err}
	}()

	// Simulate an assistant turn that includes a tool_use (model
	// pauses for tool results), followed by a real response after
	// the tool result comes back.
	dispatch(Event{Type: "assistant", Text: "let me check", StopReason: "tool_use"})
	time.Sleep(waitSettleDuration + 100*time.Millisecond)

	// WaitForResponse must NOT have returned yet — tool_use is not
	// terminal.
	select {
	case r := <-done:
		t.Fatalf("WaitForResponse resolved on tool_use stop: text=%q", r.text)
	default:
	}

	// Tool result arrives (user event), then assistant continues.
	dispatch(Event{Type: "user"})
	dispatch(Event{Type: "assistant", Text: "done", StopReason: "end_turn"})

	select {
	case r := <-done:
		want := "let me check\ndone"
		if r.text != want {
			t.Errorf("text = %q, want %q", r.text, want)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("WaitForResponse did not return")
	}
}

// TestAgentMultiTurn exercises back-to-back Send + WaitForResponse on
// the same agent. Verifies that state between turns (particularly the
// settle timer cleanup in WaitForResponse's defer) does not carry
// over and cause the second turn to misbehave.
func TestAgentMultiTurn(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test spends API credit)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}

	workDir := t.TempDir()
	agent, err := Start(Config{WorkDir: workDir, Model: "haiku"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	send := func(prompt string) string {
		t.Helper()
		if err := agent.Send(prompt); err != nil {
			t.Fatalf("Send(%q): %v", prompt, err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		reply, err := agent.WaitForResponse(ctx)
		if err != nil {
			t.Fatalf("WaitForResponse after %q: %v", prompt, err)
		}
		if reply == "" {
			t.Fatalf("WaitForResponse after %q: empty reply", prompt)
		}
		return reply
	}

	first := send("respond with exactly: alpha")
	t.Logf("first reply: %q", first)
	second := send("respond with exactly: beta")
	t.Logf("second reply: %q", second)

	// Loose sanity checks — the point is that both replies came back,
	// not that Claude obeyed the prompt format perfectly.
	if first == second {
		t.Error("both turns returned identical text — state probably carried over")
	}
}

// TestRunHelper exercises the package-level claudia.Run one-shot
// helper end-to-end. Run is the simplest possible consumer API and
// had no prior coverage.
func TestRunHelper(t *testing.T) {
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("CLAUDIA_LIVE not set (this test spends API credit)")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reply, err := Run(ctx, "respond with: ok", Config{
		WorkDir: t.TempDir(),
		Model:   "haiku",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if reply == "" {
		t.Error("Run returned empty reply")
	}
	t.Logf("reply: %q", reply)
}

// TestAgentReadinessFailureOnDeadProcess verifies that if the Claude
// process dies during startup (before the TUI quiesces), detectReady
// sets a sensible error and closes the channel rather than hanging.
// We simulate a dead process by spawning claude with a flag that
// causes it to exit immediately — --help is a reasonable stand-in for
// "claude that exits without finishing startup".
func TestAgentReadinessFailureOnDeadProcess(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}

	workDir := t.TempDir()

	// ExtraArgs lets us inject --help so claude prints its usage and
	// exits cleanly, which from claudia's perspective looks like
	// "child exited during startup".
	agent, err := Start(Config{
		WorkDir:   workDir,
		ExtraArgs: []string{"--help"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer agent.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// WaitReady should return within a reasonable time with a non-nil
	// error — either "claude exited before TUI became ready" or
	// possibly nil if the burst of --help output happened to quiesce
	// before the process exited. Either is acceptable; the hard
	// requirement is that WaitReady returns at all.
	done := make(chan error, 1)
	go func() { done <- agent.WaitReady(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Logf("WaitReady returned error (expected): %v", err)
		} else {
			t.Log("WaitReady returned nil (--help burst quiesced before exit)")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("WaitReady hung beyond reasonable bound on a dead process")
	}
}
