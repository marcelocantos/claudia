// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"sync/atomic"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

// Jevons 🎯T887: a sidecar seat that takes a prompt and thinks for a minute
// before its first text must still show, at once, that the prompt landed —
// otherwise a host's delivery check (45 s) reports a working seat as
// not_submitted. The sidecar says "accepted"; the seat publishes it as
// prompt_accepted before any text exists.
func TestT887AcceptedPromptIsVisibleBeforeAnyText(t *testing.T) {
	var refreshes atomic.Int32
	s := startT141Sidecar(t)
	t141Plan(t, "live", &refreshes, "")
	agent, conn := startT141Seat(t, s)

	events := make(chan Event, 16)
	tok := agent.SubscribeEvents(func(ev Event) { events <- ev })
	defer agent.UnsubscribeEvents(tok)

	if err := agent.Send("a slow question"); err != nil {
		t.Fatal(err)
	}
	for m := range s.got {
		if m.Op == omp.OpPrompt && m.Text == "a slow question" {
			break
		}
	}
	// The sidecar takes the turn; its first text is still far off.
	t141Write(t, conn, `{"type":"accepted"}`)
	for ev := range events {
		if ev.Type == "progress" && ev.ProgressType == ProgressPromptAccepted {
			break // blocks until seen; `go test -timeout` is the clock
		}
		if ev.Type == "assistant" && ev.Text != "" {
			t.Fatalf("text arrived before the acceptance: %+v", ev)
		}
	}
	if !agent.PromptInFlight() {
		t.Fatal("an accepted prompt must read as in flight")
	}
}
