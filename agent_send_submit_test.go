// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"os"
	"path/filepath"
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
	marker := filepath.Join(t.TempDir(), "ran")
	bin := filepath.Join(t.TempDir(), "cat")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("/tmp", "t28")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s.sock")
	t.Setenv("CLAUDIA_TMUX_SOCKET", sock)

	id, err := tmuxagent.SpawnWindow(t.TempDir(), "t28-no-swallow", bin, nil)
	if id != "" || !errors.Is(err, tmuxagent.ErrLegacyCLISeatTransport) {
		t.Fatalf("SpawnWindow = (%q, %v), want legacy refusal", id, err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("SpawnWindow executed the command: %v", statErr)
	}
	if _, statErr := os.Stat(sock); !os.IsNotExist(statErr) {
		t.Fatalf("SpawnWindow created a tmux socket: %v", statErr)
	}
}
