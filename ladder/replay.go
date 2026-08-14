// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"context"
	"fmt"
)

// ReplayArgs configures a replay of a recorded corpus through a ladder.
type ReplayArgs struct {
	// Ladder to replay through.
	Ladder *Ladder

	// Fingerprint identifies the rule set this run is measured against.
	//
	// It is required. Behaviour is history-dependent once rules
	// accumulate, so a replay against an unidentified rule set is
	// silently measuring a moving target and its numbers cannot be
	// compared with anything. A content hash rather than a position,
	// because once memory outlives the process a counter means nothing.
	Fingerprint string

	// Corpus is the recorded requests, in order.
	Corpus []*Request

	// ModelLayers names the model-backed rungs.
	ModelLayers []string

	// Delivered judges whether a replayed request produced its work.
	//
	// It is required, and that is deliberate. A replay reporting only
	// which rung answered would measure cost with nothing to weigh it
	// against, which is precisely the measurement a ladder optimising
	// away its own oversight would pass.
	Delivered func(req *Request, res *Result) bool
}

// ReplayReport is what one replay observed.
type ReplayReport struct {
	// Fingerprint is the rule set this ran against, carried so a report
	// cannot be compared against one from a different world by accident.
	Fingerprint string
	Stats       ClassStats
	// PerClass breaks the same numbers down by request class.
	PerClass map[string]ClassStats
}

// Replay runs a recorded corpus through a ladder against a pinned store
// version and reports which rung would have answered each request.
func Replay(ctx context.Context, args *ReplayArgs) (*ReplayReport, error) {
	switch {
	case args == nil:
		return nil, fmt.Errorf("ladder: nil ReplayArgs")
	case args.Ladder == nil:
		return nil, fmt.Errorf("ladder: replay needs a Ladder")
	case args.Fingerprint == "":
		return nil, fmt.Errorf("ladder: replay needs the fingerprint of the rule set it runs against — an unidentified replay measures a moving target")
	case args.Delivered == nil:
		return nil, fmt.Errorf("ladder: replay needs a Delivered judgement — measuring cost with nothing to weigh it against is the failure mode, not the metric")
	}

	meter := NewMeter(&MeterConfig{ModelLayers: args.ModelLayers})
	for _, req := range args.Corpus {
		if req == nil {
			continue
		}
		// Each request gets a fresh copy, so notes appended by one
		// replay do not leak into the next run over the same corpus.
		walked := &Request{Kind: req.Kind, Payload: req.Payload}
		res, err := args.Ladder.Evaluate(ctx, walked)
		meter.Observe(&Observation{
			Request:   walked,
			Result:    res,
			Err:       err,
			Delivered: args.Delivered(walked, res),
		})
	}

	report := &ReplayReport{
		Fingerprint: args.Fingerprint,
		Stats:       meter.Totals(),
		PerClass:    make(map[string]ClassStats),
	}
	for _, kind := range meter.Classes() {
		report.PerClass[kind] = meter.Class(kind)
	}
	return report, nil
}

// ReplayComparison is the verdict on two replays over the same corpus at
// different store versions.
type ReplayComparison struct {
	Before, After string

	// ModelShareDelta is negative when fewer requests reached a model.
	ModelShareDelta float64
	// DeliveredShareDelta is negative when less work got done.
	DeliveredShareDelta float64
	// EscalationRateDelta is negative when the ladder escalated less.
	EscalationRateDelta float64

	// Cheaper reports that fewer requests reached a model.
	Cheaper bool

	// OversightRegression is the finding this whole architecture exists
	// to make visible: cost fell AND delivered work fell with it.
	//
	// Every other system in the cascade literature fails by emitting a
	// worse artifact of the same kind, which sits where you can see it.
	// This one can fail by omission — the escalation does not happen,
	// no deliberation exists to inspect, and the optimised metric
	// improves. Two series diverging across a corpus is the only place
	// that becomes observable, so it is computed rather than left to a
	// reader comparing two numbers by eye.
	OversightRegression bool
}

// Summary renders the comparison for a human, leading with the finding
// rather than the saving.
func (c *ReplayComparison) Summary() string {
	verdict := "no change"
	switch {
	case c.OversightRegression:
		verdict = "OVERSIGHT REGRESSION: cost fell and delivered work fell with it"
	case c.Cheaper:
		verdict = "cheaper at no cost to delivered work"
	case c.DeliveredShareDelta < 0:
		verdict = "delivered work fell without any saving"
	}
	return fmt.Sprintf("%s→%s: model share %+.3f, delivered %+.3f, escalation %+.3f — %s",
		c.Before, c.After, c.ModelShareDelta, c.DeliveredShareDelta, c.EscalationRateDelta, verdict)
}

// CompareReplays weighs two replays of the same corpus against each
// other.
//
// It refuses to compare a report against itself, because a version
// compared with itself always looks stable and would make the oracle
// trivially satisfiable.
func CompareReplays(before, after *ReplayReport) (*ReplayComparison, error) {
	if before == nil || after == nil {
		return nil, fmt.Errorf("ladder: both reports are required")
	}
	if before.Fingerprint == after.Fingerprint {
		return nil, fmt.Errorf("ladder: both reports are at %s; there is nothing to compare", before.Fingerprint)
	}
	if before.Stats.Requests != after.Stats.Requests {
		return nil, fmt.Errorf("ladder: %d requests before and %d after — the corpus must be the same, or the deltas mean nothing",
			before.Stats.Requests, after.Stats.Requests)
	}

	c := &ReplayComparison{
		Before:              before.Fingerprint,
		After:               after.Fingerprint,
		ModelShareDelta:     after.Stats.ModelShare() - before.Stats.ModelShare(),
		DeliveredShareDelta: after.Stats.DeliveredShare() - before.Stats.DeliveredShare(),
		EscalationRateDelta: after.Stats.EscalationRate() - before.Stats.EscalationRate(),
	}
	c.Cheaper = c.ModelShareDelta < 0
	c.OversightRegression = c.ModelShareDelta < 0 && c.DeliveredShareDelta < 0
	return c, nil
}
