// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package ladder

import (
	"context"
	"fmt"
	"sort"
)

// SymptomKind classifies what a scan noticed.
type SymptomKind string

const (
	// SymptomNeverFired means a rule matched nothing in the corpus.
	SymptomNeverFired SymptomKind = "never_fired"

	// SymptomInert means removing the rule changed no outcome.
	//
	// This is the strongest detector available for a rule that has
	// quietly stopped mattering. You cannot observe the escalation that
	// did not happen, but you CAN observe whether a rule does anything
	// at all.
	SymptomInert SymptomKind = "inert"

	// SymptomLoadBearing means removing the rule changed outcomes. Not
	// a fault — it is the counterpart that makes SymptomInert mean
	// something, and it is worth surfacing so a consolidation pass
	// knows which rules it must not touch casually.
	SymptomLoadBearing SymptomKind = "load_bearing"

	// SymptomContradicted means downstream behaviour denied the rule's
	// verdict. This is the correctness signal consistency cannot
	// supply, and it comes from the consumer.
	SymptomContradicted SymptomKind = "contradicted"

	// SymptomEscalationCollapse means a class stopped escalating.
	//
	// It is not an efficiency finding. A collapse toward zero is
	// indistinguishable from the ladder optimising away its own
	// oversight, and is treated as a defect signal until proven
	// otherwise.
	SymptomEscalationCollapse SymptomKind = "escalation_collapse"

	// SymptomConsistencyOnly means a rule was promoted on repetition
	// with no correctness signal behind it. Consistency proves a rule
	// stable, never right.
	SymptomConsistencyOnly SymptomKind = "consistency_only"
)

// Symptom is something a scan noticed. It is an OBSERVATION, never a
// conclusion: the runtime computes and reports, and never promotes or
// revokes a rule because one looked stale. What a symptom means, and
// whether to act on it, needs the domain.
type Symptom struct {
	Kind   SymptomKind
	RuleID string
	Class  string

	// Detail states the finding in prose, so a report is readable
	// without the reader reconstructing the arithmetic.
	Detail string

	// Observed and Baseline carry the numbers behind the finding where
	// there are any.
	Observed, Baseline float64
}

func (s Symptom) String() string {
	if s.RuleID != "" {
		return fmt.Sprintf("%s [%s]: %s", s.Kind, s.RuleID, s.Detail)
	}
	return fmt.Sprintf("%s [%s]: %s", s.Kind, s.Class, s.Detail)
}

// SymptomScan configures [Scan].
type SymptomScan struct {
	Rules *RuleSet

	// Build makes a ladder from a set of store entries. Claudia does
	// not interpret rules, so replaying an ablated world needs the
	// consumer to say how its entries become rungs.
	Build func(rules []Recalled) (*Ladder, error)

	// Corpus is the recorded request mix to scan against.
	Corpus []*Request

	// ModelLayers names the model-backed rungs.
	ModelLayers []string

	// Delivered judges whether a request produced its work.
	Delivered func(req *Request, res *Result) bool

	// Baseline is an earlier report to compare escalation against.
	// Optional: without it, no collapse can be detected, and the scan
	// says so rather than inventing a comparison.
	Baseline *ReplayReport

	// CollapseFactor is how far an escalation rate may fall before it
	// is reported. 0.5 means "halved". Zero uses 0.5.
	CollapseFactor float64

	// Corrections counts, per rule, how many times downstream behaviour
	// denied its verdict. Supplied by the consumer, because claudia
	// cannot judge an outcome.
	Corrections map[string]int
}

