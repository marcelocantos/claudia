// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/ladder"
	"github.com/marcelocantos/claudia/ladder/laddertest"
)

func goodSizing() *ladder.Sizing {
	return &ladder.Sizing{
		Model:           "gemma4:12b",
		ContextTokens:   32768,
		MeasuredLatency: 250 * time.Millisecond,
		MeasuredOn:      "M4 Max, 128 GB",
	}
}

// fakeOllama stands in for a local backend, counting the calls that
// actually reached inference.
func fakeOllama(t *testing.T, reply string, calls *atomic.Int64) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		if req["model"] != "gemma4:12b" {
			t.Errorf("model = %v, want the sized model", req["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"response": reply})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *fixture) localRung(t *testing.T, endpoint string, parsers []ladder.Parser) *ladder.LocalRung {
	t.Helper()
	l, err := ladder.NewLocalRung(&ladder.LocalConfig{
		Scope:    f.scope(t, "local", nil, nil),
		Endpoint: endpoint,
		Sizing:   goodSizing(),
		Parsers:  parsers,
		Prompt:   func(req *ladder.Request) string { return "classify: " + req.Kind },
		Decide: func(req *ladder.Request, response string) (*ladder.Verdict, error) {
			if response == "" {
				return &ladder.Verdict{Kind: ladder.VerdictAbstain, Reason: "no answer"}, nil
			}
			return &ladder.Verdict{Kind: ladder.VerdictAnswer, Rule: "local-model", Answer: response}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewLocalRung: %v", err)
	}
	return l
}

// durationParser answers without any inference at all.
func durationParser() ladder.Parser {
	return ladder.Parser{
		Name: "duration",
		Parse: func(req *ladder.Request) *ladder.Verdict {
			s, ok := req.Payload.(string)
			if !ok {
				return nil
			}
			d, err := time.ParseDuration(s)
			if err != nil {
				return nil // decline, do not deny
			}
			return &ladder.Verdict{Kind: ladder.VerdictAnswer, Answer: d}
		},
	}
}

func TestParsersAnswerBeforeAnyInference(t *testing.T) {
	f := newFixture()
	var calls atomic.Int64
	rung := f.localRung(t, fakeOllama(t, "classified", &calls), []ladder.Parser{durationParser()})
	ctx := context.Background()

	// A duration needs no model, so no model is asked.
	v, err := rung.Evaluate(ctx, &ladder.Request{Kind: "parse", Payload: "1h30m"}, rung.Scope().NewReader())
	if err != nil {
		t.Fatal(err)
	}
	if v.Answer != 90*time.Minute {
		t.Errorf("answer = %v, want 1h30m", v.Answer)
	}
	if v.Rule != "duration" {
		t.Errorf("rule = %q; the parser should stamp itself", v.Rule)
	}
	if calls.Load() != 0 {
		t.Errorf("inference ran %d times for something a parser handled — reaching for a model where a parser would do is the defect", calls.Load())
	}

	// Something the parser declines does reach inference.
	v, err = rung.Evaluate(ctx, &ladder.Request{Kind: "parse", Payload: "not a duration"}, rung.Scope().NewReader())
	if err != nil {
		t.Fatal(err)
	}
	if v.Answer != "classified" {
		t.Errorf("answer = %v", v.Answer)
	}
	if calls.Load() != 1 {
		t.Errorf("inference calls = %d, want 1", calls.Load())
	}

	hits, inferences := rung.Counts()
	if hits != 1 || inferences != 1 {
		t.Errorf("counts = %d parser hits, %d inferences", hits, inferences)
	}
}

func TestAnUnmeasuredModelRefusesToRun(t *testing.T) {
	f := newFixture()
	base := func() *ladder.LocalConfig {
		return &ladder.LocalConfig{
			Scope:    f.scope(t, "local-"+t.Name(), nil, nil),
			Endpoint: "http://127.0.0.1:1",
			Sizing:   goodSizing(),
			Prompt:   func(*ladder.Request) string { return "" },
			Decide:   func(*ladder.Request, string) (*ladder.Verdict, error) { return nil, nil },
		}
	}

	strip := map[string]func(*ladder.Sizing){
		"no model":   func(s *ladder.Sizing) { s.Model = "" },
		"no context": func(s *ladder.Sizing) { s.ContextTokens = 0 },
		"no latency": func(s *ladder.Sizing) { s.MeasuredLatency = 0 },
		"no host":    func(s *ladder.Sizing) { s.MeasuredOn = "" },
	}
	for name, fn := range strip {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			cfg.Scope = f.scope(t, "local-"+name, nil, nil)
			fn(cfg.Sizing)
			if _, err := ladder.NewLocalRung(cfg); err == nil {
				t.Error("an unmeasured model was accepted; sizing must be recorded, not assumed")
			}
		})
	}

	t.Run("no sizing at all", func(t *testing.T) {
		cfg := base()
		cfg.Scope = f.scope(t, "local-nosizing", nil, nil)
		cfg.Sizing = nil
		if _, err := ladder.NewLocalRung(cfg); err == nil {
			t.Error("a rung with no sizing was accepted")
		}
	})
}

func TestAnUnreachableBackendEscalates(t *testing.T) {
	f := newFixture()
	rung, err := ladder.NewLocalRung(&ladder.LocalConfig{
		Scope:    f.scope(t, "local", nil, nil),
		Endpoint: "http://127.0.0.1:1/api/generate",
		Sizing:   goodSizing(),
		HTTP:     &http.Client{Timeout: 50 * time.Millisecond},
		Prompt:   func(*ladder.Request) string { return "x" },
		Decide: func(*ladder.Request, string) (*ladder.Verdict, error) {
			t.Error("Decide ran despite the backend being unreachable")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	top := f.scope(t, "model", nil, nil)
	l := ladder.New(rung, ladder.NewLayerFunc(top, func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
		return &ladder.Verdict{Kind: ladder.VerdictAnswer, Answer: "handled upstairs"}, nil
	}))

	res, err := l.Evaluate(context.Background(), &ladder.Request{Kind: "k"})
	if err != nil {
		t.Fatalf("a dead local rung took the request down: %v", err)
	}
	if res.Layer != "model" || res.Verdict.Answer != "handled upstairs" {
		t.Errorf("answered by %q with %v", res.Layer, res.Verdict.Answer)
	}
	if res.Rungs[0].Err == nil {
		t.Error("the local rung's failure was not recorded")
	}
}

func TestSpeculationIsFreeButBoundedAndYields(t *testing.T) {
	spec := ladder.NewSpeculator(2)
	ctx := context.Background()
	var ran atomic.Int64

	work := func(context.Context) (any, error) {
		ran.Add(1)
		return "brief", nil
	}

	// Precomputed work is claimable once.
	spec.Precompute(ctx, "agent-1", work)
	spec.Wait()
	if v, ok := spec.Take("agent-1"); !ok || v != "brief" {
		t.Errorf("Take = %v, %v", v, ok)
	}
	if _, ok := spec.Take("agent-1"); ok {
		t.Error("a precomputed result was claimed twice")
	}

	// While live work is in flight, speculation stands down. Free of
	// tokens is not free of the machine.
	spec.Live(true)
	before := ran.Load()
	spec.Precompute(ctx, "agent-2", work)
	spec.Wait()
	if ran.Load() != before {
		t.Error("speculation ran while live work was in flight")
	}
	if _, ok := spec.Take("agent-2"); ok {
		t.Error("speculation produced a result while standing down")
	}
	spec.Live(false)

	// A cancelled context produces nothing, and that is not an error:
	// speculative work that cannot run was never owed.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	spec.Precompute(cancelled, "agent-3", work)
	spec.Wait()
	if _, ok := spec.Take("agent-3"); ok {
		t.Error("cancelled speculation filed a result")
	}

	// Unclaimed guesses are thrown away without ceremony.
	spec.Precompute(ctx, "agent-4", work)
	spec.Wait()
	if n := spec.Discard(); n != 1 {
		t.Errorf("Discard = %d, want 1", n)
	}
}

func TestLocalRungSatisfiesTheRungContract(t *testing.T) {
	f := newFixture()
	var calls atomic.Int64
	rung := f.localRung(t, fakeOllama(t, "classified", &calls), []ladder.Parser{durationParser()})

	laddertest.Conform(t, &laddertest.Config{
		Layer: rung,
		Cases: []laddertest.Case{
			{Name: "parser answers", Request: &ladder.Request{Kind: "parse", Payload: "45s"}},
			{Name: "inference answers", Request: &ladder.Request{Kind: "classify", Payload: "opaque"}},
		},
	})
}

func TestLocalRungIsSelectableAsACheapTier(t *testing.T) {
	f := newFixture()
	var localCalls, dearCalls atomic.Int64
	local := f.localRung(t, fakeOllama(t, "local answer", &localCalls), nil)

	dear := f.scope(t, "dear", nil, nil)
	router, err := ladder.NewRouter(&ladder.RouterConfig{
		Scope: f.scope(t, "router", nil, nil),
		Tiers: []ladder.Tier{
			{Name: "dear", Cost: 100, Capabilities: []string{"text"},
				Layer: ladder.NewLayerFunc(dear, func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
					dearCalls.Add(1)
					return &ladder.Verdict{Kind: ladder.VerdictAnswer, Answer: "dear answer"}, nil
				})},
			// Latency, not tokens — which is what makes it the cheap tier.
			{Name: "local", Cost: 1, Capabilities: []string{"text"}, Layer: local},
		},
		Requires: func(*ladder.Request) []string { return []string{"text"} },
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := ladder.New(router).Evaluate(context.Background(), &ladder.Request{Kind: "classify"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Layer != "local" {
		t.Errorf("answered by %q, want local", res.Layer)
	}
	if dearCalls.Load() != 0 {
		t.Error("the dear tier was called when a local one would do")
	}
	if !strings.Contains(local.Sizing().MeasuredOn, "M4 Max") {
		t.Errorf("sizing did not survive: %+v", local.Sizing())
	}
}
