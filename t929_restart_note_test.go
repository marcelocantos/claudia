// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// Jevons 🎯T929: an Oh My Pi seat relaunched without its stored conversation
// knows it started empty, from the sidecar's own word.
func TestT929SidecarReadySaysWhetherHistoryCameBack(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name        string
		ev          omp.Event
		summaryOnly bool
		lost        bool
	}{
		{"restored", omp.Event{Type: "ready", How: "launched", Restored: &yes}, false, false},
		{"nothing stored", omp.Event{Type: "ready", How: "launched", Restored: &no}, false, true},
		{"sidecar predates the store", omp.Event{Type: "ready", How: "launched"}, false, true},
		{"adopted: never left", omp.Event{Type: "ready", How: "adopted"}, false, false},
		{"summary-only: no conversation to lose", omp.Event{Type: "ready", How: "launched"}, true, false},
	} {
		if got := ompHistoryLost(tc.ev, tc.summaryOnly); got != tc.lost {
			t.Errorf("%s: lost = %v, want %v", tc.name, got, tc.lost)
		}
	}
}

// Jevons 🎯T929: a relaunched seat that came back without its conversation
// is told so, not that it "was resumed from its saved transcript" — the
// claim that sent seats back to work they no longer knew about.
func TestT929RelaunchedSeatWithoutHistoryIsToldSo(t *testing.T) {
	now := time.Date(2026, 9, 30, 1, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, nudge string
		lost        bool
		want        []string
	}{
		{"history kept", "", false, []string{fmt.Sprintf(DefaultRestartNudge, now.Format(time.RFC3339))}},
		{"history lost", "", true, []string{fmt.Sprintf(LostHistoryRestartNudge, now.Format(time.RFC3339))}},
		{"history lost, custom note", "custom note", true, []string{fmt.Sprintf(LostHistoryRestartNudge, now.Format(time.RFC3339))}},
		{"history lost, notes disabled", NoRestartNudge, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSeatFixture(t)
			launch := registryStart
			registryStart = func(ctx context.Context, cfg Config) (*Agent, error) {
				a, err := launch(ctx, cfg)
				if a != nil {
					a.historyLost = tc.lost
				}
				return a, err
			}
			t.Cleanup(func() { registryStart = launch })
			f.open(t, []AgentDef{{Name: "seat", WorkDir: t.TempDir(), SessionID: "sid", AutoStart: true}})
			out := f.reg.ResumeAll(context.Background(), &ResumeArgs{Nudge: tc.nudge, Now: now})
			if len(out) != 1 || out[0].Err != nil {
				t.Fatalf("outcomes = %+v", out)
			}
			if got := f.sends("seat"); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("sends = %q, want %q", got, tc.want)
			}
		})
	}
}
