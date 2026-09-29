// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

// t151Load starts a seat against a fresh fake sidecar and returns its load.
func t151Load(t *testing.T, sessionID string) (*Agent, omp.Message) {
	t.Helper()
	var refreshes atomic.Int32
	s := startT141Sidecar(t)
	t141Plan(t, "live", &refreshes, "")
	agent, err := StartDirect(Config{
		Name: "po", Provider: Provider(omp.Anthropic), Model: "claude-sonnet",
		WorkDir: t.TempDir(), TermLogPath: "-", SessionID: sessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Stop() })
	if err := agent.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	return agent, s.nextLoad()
}

// 🎯T151: the sidecar keeps a seat's conversation on disk under the seat's
// session, so the host names that session on every load: the same session
// after a sidecar restart resumes, and a new one starts fresh.
func TestT151LoadNamesTheSeatsSession(t *testing.T) {
	if _, load := t151Load(t, "s-kept"); load.SessionID != "s-kept" {
		t.Fatalf("load session %q, want the seat's own s-kept", load.SessionID)
	}
}

// 🎯T151: a seat with no session yet is loaded under the one the host is
// about to record for it, not under an empty name.
func TestT151LoadNamesAMintedSession(t *testing.T) {
	agent, load := t151Load(t, "")
	if load.SessionID == "" || load.SessionID != agent.SessionID() {
		t.Fatalf("load session %q, want the minted session %q the host records", load.SessionID, agent.SessionID())
	}
}
