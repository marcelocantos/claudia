// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A provider may end the first turn with an error instead of a terminal
// assistant event. The live journey must report that error, not wait 180s.
func TestGoalJourneyFirstTurnProviderError(t *testing.T) {
	const refusal = "You've hit your usage limit; try again after reset"
	start := func(cfg Config) (*Agent, error) {
		var agent *Agent
		var err error
		agent, err = StartStub(context.Background(), cfg, &StubAgentOps{
			Send: func(string) error {
				agent.PublishEvent(Event{Type: "system", IsError: true, Text: refusal})
				return nil
			},
		})
		return agent, err
	}
	began := time.Now()
	err := runLiveGoalJourney(t, Config{Goal: "Produce three observations"}, start, time.Second)
	if err == nil || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("journey error = %v, want provider refusal %q", err, refusal)
	}
	if elapsed := time.Since(began); elapsed >= time.Second {
		t.Fatalf("provider error took %s; should fail before the test deadline", elapsed)
	}
}
