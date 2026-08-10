// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder_test

import (
	"context"
	"errors"
	"testing"

	"github.com/marcelocantos/claudia/ladder"
)

// fixture builds a registry with one effectful action, one layer-only
// action, and one read, plus counters so a test can prove whether a
// handler actually ran.
type fixture struct {
	reg *ladder.Registry

	spawn  ladder.Action // model-exposed
	reap   ladder.Action // layer-only
	status ladder.Read

	spawned  int
	reaped   int
	statused int
}

func newFixture() *fixture {
	f := &fixture{reg: ladder.NewRegistry()}
	f.spawn = f.reg.Action(&ladder.ActionDef{
		Name:         "agent.spawn",
		Description:  "start a worker for a target that has none",
		ModelExposed: true,
		Handler: func(ctx context.Context, args any) (any, error) {
			f.spawned++
			return "spawned", nil
		},
	})
	f.reap = f.reg.Action(&ladder.ActionDef{
		Name:        "agent.reap",
		Description: "retire a worker that has finished and reported",
		Handler: func(ctx context.Context, args any) (any, error) {
			f.reaped++
			return "reaped", nil
		},
	})
	f.status = f.reg.Read(&ladder.ReadDef{
		Name:        "agent.status",
		Description: "current phase of a named agent",
		Handler: func(ctx context.Context, args any) (any, error) {
			f.statused++
			return "idle", nil
		},
	})
	return f
}

