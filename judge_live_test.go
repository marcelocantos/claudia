// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

const jevLiveEnv = "CLAUDIA_JEV_LIVE"

// judgeProbabilitySlack is how far from 1 a distribution may sum: the API
// rounds each probability to two places.
const judgeProbabilitySlack = 0.05

// TestJudgeLiveSmoke asks the real TypeSafe endpoint one Choice and one
// Noul (🎯T127). Skip unless CLAUDIA_JEV_LIVE=1; needs a key in
// TYPESAFE_API_KEY or ~/.typesafe/env. Direct, so it measures the library
// call itself; the daemon hop is hermetic (daemon.TestJudgeGoesThroughDaemon)
// and sends the same bytes. One request, about 300 input tokens.
func TestJudgeLiveSmoke(t *testing.T) {
	if os.Getenv(jevLiveEnv) != "1" {
		t.Skipf("set %s=1 to run the live Jev smoke", jevLiveEnv)
	}
	j := NewJudge(JudgeConfig{})
	j.SetDirect(true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := j.Ask(ctx, JudgeRequest{
		State: "Help! My payouts have been failing for 3 days.",
		Questions: map[string]JudgeQuestion{
			"is_urgent": {Type: JudgeNoul, Instructions: "Does this convey urgency?"},
			"department": {Type: JudgeChoice, Instructions: "Which team should handle this?",
				Options: map[string]any{"billing": "Payments, invoicing, refunds", "technical": "Bugs, outages, integrations", "sales": "Pricing, upgrades, new accounts"}},
		},
	})
	if errors.Is(err, ErrJudgeNoKey) {
		t.Skipf("residue, not a pass: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Model, "jev-") || res.Model == DefaultJudgeModel {
		t.Fatalf("resolved model = %q; want a release, not the alias", res.Model)
	}
	if res.Usage.InputTokens == 0 || res.Usage.OutputTokens == 0 {
		t.Fatalf("usage = %+v", res.Usage)
	}
	u := res.Answers["is_urgent"]
	if u.Noul < 0 || u.Noul > 1 {
		t.Fatalf("noul = %v", u.Noul)
	}
	d := res.Answers["department"]
	sum := 0.0
	for _, p := range d.Probabilities {
		sum += p
	}
	if len(d.Probabilities) != 3 || math.Abs(sum-1) > judgeProbabilitySlack {
		t.Fatalf("choice distribution %v sums to %v", d.Probabilities, sum)
	}
	t.Logf("live jev ok: model=%s urgent=%.2f dept=%s %v usage=%+v %.0fms attempts=%d",
		res.Model, u.Noul, d.Choice, d.Probabilities, res.Usage, res.DurationMs, res.Attempts)
}
