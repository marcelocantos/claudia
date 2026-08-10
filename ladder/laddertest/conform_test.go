// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package laddertest_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/ladder"
	"github.com/marcelocantos/claudia/ladder/laddertest"
)

// fakeTB records what the harness complained about, so a test can assert
// the harness CATCHES a violation rather than only that it tolerates a
// compliant layer.
type fakeTB struct{ msgs []string }

func (f *fakeTB) Helper() {}
func (f *fakeTB) Errorf(format string, args ...any) {
	f.msgs = append(f.msgs, fmt.Sprintf(format, args...))
}
func (f *fakeTB) joined() string { return strings.Join(f.msgs, "\n") }

type harness struct {
	reg    *ladder.Registry
	scope  *ladder.Scope
	held   ladder.Action // in the layer's manifest
	unheld ladder.Action // registered, deliberately not in the manifest
	read   ladder.Read
	reads  int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{reg: ladder.NewRegistry()}
	h.held = h.reg.Action(&ladder.ActionDef{
		Name:    "held",
		Handler: func(context.Context, any) (any, error) { return "ok", nil },
	})
	h.unheld = h.reg.Action(&ladder.ActionDef{
		Name:    "unheld",
		Handler: func(context.Context, any) (any, error) { return "ok", nil },
	})
	h.read = h.reg.Read(&ladder.ReadDef{
		Name: "counter",
		Handler: func(context.Context, any) (any, error) {
			h.reads++
			return h.reads, nil
		},
	})
	s, err := h.reg.Resolve(&ladder.Manifest{
		Layer:   "under-test",
		Reads:   []ladder.Read{h.read},
		Actions: []ladder.Action{h.held},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.scope = s
	return h
}

func (h *harness) cases() []laddertest.Case {
	return []laddertest.Case{
		{Name: "answers", Request: &ladder.Request{Kind: "answerable"}},
		{Name: "abstains", Request: &ladder.Request{Kind: "other"}},
	}
}

func (h *harness) layer(fn func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error)) ladder.Layer {
	return ladder.NewLayerFunc(h.scope, fn)
}

func TestConformPassesACompliantLayer(t *testing.T) {
	h := newHarness(t)
	compliant := h.layer(func(ctx context.Context, req *ladder.Request, rd *ladder.Reader) (*ladder.Verdict, error) {
		if _, err := rd.Do(ctx, h.read, nil); err != nil {
			return nil, err
		}
		if req.Kind == "answerable" {
			return &ladder.Verdict{Kind: ladder.VerdictAct, Rule: "r", Action: h.held}, nil
		}
		return &ladder.Verdict{Kind: ladder.VerdictAbstain, Reason: "not mine"}, nil
	})

	f := &fakeTB{}
	laddertest.Conform(f, &laddertest.Config{Layer: compliant, Cases: h.cases()})
	if len(f.msgs) != 0 {
		t.Errorf("compliant layer failed conformance:\n%s", f.joined())
	}
}

// Each violation below is a way a rung can be wrong that the ladder
// depends on catching. A harness that tolerates any of them is not
// evidence of substitutability.
func TestConformCatchesContractViolations(t *testing.T) {
	tests := []struct {
		name string
		fn   func(h *harness) ladder.Layer
		want string
	}{
		{
			name: "silent pass-through",
			fn: func(h *harness) ladder.Layer {
				return h.layer(func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
					return nil, nil
				})
			},
			want: "silent pass-through",
		},
		{
			name: "names an action outside its manifest",
			fn: func(h *harness) ladder.Layer {
				return h.layer(func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
					return &ladder.Verdict{Kind: ladder.VerdictAct, Action: h.unheld}, nil
				})
			},
			want: "does not satisfy the layer's own scope",
		},
		{
			name: "abstains while naming an action",
			fn: func(h *harness) ladder.Layer {
				return h.layer(func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
					return &ladder.Verdict{Kind: ladder.VerdictAbstain, Action: h.held}, nil
				})
			},
			want: "does not satisfy the layer's own scope",
		},
		{
			name: "panics",
			fn: func(h *harness) ladder.Layer {
				return h.layer(func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
					panic("boom")
				})
			},
			want: "panicked",
		},
		{
			name: "not deterministic under replay",
			fn: func(h *harness) ladder.Layer {
				calls := 0
				return h.layer(func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
					calls++
					if calls%2 == 0 {
						return &ladder.Verdict{Kind: ladder.VerdictAbstain}, nil
					}
					return &ladder.Verdict{Kind: ladder.VerdictAct, Action: h.held}, nil
				})
			},
			want: "replay verdict kind",
		},
		{
			name: "unknown verdict kind",
			fn: func(h *harness) ladder.Layer {
				return h.layer(func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
					return &ladder.Verdict{Kind: "sideways"}, nil
				})
			},
			want: "unknown verdict kind",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			f := &fakeTB{}
			laddertest.Conform(f, &laddertest.Config{Layer: tc.fn(h), Cases: h.cases()})
			if !strings.Contains(f.joined(), tc.want) {
				t.Errorf("harness did not catch %q\nwant a message containing %q\ngot:\n%s", tc.name, tc.want, f.joined())
			}
		})
	}
}

func TestConformAllowsAFailingLayerButNotAnEmptySuite(t *testing.T) {
	h := newHarness(t)

	// Returning an error is legitimate: the ladder escalates past a
	// broken rung rather than dropping the request.
	failing := h.layer(func(context.Context, *ladder.Request, *ladder.Reader) (*ladder.Verdict, error) {
		return nil, errors.New("store unavailable")
	})
	f := &fakeTB{}
	laddertest.Conform(f, &laddertest.Config{Layer: failing, Cases: h.cases()})
	if len(f.msgs) != 0 {
		t.Errorf("a rung that fails upward was reported as non-conformant:\n%s", f.joined())
	}

	// An empty suite would pass anything, so it is itself a failure.
	f = &fakeTB{}
	laddertest.Conform(f, &laddertest.Config{Layer: failing})
	if !strings.Contains(f.joined(), "no cases") {
		t.Errorf("empty suite accepted: %s", f.joined())
	}
}
