// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Jevons 🎯T861: a seat reminted after session/load refusal must be told its
// history is gone and named the dead session id — not that the transcript was
// restored or that it should continue prior work.
func TestT861RemintedSeatGetsLostHistoryNudgeWithDeadSession(t *testing.T) {
	f := newSeatFixture(t)
	old := "1207c272-4a2d-4ffd-bd4b-27a606a9d7db"
	launch := registryStart
	registryStart = func(ctx context.Context, cfg Config) (*Agent, error) {
		if cfg.RequireResume {
			return nil, fmt.Errorf("acp session/load %s: Invalid params (%w)", cfg.SessionID, ErrCursorResumeDenied)
		}
		return launch(ctx, cfg)
	}
	t.Cleanup(func() { registryStart = launch })
	f.open(t, []AgentDef{{
		Name: "jv-compact-1207c272", WorkDir: t.TempDir(), SessionID: old,
		AutoStart: true, Materialized: true, Provider: ProviderCursor,
	}})
	now := time.Date(2026, 9, 23, 5, 46, 31, 0, time.UTC)
	out := f.reg.ResumeAll(context.Background(), &ResumeArgs{Now: now})
	if len(out) != 1 || out[0].Err != nil || out[0].How != ResumeReminted || out[0].OldSessionID != old {
		t.Fatalf("outcome = %+v, want reminted off %s", out, old)
	}
	if !out[0].Nudged {
		t.Fatal("reminted seat was not nudged")
	}
	want := lostHistoryRestartNudge(now, old)
	got := strings.Join(f.sends("jv-compact-1207c272"), "|")
	if got != want {
		t.Fatalf("nudge = %q, want lost-history with dead session %q", got, old)
	}
	if strings.Contains(got, "resumed from its saved transcript") || strings.Contains(got, "continue the task you were working on") {
		t.Fatalf("nudge falsely claims continuity: %q", got)
	}
}

// Control: a relaunch that kept history still gets the continuity wording.
func TestT861ResumedSeatWithHistoryKeepsContinuityNudge(t *testing.T) {
	now := time.Date(2026, 9, 23, 5, 46, 31, 0, time.UTC)
	f := newSeatFixture(t)
	f.open(t, []AgentDef{{Name: "seat", WorkDir: t.TempDir(), SessionID: "sid", AutoStart: true}})
	out := f.reg.ResumeAll(context.Background(), &ResumeArgs{Now: now})
	if len(out) != 1 || out[0].Err != nil || !out[0].Nudged {
		t.Fatalf("outcomes = %+v", out)
	}
	want := fmt.Sprintf(DefaultRestartNudge, now.Format(time.RFC3339))
	if got := strings.Join(f.sends("seat"), "|"); got != want {
		t.Fatalf("nudge = %q, want continuity restart nudge", got)
	}
}

// Remint is visible on the seat-event stream for overseer/registry consumers.
func TestT861RemintPublishedOnSeatEventStream(t *testing.T) {
	f := newSeatFixture(t)
	old := "b54f134f-f7ef-4780-a077-37132cd64d14"
	launch := registryStart
	registryStart = func(ctx context.Context, cfg Config) (*Agent, error) {
		if cfg.RequireResume {
			return nil, fmt.Errorf("acp session/load %s: Invalid params (%w)", cfg.SessionID, ErrCursorResumeDenied)
		}
		return launch(ctx, cfg)
	}
	t.Cleanup(func() { registryStart = launch })
	f.open(t, []AgentDef{{
		Name: "seat", WorkDir: t.TempDir(), SessionID: old,
		AutoStart: true, Materialized: true, Provider: ProviderCursor,
	}})
	var mu sync.Mutex
	var resumed []SeatEvent
	id := f.reg.SubscribeSeatEvents(func(ev SeatEvent) {
		if ev.Kind == SeatResumed {
			mu.Lock()
			resumed = append(resumed, ev)
			mu.Unlock()
		}
	})
	t.Cleanup(func() { f.reg.UnsubscribeSeatEvents(id) })
	out := f.reg.ResumeAll(context.Background(), &ResumeArgs{Nudge: NoRestartNudge})
	if len(out) != 1 || out[0].How != ResumeReminted {
		t.Fatalf("outcome = %+v", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(resumed) != 1 || resumed[0].How != ResumeReminted || resumed[0].OldSessionID != old {
		t.Fatalf("seat resumed events = %+v, want one reminted with old_session %s", resumed, old)
	}
}
