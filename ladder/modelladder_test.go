// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder_test

import (
	"context"
	"errors"
	"testing"

	"github.com/marcelocantos/claudia/ladder"
	"github.com/marcelocantos/claudia/ladder/laddertest"
)

// tierLayer is a model rung that answers with its own name, so a test
// can see which tier actually decided.
func (f *fixture) tierLayer(t *testing.T, name string, calls *int, answer string) ladder.Layer {
	t.Helper()
	s := f.scope(t, name, nil, nil)
	return ladder.NewLayerFunc(s, func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
		*calls++
		return &ladder.Verdict{Kind: ladder.VerdictAnswer, Rule: name, Answer: answer}, nil
	})
}

type routerFixture struct {
	router                *ladder.Router
	log                   *ladder.AgreementLog
	cheapCalls, dearCalls int
}

func newRouter(t *testing.T, f *fixture, cheapAnswer, dearAnswer string) *routerFixture {
	t.Helper()
	rf := &routerFixture{log: ladder.NewAgreementLog()}
	r, err := ladder.NewRouter(&ladder.RouterConfig{
		Scope: f.scope(t, "router", nil, nil),
		Tiers: []ladder.Tier{
			// Declared dear-first, to prove the router sorts by cost
			// rather than by declaration order.
			{Name: "dear", Cost: 100, Capabilities: []string{"text", "images", "long-context"},
				Layer: f.tierLayer(t, "dear", &rf.dearCalls, dearAnswer)},
			{Name: "cheap", Cost: 1, Capabilities: []string{"text"},
				Layer: f.tierLayer(t, "cheap", &rf.cheapCalls, cheapAnswer)},
		},
		Requires: func(req *ladder.Request) []string {
			if req.Kind == "vision" {
				return []string{"images"}
			}
			return []string{"text"}
		},
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	rf.router = r
	return rf
}

func TestEscalationLandsOnTheCheapestCapableTier(t *testing.T) {
	f := newFixture()
	rf := newRouter(t, f, "same", "same")

	// A text request only needs the cheap tier, so it goes there even
	// though a more capable one is available.
	res, err := ladder.New(rf.router).Evaluate(context.Background(), &ladder.Request{Kind: "routine"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Layer != "cheap" {
		t.Errorf("answered by %q, want cheap", res.Layer)
	}
	if rf.dearCalls != 0 {
		t.Errorf("the dear tier was called %d times for a request that did not need it", rf.dearCalls)
	}

	// A request needing images cannot be served cheaply, so it climbs —
	// capability, not cost, decides what is eligible.
	res, err = ladder.New(rf.router).Evaluate(context.Background(), &ladder.Request{Kind: "vision"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Layer != "dear" {
		t.Errorf("answered by %q, want dear", res.Layer)
	}

	// A capability nobody has is an error, not a silent downgrade to
	// whatever happened to be nearest.
	unserved, err := ladder.NewRouter(&ladder.RouterConfig{
		Scope: f.scope(t, "router2", nil, nil),
		Tiers: []ladder.Tier{{Name: "only", Cost: 1, Capabilities: []string{"text"},
			Layer: f.tierLayer(t, "only", new(int), "x")}},
		Requires: func(*ladder.Request) []string { return []string{"video"} },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unserved.Select(&ladder.Request{Kind: "k"}); err == nil {
		t.Error("a request routed to a tier that cannot serve it")
	}
}

func TestRouteToACheaperTierMustBeEarnedAtThatBoundary(t *testing.T) {
	f := newFixture()
	rf := newRouter(t, f, "agreed", "agreed")
	ctx := context.Background()
	boundary := ladder.Boundary{Class: "vision", From: "dear", To: "cheap"}

	if err := rf.router.Pin(boundary, rf.log); !errors.Is(err, ladder.ErrInsufficientEvidence) {
		t.Fatalf("err = %v, want ErrInsufficientEvidence with no evidence at all", err)
	}

	// Evidence from a DIFFERENT class does not license this crossing.
	for range 20 {
		if err := rf.router.Shadow(ctx, &ladder.Request{Kind: "routine"}, rf.log, "dear", "cheap"); err != nil {
			t.Fatal(err)
		}
	}
	if err := rf.router.Pin(boundary, rf.log); !errors.Is(err, ladder.ErrInsufficientEvidence) {
		t.Error("evidence earned on one class licensed a crossing on another")
	}

	// Evidence on the right boundary does.
	for range 20 {
		if err := rf.router.Shadow(ctx, &ladder.Request{Kind: "vision"}, rf.log, "dear", "cheap"); err != nil {
			t.Fatal(err)
		}
	}
	if err := rf.router.Pin(boundary, rf.log); err != nil {
		t.Fatalf("Pin with earned evidence: %v", err)
	}

	// The class now routes to the cheap tier even though its declared
	// capabilities would not have selected it — the evidence is what
	// changed, not the capability model.
	res, err := ladder.New(rf.router).Evaluate(ctx, &ladder.Request{Kind: "vision"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Layer != "cheap" {
		t.Errorf("answered by %q after pinning, want cheap", res.Layer)
	}

	// Unpinning needs no evidence: stopping is always allowed.
	rf.router.Unpin("vision")
	res, err = ladder.New(rf.router).Evaluate(ctx, &ladder.Request{Kind: "vision"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Layer != "dear" {
		t.Errorf("answered by %q after unpinning, want dear", res.Layer)
	}
}

func TestDisagreementBlocksTheRoute(t *testing.T) {
	f := newFixture()
	// The cheap tier reaches a different answer than the dear one.
	rf := newRouter(t, f, "cheap answer", "dear answer")
	ctx := context.Background()
	boundary := ladder.Boundary{Class: "routine", From: "dear", To: "cheap"}

	for range 40 {
		if err := rf.router.Shadow(ctx, &ladder.Request{Kind: "routine"}, rf.log, "dear", "cheap"); err != nil {
			t.Fatal(err)
		}
	}
	ev := rf.log.Evidence(boundary)
	if ev.Runs != 40 || ev.Identical != 0 {
		t.Fatalf("evidence = %+v, want 40 runs and no agreement", ev)
	}
	if err := rf.router.Pin(boundary, rf.log); !errors.Is(err, ladder.ErrInsufficientEvidence) {
		t.Errorf("a route was pinned despite total disagreement: %v", err)
	}
}

func TestPinRefusesAnUpgradeDressedAsADemotion(t *testing.T) {
	f := newFixture()
	rf := newRouter(t, f, "same", "same")
	ctx := context.Background()

	for range 20 {
		if err := rf.router.Shadow(ctx, &ladder.Request{Kind: "routine"}, rf.log, "cheap", "dear"); err != nil {
			t.Fatal(err)
		}
	}
	// Perfect agreement, but this crossing goes UP in cost. Routing a
	// class to a dearer tier is not something evidence of agreement
	// argues for.
	err := rf.router.Pin(ladder.Boundary{Class: "routine", From: "cheap", To: "dear"}, rf.log)
	if err == nil {
		t.Error("a crossing to a dearer tier was pinned as if it were a demotion")
	}
}

func TestRouterConfigRejectsWhatCannotWork(t *testing.T) {
	f := newFixture()
	s := f.scope(t, "r", nil, nil)
	good := ladder.Tier{Name: "a", Cost: 1, Layer: f.tierLayer(t, "a", new(int), "x")}

	cases := map[string]*ladder.RouterConfig{
		"no scope":       {Tiers: []ladder.Tier{good}},
		"no tiers":       {Scope: s},
		"unnamed tier":   {Scope: s, Tiers: []ladder.Tier{{Cost: 1, Layer: good.Layer}}},
		"tier no layer":  {Scope: s, Tiers: []ladder.Tier{{Name: "b", Cost: 1}}},
		"duplicate tier": {Scope: s, Tiers: []ladder.Tier{good, good}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ladder.NewRouter(cfg); err == nil {
				t.Error("config accepted")
			}
		})
	}
}

// A third engine, with nothing in common with the other two, against the
// same suite.
func TestRouterSatisfiesTheRungContract(t *testing.T) {
	f := newFixture()
	rf := newRouter(t, f, "same", "same")
	laddertest.Conform(t, &laddertest.Config{
		Layer: rf.router,
		Cases: []laddertest.Case{
			{Name: "routine text", Request: &ladder.Request{Kind: "routine"}},
			{Name: "needs images", Request: &ladder.Request{Kind: "vision"}},
		},
	})
}
