// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"testing"
	"time"
)

// 🎯T86: a plan with headroom can still hold a model with none.
//
// The payload below is the real 2026-09-20 shape, trimmed: the account's
// weekly window at 67% used with room to spare, and Fable — metered only
// in limits[], with no top-level key — at 100%.
const t86Body = `{
  "five_hour": {"utilization": 10.0, "resets_at": "2026-09-20T06:00:00.697284+00:00"},
  "seven_day": {"utilization": 67.0, "resets_at": "2026-09-21T02:00:00.697306+00:00"},
  "seven_day_opus": null,
  "limits": [
    {"kind": "session", "group": "session", "percent": 10, "scope": null,
     "resets_at": "2026-09-20T06:00:00.697284+00:00"},
    {"kind": "weekly_all", "group": "weekly", "percent": 67, "scope": null,
     "resets_at": "2026-09-21T02:00:00.697306+00:00"},
    {"kind": "weekly_scoped", "group": "weekly", "percent": 100,
     "scope": {"model": {"id": null, "display_name": "Fable"}},
     "resets_at": "2026-09-21T01:59:58.697497+00:00"}
  ]
}`

func t86Now(t *testing.T) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, "2026-09-20T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The per-model window is read from limits[], labelled by the server.
func TestT86PerModelWeeklyWindowIsParsed(t *testing.T) {
	pu, err := parseClaudeOAuthUsage([]byte(t86Body), t86Now(t))
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageAvailable {
		t.Fatalf("status %q, want available", pu.Status)
	}
	w := ModelWindowFor(pu, "claude-fable-5")
	if w == nil {
		t.Fatal("no per-model window for Fable; the limits[] entry was dropped")
	}
	if w.Model != "Fable" {
		t.Fatalf("model label %q, want the server's own \"Fable\"", w.Model)
	}
	if w.UsedPercent == nil || *w.UsedPercent != 100 {
		t.Fatalf("used = %v, want 100", w.UsedPercent)
	}
	if w.RemainingPercent == nil || *w.RemainingPercent != 0 {
		t.Fatalf("remaining = %v, want 0", w.RemainingPercent)
	}
	if w.ResetsAt == nil {
		t.Fatal("the per-model window lost its rollover")
	}
	// The account's own weekly is untouched and still says 67% used.
	var weekly *PlanWindow
	for i := range pu.Windows {
		if pu.Windows[i].Name == PlanWindowWeekly {
			weekly = &pu.Windows[i]
		}
	}
	if weekly == nil || weekly.UsedPercent == nil || *weekly.UsedPercent != 67 {
		t.Fatalf("plan weekly window changed: %+v", weekly)
	}
	if weekly.Model != "" {
		t.Fatal("the plan window was attributed to a model")
	}
}

// A scoped entry with no model name is dropped: an unattributed figure
// would read as the plan's own.
func TestT86UnnamedScopeIsDropped(t *testing.T) {
	body := `{"seven_day":{"utilization":10},"limits":[
		{"kind":"weekly_scoped","percent":100,"scope":{"model":{"display_name":"  "}}}]}`
	pu, err := parseClaudeOAuthUsage([]byte(body), t86Now(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range pu.Windows {
		if w.Name == PlanWindowModelWeekly {
			t.Fatalf("an unattributed scoped window was published: %+v", w)
		}
	}
}

// The label is matched against a catalog id only as a whole word.
func TestT86ModelMatchingIsNarrow(t *testing.T) {
	pu := PlanUsage{Windows: []PlanWindow{
		{Name: PlanWindowModelWeekly, Model: "Fable", RemainingPercent: floatPtr(0)},
	}}
	for _, id := range []string{"claude-fable-5", "claude-fable-5[1m]", "fable"} {
		if ModelWindowFor(pu, id) == nil {
			t.Fatalf("%q did not match the Fable window", id)
		}
	}
	for _, id := range []string{"claude-opus-5", "fabled-model-1", "", "claude-sonnet-5"} {
		if ModelWindowFor(pu, id) != nil {
			t.Fatalf("%q matched the Fable window and should not have", id)
		}
	}
}

// The rule that matters: the plan has headroom, the model does not.
func TestT86SpentModelIsIneligibleWhileThePlanIsFine(t *testing.T) {
	now := t86Now(t)
	pu, err := parseClaudeOAuthUsage([]byte(t86Body), now)
	if err != nil {
		t.Fatal(err)
	}
	if !HasAvailableTokens(pu, now, nil) {
		t.Fatal("the plan itself should still have tokens at 67% weekly")
	}
	if ModelHasAvailableTokens(pu, "claude-fable-5", now, nil) {
		t.Fatal("Fable at 100% used was judged able to accept a turn")
	}
	// A sibling model on the same plan is unaffected.
	if !ModelHasAvailableTokens(pu, "claude-opus-5", now, nil) {
		t.Fatal("Opus was refused on Fable's exhaustion")
	}
	// And a model with no window of its own is judged by the plan alone:
	// absence of a figure is not exhaustion.
	if !ModelHasAvailableTokens(pu, "claude-haiku-4-5", now, nil) {
		t.Fatal("a model with no published window was treated as spent")
	}
	if got := ExhaustedModels(pu); len(got) != 1 || got[0] != "Fable" {
		t.Fatalf("ExhaustedModels = %v, want [Fable]", got)
	}
}

// End to end through Resolve: asking for a frontier Claude seat must not
// land on the spent model when a live sibling exists.
func TestT86ResolveSkipsTheSpentModel(t *testing.T) {
	now := t86Now(t)
	pu, err := parseClaudeOAuthUsage([]byte(t86Body), now)
	if err != nil {
		t.Fatal(err)
	}
	pick, err := Resolve(context.Background(), ModelPredicates{
		Mode:           CapabilitySession,
		Quality:        ModelQualityFrontier,
		PreferProvider: ProviderClaude,
		Usage:          []PlanUsage{pu},
		Now:            now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pick.Model == "claude-fable-5" {
		t.Fatalf("Resolve placed a seat on a model with no tokens: %+v", pick)
	}
}