// reaperScope is a layer that may reap but not spawn — the manifest
// example the design is written around.
func (f *fixture) reaperScope(t *testing.T) *ladder.Scope {
	t.Helper()
	s, err := f.reg.Resolve(&ladder.Manifest{
		Layer:   "reaper",
		Reads:   []ladder.Read{f.status},
		Actions: []ladder.Action{f.reap},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return s
}

func TestVerdictOutsideManifestIsRefusedBeforeExecuting(t *testing.T) {
	f := newFixture()
	s := f.reaperScope(t)

	// The reaper layer may reap. It may not spawn, even though spawn is
	// registered and its handle is in hand.
	_, err := s.Perform(context.Background(), &ladder.Verdict{
		Kind:   ladder.VerdictAct,
		Layer:  "reaper",
		Action: f.spawn,
	})
	if err == nil {
		t.Fatal("spawn from a reap-only layer succeeded; the manifest is not the surface")
	}
	var scopeErr *ladder.ScopeError
	if !errors.As(err, &scopeErr) {
		t.Fatalf("want *ladder.ScopeError, got %T: %v", err, err)
	}
	if scopeErr.Layer != "reaper" || scopeErr.Name != "agent.spawn" {
		t.Errorf("ScopeError does not name the drift: %+v", scopeErr)
	}
	if f.spawned != 0 {
		t.Errorf("handler ran %d times; refusal must precede execution", f.spawned)
	}

	// The action it does hold still works, so the refusal is scoped
	// rather than a blanket failure.
	if _, err := s.Perform(context.Background(), &ladder.Verdict{
		Kind:   ladder.VerdictAct,
		Layer:  "reaper",
		Action: f.reap,
	}); err != nil {
		t.Fatalf("in-scope reap failed: %v", err)
	}
	if f.reaped != 1 {
		t.Errorf("reaped = %d, want 1", f.reaped)
	}
}

func TestZeroAndForeignHandlesAreRejected(t *testing.T) {
	f := newFixture()

	var zeroAction ladder.Action
	var zeroRead ladder.Read
	if !zeroAction.IsZero() || !zeroRead.IsZero() {
		t.Fatal("zero handles do not report themselves as zero")
	}

	if _, err := f.reg.Resolve(&ladder.Manifest{
		Layer:   "bad",
		Actions: []ladder.Action{zeroAction},
	}); err == nil {
		t.Error("Resolve accepted a zero Action handle")
	}
	if _, err := f.reg.Resolve(&ladder.Manifest{
		Layer: "bad",
		Reads: []ladder.Read{zeroRead},
	}); err == nil {
		t.Error("Resolve accepted a zero Read handle")
	}
	if _, err := f.reg.Resolve(&ladder.Manifest{Layer: "", Actions: []ladder.Action{f.reap}}); err == nil {
		t.Error("Resolve accepted an anonymous layer")
	}

	// A handle minted by another consumer's registry must not work
	// here: claudia is explicitly multi-consumer.
	other := newFixture()
	if _, err := f.reg.Resolve(&ladder.Manifest{
		Layer:   "foreign",
		Actions: []ladder.Action{other.reap},
	}); err == nil {
		t.Error("Resolve accepted a handle from a different registry")
	}
}

func TestLookupIsTheLoadTimeGate(t *testing.T) {
	f := newFixture()
	s := f.reaperScope(t)

	// This is what an interpreted rule does at load time: it names an
	// action by string and the lookup either resolves or the rule is
	// rejected before it ever becomes active.
	if _, err := s.LookupAction("agent.reap"); err != nil {
		t.Errorf("in-scope lookup failed: %v", err)
	}
	if _, err := s.LookupAction("agent.spawn"); err == nil {
		t.Error("lookup resolved an action outside the layer's scope")
	}
	if _, err := s.LookupAction("agent.nonexistent"); err == nil {
		t.Error("lookup resolved an unregistered action")
	}
	if _, err := s.LookupRead("agent.status"); err != nil {
		t.Errorf("in-scope read lookup failed: %v", err)
	}

	// Handle to string is total; string to handle is what can fail.
	a, err := s.LookupAction("agent.reap")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name() != "agent.reap" {
		t.Errorf("Name() = %q, want agent.reap", a.Name())
	}
}

func TestRecordedReadReplaysWithoutReissuing(t *testing.T) {
	f := newFixture()
	s := f.reaperScope(t)
	ctx := context.Background()

	live := s.NewReader()
	got, err := live.Do(ctx, f.status, "jevons-po")
	if err != nil {
		t.Fatalf("live read: %v", err)
	}
	if got != "idle" {
		t.Fatalf("live read = %v, want idle", got)
	}
	if f.statused != 1 {
		t.Fatalf("read handler ran %d times, want 1", f.statused)
	}

	records := live.Records()
	if len(records) != 1 || records[0].Name != "agent.status" {
		t.Fatalf("records = %+v", records)
	}

	// Replay serves the recorded answer. Purity here means determinism,
	// not abstinence: the classifier looked something up, and the
	// lookup does not happen again.
	replay := s.NewReplayReader(records)
	got, err = replay.Do(ctx, f.status, "jevons-po")
	if err != nil {
		t.Fatalf("replay read: %v", err)
	}
	if got != "idle" {
		t.Errorf("replay read = %v, want idle", got)
	}
	if f.statused != 1 {
		t.Errorf("read handler ran %d times after replay; replay re-issued the query", f.statused)
	}
}

func TestReplayDivergenceIsAnErrorNotAFreshQuery(t *testing.T) {
	f := newFixture()
	s := f.reaperScope(t)
	ctx := context.Background()

	// A recording for a different read than the one now being issued.
	replay := s.NewReplayReader([]ladder.ReadRecord{{Name: "agent.somethingelse", Result: "x"}})
	if _, err := replay.Do(ctx, f.status, nil); err == nil {
		t.Error("replay accepted a diverging read instead of reporting it")
	}
	if f.statused != 0 {
		t.Error("divergence silently fell back to a live query")
	}

	// Running past the end of a recording is likewise an error: the
	// recording is evidence about one request, and filling a gap would
	// make it evidence about a different one.
	exhausted := s.NewReplayReader(nil)
	if _, err := exhausted.Do(ctx, f.status, nil); err == nil {
		t.Error("replay past the end of the recording did not fail")
	}
	if f.statused != 0 {
		t.Error("exhausted replay fell back to a live query")
	}
}

func TestReadOutsideManifestIsRefused(t *testing.T) {
	f := newFixture()
	nostate, err := f.reg.Resolve(&ladder.Manifest{Layer: "nostate", Actions: []ladder.Action{f.reap}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nostate.NewReader().Do(context.Background(), f.status, nil); err == nil {
		t.Error("a layer with no reads issued one")
	}
	if f.statused != 0 {
		t.Error("out-of-scope read reached the handler")
	}
}

func TestAbstainAndAnswerNameNoAction(t *testing.T) {
	f := newFixture()
	s := f.reaperScope(t)

	for _, v := range []*ladder.Verdict{
		{Kind: ladder.VerdictAbstain, Action: f.reap},
		{Kind: ladder.VerdictAnswer, Action: f.reap},
	} {
		if err := v.Validate(s); err == nil {
			t.Errorf("%s verdict carrying an action was accepted", v.Kind)
		}
	}

	// A well-formed abstention is valid, and Perform refuses it because
	// there is nothing to do — not because it is malformed.
	abstain := &ladder.Verdict{Kind: ladder.VerdictAbstain, Layer: "reaper", Reason: "no rule matched"}
	if err := abstain.Validate(s); err != nil {
		t.Errorf("well-formed abstention rejected: %v", err)
	}
	if _, err := s.Perform(context.Background(), abstain); err == nil {
		t.Error("Perform acted on an abstention")
	}

	if err := (&ladder.Verdict{Kind: ladder.VerdictAct, Layer: "reaper"}).Validate(s); err == nil {
		t.Error("acting verdict with no action was accepted")
	}
	if err := (&ladder.Verdict{Kind: "sideways"}).Validate(s); err == nil {
		t.Error("unknown verdict kind was accepted")
	}
}

func TestModelExposureIsDeclaredPerAction(t *testing.T) {
	f := newFixture()

	model := f.reg.ModelActions()
	if len(model) != 1 || model[0] != "agent.spawn" {
		t.Errorf("ModelActions() = %v, want [agent.spawn]", model)
	}

	// Everything here is a capability the cheapest actor has and the
	// supervised one does not, so a consumer audits the list on purpose.
	layerOnly := f.reg.LayerOnlyActions()
	if len(layerOnly) != 1 || layerOnly[0] != "agent.reap" {
		t.Errorf("LayerOnlyActions() = %v, want [agent.reap]", layerOnly)
	}
}

func TestVerdictExplainsItselfWithoutAModel(t *testing.T) {
	f := newFixture()
	s := f.reaperScope(t)

	withReason := &ladder.Verdict{
		Kind:   ladder.VerdictAct,
		Layer:  "reaper",
		Rule:   "reap-finished-worker",
		Reason: "worker reported and has no open mission",
		Action: f.reap,
	}
	got := withReason.Explain(s)
	want := "reaper (reap-finished-worker): worker reported and has no open mission [act]"
	if got != want {
		t.Errorf("Explain() = %q, want %q", got, want)
	}

	// With no reason of its own, it falls back to the prose the action
	// was registered with rather than explaining nothing.
	bare := &ladder.Verdict{Kind: ladder.VerdictAct, Layer: "reaper", Action: f.reap}
	if got := bare.Explain(s); got != "reaper: retire a worker that has finished and reported [act]" {
		t.Errorf("Explain() fallback = %q", got)
	}
}

func TestRegistrationMistakesFailAtConstruction(t *testing.T) {
	cases := map[string]func(){
		"duplicate action": func() {
			r := ladder.NewRegistry()
			def := &ladder.ActionDef{Name: "a", Handler: func(context.Context, any) (any, error) { return nil, nil }}
			r.Action(def)
			r.Action(def)
		},
		"empty name": func() {
			ladder.NewRegistry().Action(&ladder.ActionDef{Handler: func(context.Context, any) (any, error) { return nil, nil }})
		},
		"nil handler": func() {
			ladder.NewRegistry().Action(&ladder.ActionDef{Name: "a"})
		},
		"duplicate read": func() {
			r := ladder.NewRegistry()
			def := &ladder.ReadDef{Name: "r", Handler: func(context.Context, any) (any, error) { return nil, nil }}
			r.Read(def)
			r.Read(def)
		},
	}

	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("registration mistake did not fail at construction")
				}
			}()
			fn()
		})
	}
}

func TestReadErrorsAreRecordedAndReplayed(t *testing.T) {
	reg := ladder.NewRegistry()
	boom := reg.Read(&ladder.ReadDef{
		Name:    "boom",
		Handler: func(context.Context, any) (any, error) { return nil, errors.New("upstream down") },
	})
	s, err := reg.Resolve(&ladder.Manifest{Layer: "l", Reads: []ladder.Read{boom}})
	if err != nil {
		t.Fatal(err)
	}

	live := s.NewReader()
	if _, err := live.Do(context.Background(), boom, nil); err == nil {
		t.Fatal("failing read reported success")
	}
	records := live.Records()
	if len(records) != 1 || records[0].Err == "" {
		t.Fatalf("failure was not recorded: %+v", records)
	}

	// A request that failed replays as having failed, so a recording
	// cannot quietly become a happier one.
	if _, err := s.NewReplayReader(records).Do(context.Background(), boom, nil); err == nil {
		t.Error("replay of a recorded failure succeeded")
	}
}
