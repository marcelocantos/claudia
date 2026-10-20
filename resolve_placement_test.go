// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// 🎯T203 fixtures: one clock, weekly windows reset in 3.5 days.
var placementNow = time.Date(2026, 10, 20, 18, 47, 0, 0, time.UTC)

func placementWeekly(p Provider, used float64, resetIn time.Duration) PlanUsage {
	reset := placementNow.Add(resetIn)
	return PlanUsage{Provider: p, Status: PlanUsageAvailable, Windows: []PlanWindow{{
		Name: PlanWindowWeekly, UsedPercent: floatPtr(used), RemainingPercent: floatPtr(100 - used),
		ResetsAt: &reset, LimitWindow: defaultWeeklyWindow,
	}}}
}

func withSession(u PlanUsage, remaining float64) PlanUsage {
	reset := placementNow.Add(2 * time.Hour)
	u.Windows = append(u.Windows, PlanWindow{
		Name: PlanWindowSession, RemainingPercent: floatPtr(remaining), UsedPercent: floatPtr(100 - remaining),
		ResetsAt: &reset, LimitWindow: defaultSessionWindow,
	})
	return u
}

// specimenPred is the 2026-10-20 18:47 mint: a PO start with no provider,
// standard quality, plan access, Grok and Cursor out of the running, so
// claude/claude-sonnet-5 and codex/gpt-6-sol are the two eligible rows.
func specimenPred(usage ...PlanUsage) ModelPredicates {
	return ModelPredicates{
		Mode: CapabilitySession, Quality: ModelQualityStandard, PreferPlan: true, Now: placementNow,
		ExcludeProviders: []Provider{ProviderGrok, ProviderCursor},
		Usage:            usage,
	}
}

// The specimen: both rows standard, both with headroom, weekly pressure
// within the old 0.05 dead band. Before 🎯T203 this was
// "resolve: token-tied models claude/claude-sonnet-5 codex/gpt-6-sol; set
// TieBreak" and Jevons refused the mint plan_dest_empty.
func TestT203SpecimenResolvesInsteadOfTying(t *testing.T) {
	half := 3*24*time.Hour + 12*time.Hour
	pred := specimenPred(placementWeekly(ProviderClaude, 40, half), placementWeekly(ProviderCodex, 40.5, half))
	if a, b := weeklyPressure(pred.Usage[0], placementNow, nil), weeklyPressure(pred.Usage[1], placementNow, nil); a == b || a-b >= 0.05 || b-a >= 0.05 {
		t.Fatalf("fixture must sit inside the old slack epsilon: %v %v", a, b)
	}
	got, err := Resolve(context.Background(), pred)
	if err != nil {
		t.Fatalf("specimen must resolve, got %v", err)
	}
	if got.Provider != ProviderClaude || got.Model != "claude-sonnet-5" {
		t.Fatalf("lower weekly use (more headroom, same reset) must win: %+v", got)
	}
	// Mirror image: the other row has the headroom.
	pred = specimenPred(placementWeekly(ProviderClaude, 40.5, half), placementWeekly(ProviderCodex, 40, half))
	got, err = Resolve(context.Background(), pred)
	if err != nil || got.Provider != ProviderCodex || got.Model != "gpt-6-sol" {
		t.Fatalf("mirrored specimen: %+v (%v)", got, err)
	}
}

// Identical weekly readings, different session headroom: session headroom
// decides, and the identical readings themselves never error.
func TestT203SpecimenSessionHeadroomDecidesEqualWeekly(t *testing.T) {
	half := 3*24*time.Hour + 12*time.Hour
	for _, tc := range []struct {
		claudeSession, codexSession float64
		want                        Provider
	}{
		{90, 40, ProviderClaude},
		{40, 90, ProviderCodex},
	} {
		pred := specimenPred(
			withSession(placementWeekly(ProviderClaude, 40, half), tc.claudeSession),
			withSession(placementWeekly(ProviderCodex, 40, half), tc.codexSession),
		)
		got, err := Resolve(context.Background(), pred)
		if err != nil || got.Provider != tc.want {
			t.Fatalf("session %v/%v: got %+v (%v), want %s", tc.claudeSession, tc.codexSession, got, err, tc.want)
		}
	}
}

