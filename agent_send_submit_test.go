// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/internal/tmuxagent"
)

// 🎯T28 acceptance 4: the send contract holds end to end. tmuxagent
// decides whether the turn began; nothing between that verdict and
// Agent.Send's return may swallow or downgrade it.

// TestSendPropagatesProviderSendFailure closes the generic swallow:
// Agent.Send returns the provider operation's error verbatim rather
// than reporting the keystrokes-left-the-process success that made a
// lost submit invisible above agent.go's Send.
func TestSendPropagatesProviderSendFailure(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("turn not submitted: composer state=typed_unsubmitted after 8 Enter presses")
	ready := make(chan struct{})
	close(ready)
	a := &Agent{
		provider: ProviderClaude,
		alive:    true,
		ready:    ready,
		ops: agentOps{
			send: func(*Agent, string) error { return wantErr },
		},
	}
	err := a.Send("a brief that never became a turn")
	if err == nil {
		t.Fatal("Agent.Send returned nil while the provider reported the turn was never submitted")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Agent.Send error = %v, want it to carry %v", err, wantErr)
	}
}

// TestClaudeSendReportsUnsubmittedTurnThroughRealTmux is the end-to-end
// version, with no fakes below Agent.Send: a real tmux window running
// `cat` — which accepts the paste and never draws Claude Code's turn
// chrome — must make Agent.Send fail. It exercises the whole chain
// (Agent.Send → claudeAgentOps().send → tmuxagent.SendKeys →
// waitContentLanded / ensureSubmitted), so a swallow reintroduced at
// any link turns this red.
func TestClaudeSendReportsUnsubmittedTurnThroughRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	// Isolated server: never touch the live claudia tmux socket. The
	// socket lives in a short temp path of its own — t.TempDir() encodes
	// the test name and blows the 104-byte sun_path limit.
	sockDir, err := os.MkdirTemp("", "t28")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s.sock")
	t.Setenv("CLAUDIA_TMUX_SOCKET", sock)
	if err := tmuxagent.EnsureServer(); err != nil {
		t.Skipf("cannot start an isolated tmux server: %v", err)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sock, "kill-server").Run() })

	windowID, err := tmuxagent.SpawnWindow(t.TempDir(), "t28-no-swallow", "cat", nil)
	if err != nil {
		t.Fatalf("SpawnWindow: %v", err)
	}
	ready := make(chan struct{})
	close(ready)
	a := &Agent{
		provider:     ProviderClaude,
		alive:        true,
		ready:        ready,
		ops:          claudeAgentOps(),
		tmuxWindowID: windowID,
	}

	msg := "🎯T28 payload that will never become a turn.\n" +
		strings.Repeat("padding so the send path takes the bracketed-paste branch.\n", 8)
	if err := a.Send(msg); err == nil {
		t.Fatal("Agent.Send returned nil for a payload that never became a turn")
	} else if !strings.Contains(err.Error(), "turn not submitted") {
		t.Errorf("error should name the unsubmitted turn: %v", err)
	}
}
