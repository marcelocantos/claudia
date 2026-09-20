// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"os"
	"sort"
	"testing"
	"time"
)

// TestT92MeasureFakePeerTurnaround measures how long the python fake ACP
// peer takes to say its first word after a session/prompt is written.
//
// It is a measurement probe: it asserts nothing, and it is skipped unless
// CLAUDIA_T92_MEASURE is set, so it costs the gate nothing. The precedent
// is TestT96MeasureSilence* — a constant that is questioned gets measured
// again rather than argued about.
//
// Why it exists. cursorHermeticPeerBound applies the product's 6.5x margin
// to the fake's worst observed turnaround, and that worst observation was
// ONE sample: 402ms, at load average 122, in 🎯T92's own failing run. One
// sample is exactly the evidence this target family exists to distrust. Run
// this on the busiest host you can find — a hostile machine makes the
// measurement better, not worse, because the number wanted is the worst
// case and nothing here can be corrupted by slowness. A probe that only
// records cannot be made to lie by a slow scheduler; it can only record a
// larger number, which is the number being asked for.
//
// What it times: Start (so the peer is already warm, as it is in every test
// that shortens the bound), then Send with the silence watch armed and its
// bound set far out of reach. The 🎯T83 watch holds Send open until the
// peer says ANYTHING, so Send's own duration is the turnaround the bound
// has to cover.
//
// No livegate directive: CLAUDIA_T92_MEASURE is not a live-gate name
// (LiveGateName requires a LIVE word), so the census never classifies this
// as a live test and has nothing to be told.
func TestT92MeasureFakePeerTurnaround(t *testing.T) {
	if os.Getenv("CLAUDIA_T92_MEASURE") == "" {
		t.Skip("CLAUDIA_T92_MEASURE not set (measurement probe; asserts nothing)")
	}
	// Far out of reach: this probe must never convict the peer, only time it.
	shortenCursorSilenceBound(t, 5*time.Minute)
	bin := writeFakeCursorACP(t)
	t.Setenv("CURSOR_BIN", bin)

	const mints = 10
	samples := make([]time.Duration, 0, mints)
	for i := range mints {
		func() {
			agent, err := Start(Config{Provider: ProviderCursor, WorkDir: t.TempDir(), TermLogPath: "-"})
			if err != nil {
				t.Fatalf("mint %d: Start: %v", i, err)
			}
			defer agent.Stop()
			start := time.Now()
			if err := agent.Send("Reply with exactly: pong"); err != nil {
				t.Fatalf("mint %d: Send: %v", i, err)
			}
			samples = append(samples, time.Since(start))
		}()
	}
	sort.Slice(samples, func(a, b int) bool { return samples[a] < samples[b] })
	worst := samples[len(samples)-1]
	var lines string
	for _, d := range samples {
		lines += fmt.Sprintf(" %v", d.Round(time.Millisecond))
	}
	t.Logf("fake peer turnaround over %d mints (sorted):%s", mints, lines)
	t.Logf("worst=%v  median=%v", worst.Round(time.Millisecond), samples[len(samples)/2].Round(time.Millisecond))
	t.Logf("6.5x worst = %v; cursorHermeticPeerBound is currently %v",
		(worst * 13 / 2).Round(time.Millisecond), cursorHermeticPeerBound)
}