// Exactly equal readings: the draw goes where the injected source says.
func TestT203ExactTieDrawsFromInjectedSource(t *testing.T) {
	half := 3*24*time.Hour + 12*time.Hour
	for draw, want := range map[int]Provider{0: ProviderClaude, 1: ProviderCodex} {
		pred := specimenPred(placementWeekly(ProviderClaude, 40, half), placementWeekly(ProviderCodex, 40, half))
		var asked int
		pred.Rand = func(n int) int { asked = n; return draw }
		got, err := Resolve(context.Background(), pred)
		if err != nil {
			t.Fatal(err)
		}
		if asked != 2 {
			t.Fatalf("draw must be over the 2 exactly tied rows, asked over %d", asked)
		}
		if got.Provider != want {
			t.Fatalf("draw %d: got %s, want %s", draw, got.Provider, want)
		}
	}
	// No tie, no draw.
	pred := specimenPred(placementWeekly(ProviderClaude, 40, half), placementWeekly(ProviderCodex, 41, half))
	pred.Rand = func(int) int { t.Fatal("drew with no exact tie"); return 0 }
	if _, err := Resolve(context.Background(), pred); err != nil {
		t.Fatal(err)
	}
}

// With the production source, an exact tie spreads mints roughly evenly.
// 1000 draws over 2 rows: each lands 40-60% (p of falling outside is
// below 1e-9 for a fair coin).
func TestT203ExactTieSpreadsEvenly(t *testing.T) {
	half := 3*24*time.Hour + 12*time.Hour
	counts := map[Provider]int{}
	const draws = 1000
	for range draws {
		got, err := Resolve(context.Background(), specimenPred(
			placementWeekly(ProviderClaude, 40, half), placementWeekly(ProviderCodex, 40, half)))
		if err != nil {
			t.Fatal(err)
		}
		counts[got.Provider]++
	}
	if len(counts) != 2 {
		t.Fatalf("draws landed on %v, want claude and codex only", counts)
	}
	for p, n := range counts {
		if n < 400 || n > 600 {
			t.Fatalf("%s drew %d/%d, outside 40-60%%: %v", p, n, draws, counts)
		}
	}
	// Unknown usage everywhere is an exact tie too: still a spread, never
	// an error, never a row outside the standard plan shelf.
	counts = map[Provider]int{}
	for range draws {
		got, err := Resolve(context.Background(), ModelPredicates{
			Mode: CapabilitySession, Quality: ModelQualityStandard, PreferPlan: true, Usage: []PlanUsage{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.Quality != ModelQualityStandard || got.Access != ModelAccessPlan {
			t.Fatalf("ineligible draw %+v", got)
		}
		counts[got.Provider]++
	}
	for _, p := range []Provider{ProviderClaude, ProviderCodex, ProviderGrok, ProviderCursor} {
		if counts[p] < 150 || counts[p] > 350 {
			t.Fatalf("%s drew %d/%d over 4 unknown rows, want ~25%%: %v", p, counts[p], draws, counts)
		}
	}
}

// The ranking table: band, then continuous pressure, then headroom.
// Each row names why its winner wins.
func TestT203PlacementTable(t *testing.T) {
	week := 7 * 24 * time.Hour
	for _, tc := range []struct {
		name   string
		claude PlanUsage
		codex  PlanUsage
		want   Provider
		why    string
	}{
		{
			name:   "under beats ok even with less headroom",
			claude: placementWeekly(ProviderClaude, 71, 17*time.Hour),
			codex:  placementWeekly(ProviderCodex, 5, 137*time.Hour),
			want:   ProviderClaude, why: "dest band rank (under) precedes pressure and headroom",
		},
		{
			name:   "locked beats under",
			claude: placementWeekly(ProviderClaude, 10, 6*time.Hour),
			codex:  placementWeekly(ProviderCodex, 60, 20*time.Hour),
			want:   ProviderClaude, why: "locked surplus expires soonest",
		},
		{
			name:   "ok vs ok: lower pressure, tiny gap",
			claude: placementWeekly(ProviderClaude, 50.2, week/2),
			codex:  placementWeekly(ProviderCodex, 50, week/2),
			want:   ProviderCodex, why: "continuous pressure decides inside the band, no dead band",
		},
		{
			name:   "ok vs ok: same use, later reset loses",
			claude: placementWeekly(ProviderClaude, 30, 3*24*time.Hour),
			codex:  placementWeekly(ProviderCodex, 30, 4*24*time.Hour),
			want:   ProviderClaude, why: "same use with less time left is further behind pace",
		},
		{
			name:   "ok vs ahead: ahead is not a destination",
			claude: placementWeekly(ProviderClaude, 70, week/2),
			codex:  placementWeekly(ProviderCodex, 50, week/2),
			want:   ProviderCodex, why: "ahead is never a dest while a dest exists",
		},
		{
			name:   "ok vs hot: hot is ineligible",
			claude: placementWeekly(ProviderClaude, 20, week/2),
			codex:  placementWeekly(ProviderCodex, 85, week/2),
			want:   ProviderClaude, why: "hot fails HasAvailableTokens",
		},
		{
			name:   "equal weekly, session low vetoes",
			claude: withSession(placementWeekly(ProviderClaude, 40, week/2), 10),
			codex:  withSession(placementWeekly(ProviderCodex, 40, week/2), 60),
			want:   ProviderCodex, why: "session-low fails HasAvailableTokens",
		},
		{
			name:   "equal weekly, session headroom decides",
			claude: withSession(placementWeekly(ProviderClaude, 40, week/2), 55),
			codex:  withSession(placementWeekly(ProviderCodex, 40, week/2), 54),
			want:   ProviderClaude, why: "session headroom is the next ranked signal",
		},
		{
			name:   "published dest beats unpublished",
			claude: PlanUsage{Provider: ProviderClaude, Status: PlanUsageUnavailable, Reason: "unpublished"},
			codex:  placementWeekly(ProviderCodex, 60, week/2),
			want:   ProviderCodex, why: "an unknown reading is not a destination while a published one is",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pred := specimenPred(tc.claude, tc.codex)
			pred.Rand = func(int) int { t.Fatalf("no exact tie expected (%s)", tc.why); return 0 }
			got, err := Resolve(context.Background(), pred)
			if err != nil {
				t.Fatalf("%s: %v", tc.why, err)
			}
			if got.Provider != tc.want {
				t.Fatalf("got %+v, want %s: %s", got, tc.want, tc.why)
			}
		})
	}
}

// Same inputs, same pick, whenever the readings are not an exact tie.
func TestT203RankingIsDeterministic(t *testing.T) {
	pred := specimenPred(placementWeekly(ProviderClaude, 40, 80*time.Hour), placementWeekly(ProviderCodex, 40.01, 80*time.Hour))
	first, err := Resolve(context.Background(), pred)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		got, err := Resolve(context.Background(), pred)
		if err != nil || got != first {
			t.Fatalf("run %d: %+v (%v) != %+v", i, got, err, first)
		}
	}
	// Candidate order does not leak into the result.
	pred.Usage[0], pred.Usage[1] = pred.Usage[1], pred.Usage[0]
	if got, err := Resolve(context.Background(), pred); err != nil || got != first {
		t.Fatalf("usage order changed pick: %+v (%v) != %+v", got, err, first)
	}
	cands := []catalogCand{
		{row: CatalogModel{Provider: ProviderCodex, Model: "gpt-6-sol"}, band: PlanBandOK, pressure: -0.10},
		{row: CatalogModel{Provider: ProviderClaude, Model: "claude-sonnet-5"}, band: PlanBandOK, pressure: -0.11},
	}
	a, _ := pickCatalog(cands, "", nil, nil)
	cands[0], cands[1] = cands[1], cands[0]
	b, _ := pickCatalog(cands, "", nil, nil)
	if a.row != b.row || a.row.Provider != ProviderClaude {
		t.Fatalf("pickCatalog order-dependent: %v vs %v", a.row, b.row)
	}
}

// TieBreak biases toward its provider within reach of the leader in the
// same band; it never crosses a band or picks an ineligible row.
func TestT203TieBreakBiasesWithinReach(t *testing.T) {
	half := 3*24*time.Hour + 12*time.Hour
	pred := specimenPred(placementWeekly(ProviderClaude, 40, half), placementWeekly(ProviderCodex, 40.5, half))
	got, err := Resolve(context.Background(), pred)
	if err != nil || got.Provider != ProviderClaude {
		t.Fatalf("baseline: %+v (%v)", got, err)
	}
	pred.TieBreak = []Provider{ProviderCodex}
	got, err = Resolve(context.Background(), pred)
	if err != nil || got.Provider != ProviderCodex {
		t.Fatalf("TieBreak must bias a near-equal choice: %+v (%v)", got, err)
	}
	// Out of reach: a decided pressure gap is not overturned.
	pred.Usage = []PlanUsage{placementWeekly(ProviderClaude, 30, half), placementWeekly(ProviderCodex, 45, half)}
	got, err = Resolve(context.Background(), pred)
	if err != nil || got.Provider != ProviderClaude {
		t.Fatalf("TieBreak must not overturn a decided pressure gap: %+v (%v)", got, err)
	}
	// Across bands: under Claude beats a TieBreak-named ok Codex.
	pred.Usage = []PlanUsage{placementWeekly(ProviderClaude, 71, 17*time.Hour), placementWeekly(ProviderCodex, 5, 137*time.Hour)}
	got, err = Resolve(context.Background(), pred)
	if err != nil || got.Provider != ProviderClaude {
		t.Fatalf("TieBreak must not cross a band: %+v (%v)", got, err)
	}
	// Ineligible: a hot TieBreak provider is never chosen.
	pred.Usage = []PlanUsage{placementWeekly(ProviderClaude, 40, half), placementWeekly(ProviderCodex, 90, half)}
	got, err = Resolve(context.Background(), pred)
	if err != nil || got.Provider != ProviderClaude {
		t.Fatalf("TieBreak must not pick a hot row: %+v (%v)", got, err)
	}
	// On an exact tie TieBreak decides before any draw.
	pred.Usage = []PlanUsage{placementWeekly(ProviderClaude, 40, half), placementWeekly(ProviderCodex, 40, half)}
	pred.Rand = func(int) int { t.Fatal("TieBreak should settle the exact tie"); return 0 }
	got, err = Resolve(context.Background(), pred)
	if err != nil || got.Provider != ProviderCodex {
		t.Fatalf("TieBreak on exact tie: %+v (%v)", got, err)
	}
}

// Nothing ineligible is ever chosen, across a sweep of readings: the pick
// is always a row the eligibility filters would admit on its own.
func TestT203NeverPicksIneligible(t *testing.T) {
	week := 7 * 24 * time.Hour
	for _, cu := range []float64{0, 20, 45, 60, 75, 85, 99, 100} {
		for _, xu := range []float64{0, 20, 45, 60, 75, 85, 99, 100} {
			for _, left := range []time.Duration{6 * time.Hour, week / 2, week - time.Hour} {
				usage := []PlanUsage{placementWeekly(ProviderClaude, cu, left), placementWeekly(ProviderCodex, xu, left)}
				name := fmt.Sprintf("claude%v_codex%v_left%v", cu, xu, left)
				got, err := Resolve(context.Background(), specimenPred(usage...))
				if err != nil {
					for _, u := range usage {
						if IsDestBand(ClassifyPlan(u, placementNow, nil).Weekly) && HasAvailableTokens(u, placementNow, nil) {
							t.Fatalf("%s: refused with %s eligible as a dest: %v", name, u.Provider, err)
						}
					}
					continue
				}
				for _, u := range usage {
					if u.Provider == got.Provider && !HasAvailableTokens(u, placementNow, nil) {
						t.Fatalf("%s: picked ineligible %+v", name, got)
					}
				}
				if got.Provider != ProviderClaude && got.Provider != ProviderCodex {
					t.Fatalf("%s: picked excluded %+v", name, got)
				}
			}
		}
	}
}