// Scan reports every symptom it can compute, sorted for stable output.
//
// It performs one replay per rule for the ablation pass, so it is an
// OFFLINE operation — a consolidation-pass tool, never something the hot
// path calls.
func Scan(ctx context.Context, args *SymptomScan) ([]Symptom, error) {
	switch {
	case args == nil:
		return nil, fmt.Errorf("ladder: nil SymptomScan")
	case args.Rules == nil:
		return nil, fmt.Errorf("ladder: scan needs a rule set")
	case args.Build == nil:
		return nil, fmt.Errorf("ladder: scan needs a Build — claudia does not interpret rules")
	case args.Delivered == nil:
		return nil, fmt.Errorf("ladder: scan needs a Delivered judgement")
	}

	collapse := args.CollapseFactor
	if collapse <= 0 {
		collapse = 0.5
	}

	current := args.Rules.Rules()
	fp, err := args.Rules.Fingerprint()
	if err != nil {
		return nil, err
	}
	baseReport, baseFired, err := args.replay(ctx, current, fp)
	if err != nil {
		return nil, err
	}

	var out []Symptom

	for _, entry := range current {
		if baseFired[entry.RuleID] == 0 {
			out = append(out, Symptom{
				Kind:   SymptomNeverFired,
				RuleID: entry.RuleID,
				Class:  entry.Class,
				Detail: fmt.Sprintf("matched nothing across %d requests", baseReport.Stats.Requests),
			})
		}
		if entry.ConsistencyOnly {
			out = append(out, Symptom{
				Kind:   SymptomConsistencyOnly,
				RuleID: entry.RuleID,
				Class:  entry.Class,
				Detail: "promoted on repetition with no correctness signal; consistency proves a rule stable, never right",
			})
		}
		if n := args.Corrections[entry.RuleID]; n > 0 {
			out = append(out, Symptom{
				Kind:     SymptomContradicted,
				RuleID:   entry.RuleID,
				Class:    entry.Class,
				Detail:   fmt.Sprintf("downstream behaviour denied this verdict %d times", n),
				Observed: float64(n),
			})
		}

		// The ablation pass: replay the corpus with this rule removed
		// and see whether anything moves.
		ablatedReport, _, err := args.replay(ctx, args.Rules.Ablate(entry.RuleID), fp+"-ablated")
		if err != nil {
			return nil, err
		}
		modelDelta := ablatedReport.Stats.ModelShare() - baseReport.Stats.ModelShare()
		deliveredDelta := ablatedReport.Stats.DeliveredShare() - baseReport.Stats.DeliveredShare()

		switch {
		case modelDelta == 0 && deliveredDelta == 0:
			out = append(out, Symptom{
				Kind:   SymptomInert,
				RuleID: entry.RuleID,
				Class:  entry.Class,
				Detail: "removing it changes no outcome; it is doing nothing the ladder would miss",
			})
		default:
			out = append(out, Symptom{
				Kind:     SymptomLoadBearing,
				RuleID:   entry.RuleID,
				Class:    entry.Class,
				Detail:   fmt.Sprintf("removing it moves model share by %+.3f and delivered work by %+.3f", modelDelta, deliveredDelta),
				Observed: modelDelta,
				Baseline: deliveredDelta,
			})
		}
	}

	if args.Baseline != nil {
		for class, now := range baseReport.PerClass {
			was, ok := args.Baseline.PerClass[class]
			if !ok || was.EscalationRate() == 0 {
				continue
			}
			if now.EscalationRate() <= was.EscalationRate()*collapse {
				out = append(out, Symptom{
					Kind:     SymptomEscalationCollapse,
					Class:    class,
					Detail:   fmt.Sprintf("escalation fell from %.3f to %.3f; a collapse is indistinguishable from the ladder optimising away its own oversight", was.EscalationRate(), now.EscalationRate()),
					Observed: now.EscalationRate(),
					Baseline: was.EscalationRate(),
				})
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].RuleID != out[j].RuleID {
			return out[i].RuleID < out[j].RuleID
		}
		return out[i].Class < out[j].Class
	})
	return out, nil
}

// replay runs the corpus against a candidate set of entries and reports
// both the aggregate and which rules fired.
func (args *SymptomScan) replay(ctx context.Context, rules []Recalled, fingerprint string) (*ReplayReport, map[string]int, error) {
	l, err := args.Build(rules)
	if err != nil {
		return nil, nil, fmt.Errorf("ladder: building a ladder for the scan: %w", err)
	}

	fired := make(map[string]int)
	meter := NewMeter(&MeterConfig{ModelLayers: args.ModelLayers})
	for _, req := range args.Corpus {
		if req == nil {
			continue
		}
		walked := &Request{Kind: req.Kind, Payload: req.Payload}
		res, evalErr := l.Evaluate(ctx, walked)
		if res != nil && res.Verdict != nil && res.Verdict.Rule != "" {
			fired[res.Verdict.Rule]++
		}
		meter.Observe(&Observation{
			Request:   walked,
			Result:    res,
			Err:       evalErr,
			Delivered: args.Delivered(walked, res),
		})
	}

	report := &ReplayReport{Fingerprint: fingerprint, Stats: meter.Totals(), PerClass: make(map[string]ClassStats)}
	for _, kind := range meter.Classes() {
		report.PerClass[kind] = meter.Class(kind)
	}
	return report, fired, nil
}
