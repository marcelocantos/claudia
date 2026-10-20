// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/omp"
)

// probeLoaded asks the real sidecar whether seat is loaded under session, by
// adopting it on a throwaway connection: "ready" means it is, an error that
// it is not (or not under that session).
func probeLoaded(t *testing.T, socket, seat, session string) omp.Event {
	t.Helper()
	c, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(c).Encode(omp.Message{Op: omp.OpAdopt, Seat: seat, SessionID: session, Provider: omp.Anthropic, Token: "fixture"}); err != nil {
		t.Fatal(err)
	}
	var ev omp.Event
	if err := json.NewDecoder(c).Decode(&ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

// jevons 🎯T1096, on the real sidecar, directly and through the daemon: a removed
// seat (jevons_agent_kill, the finished-work reap) is unloaded, so its name
// starts again on another provider under a fresh session; a stopped (parked)
// seat stays loaded for its own session to adopt; and a fresh start whose
// name is still loaded under another session — a removal that never reached
// the sidecar — replaces the stale seat instead of failing with
// "load seat identity mismatch".
func TestT1096SeatNameReuseOnTheActualSidecar(t *testing.T) {
	_, root, socket := bootOnActualSidecar(t)
	ctx := t.Context()
	for _, direct := range []bool{true, false} {
		t.Run(fmt.Sprintf("direct=%v", direct), func(t *testing.T) {
			reg, err := claudia.NewRegistry(filepath.Join(root, fmt.Sprintf("t1096-%v.json", direct)))
			if err != nil {
				t.Fatal(err)
			}
			reg.SetDirect(direct)
			register := func(name, session string, p claudia.Provider) {
				t.Helper()
				if err := reg.Register(claudia.AgentDef{Name: name, SessionID: session, Provider: p, WorkDir: root, TermLogPath: "-"}); err != nil {
					t.Fatal(err)
				}
			}

			// A removal frees the name.
			killed := fmt.Sprintf("jv-t1104-auto-%v", direct)
			register(killed, "killed", "anthropic")
			if _, err := reg.LaunchContext(ctx, killed); err != nil {
				t.Fatal("first launch:", err)
			}
			if err := reg.Remove(killed); err != nil {
				t.Fatal(err)
			}
			if ev := probeLoaded(t, socket, killed, "killed"); ev.Type == "ready" {
				t.Fatalf("removed seat is still loaded in the sidecar: %+v", ev)
			}
			register(killed, "remint", "openai-codex")
			again, err := reg.LaunchContext(ctx, killed)
			if err != nil {
				t.Fatal("re-start of a removed name:", err)
			}
			if again.Provider() != "openai-codex" || again.SessionID() != "remint" {
				t.Fatalf("re-start = %s on %q, want openai-codex on remint", again.Provider(), again.SessionID())
			}
			if err := reg.Remove(killed); err != nil {
				t.Fatal(err)
			}

			// A plain stop keeps the sidecar seat; a fresh registration of the
			// name then replaces it.
			parked := fmt.Sprintf("jv-parked-%v", direct)
			register(parked, "parked", "anthropic")
			if _, err := reg.LaunchContext(ctx, parked); err != nil {
				t.Fatal("parked launch:", err)
			}
			reg.Stop(parked)
			if ev := probeLoaded(t, socket, parked, "parked"); ev.Type != "ready" {
				t.Fatalf("stopped seat was unloaded: %+v", ev)
			}
			// No handle: this removal cannot reach the sidecar, so the seat
			// stays loaded under "parked" — the stale seat of the live case.
			if err := reg.Remove(parked); err != nil {
				t.Fatal(err)
			}
			register(parked, "fresh", "openai-codex")
			fresh, err := reg.LaunchContext(ctx, parked)
			if err != nil {
				t.Fatal("fresh start under a stale loaded name:", err)
			}
			if fresh.Provider() != "openai-codex" || fresh.SessionID() != "fresh" {
				t.Fatalf("fresh start = %s on %q, want openai-codex on fresh", fresh.Provider(), fresh.SessionID())
			}
			if err := reg.Remove(parked); err != nil {
				t.Fatal(err)
			}
		})
	}
}
