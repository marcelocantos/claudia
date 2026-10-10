// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"errors"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

func TestOMPAnswerCopiesSidecarTurnID(t *testing.T) {
	a := &Agent{eventSubs: make(map[int64]EventFunc)}
	var got []Event
	a.SubscribeEvents(func(ev Event) { got = append(got, ev) })
	for _, kind := range []string{"text", "turn_end"} {
		if err := publishOMPAnswer(a, omp.Event{Type: kind, TurnID: "owner-turn-42", RequestID: "retry-99"}, Event{Type: "assistant", Text: kind}); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 {
		t.Fatalf("published %d events, want 2", len(got))
	}
	for _, ev := range got {
		if ev.TurnID != "owner-turn-42" {
			t.Errorf("TurnID = %q, want sidecar turn ID", ev.TurnID)
		}
	}
}

func TestOMPAnswerMissingTurnIDRefusesPayload(t *testing.T) {
	for _, kind := range []string{"text", "turn_end"} {
		t.Run(kind, func(t *testing.T) {
			a := &Agent{eventSubs: make(map[int64]EventFunc)}
			var got []Event
			a.SubscribeEvents(func(ev Event) { got = append(got, ev) })
			err := publishOMPAnswer(a, omp.Event{Type: kind, RequestID: "not-a-turn"}, Event{Type: "assistant", Text: "secret answer"})
			if !errors.Is(err, ErrOMPMissingTurnID) {
				t.Fatalf("error = %v, want ErrOMPMissingTurnID", err)
			}
			if len(got) != 1 || !got[0].IsError || got[0].StopReason != "end_turn" || !strings.Contains(got[0].Text, ErrOMPMissingTurnID.Error()) {
				t.Fatalf("refusal events = %+v", got)
			}
			if strings.Contains(got[0].Text, "secret answer") {
				t.Fatal("missing-identity answer broadcast")
			}
		})
	}
}
