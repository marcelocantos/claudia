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
	var fragments ompFragmentSequence
	a.SubscribeEvents(func(ev Event) { got = append(got, ev) })
	for _, kind := range []string{"text", "turn_end"} {
		if err := publishOMPAnswer(a, omp.Event{Type: kind, TurnID: "owner-turn-42", RequestID: "retry-99"}, Event{Type: "assistant", Text: kind}, &fragments); err != nil {
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
			var fragments ompFragmentSequence
			a.SubscribeEvents(func(ev Event) { got = append(got, ev) })
			err := publishOMPAnswer(a, omp.Event{Type: kind, RequestID: "not-a-turn"}, Event{Type: "assistant", Text: "secret answer"}, &fragments)
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

func TestOMPAnswerFinalFragmentSequenceResetsPerTurn(t *testing.T) {
	a := &Agent{eventSubs: make(map[int64]EventFunc)}
	var got []Event
	a.SubscribeEvents(func(ev Event) { got = append(got, ev) })
	var fragments ompFragmentSequence
	for _, step := range []struct {
		turn, kind string
		seq        int
		final      bool
	}{
		{"turn-one", "text", 1, false},
		{"turn-one", "text", 2, false},
		{"turn-one", "text", 3, false},
		{"turn-one", "turn_end", 4, true},
		{"turn-two", "text", 1, false},
		{"turn-two", "text", 2, false},
		{"turn-two", "turn_end", 3, true},
	} {
		if err := publishOMPAnswer(a, omp.Event{Type: step.kind, TurnID: step.turn}, Event{Type: "assistant", Text: step.kind}, &fragments); err != nil {
			t.Fatal(err)
		}
		ev := got[len(got)-1]
		if ev.TurnID != step.turn || ev.FragmentSeq != step.seq || ev.Final != step.final {
			t.Errorf("%s %s: got (turn=%q seq=%d final=%t), want (%q %d %t)", step.turn, step.kind, ev.TurnID, ev.FragmentSeq, ev.Final, step.turn, step.seq, step.final)
		}
	}
	if len(got) != 7 {
		t.Fatalf("published %d events, want 7", len(got))
	}
}

func TestOMPAnswerErrorIsFinalAndMissingIDDoesNotAdvanceSequence(t *testing.T) {
	a := &Agent{eventSubs: make(map[int64]EventFunc)}
	var got []Event
	a.SubscribeEvents(func(ev Event) { got = append(got, ev) })
	var fragments ompFragmentSequence
	if err := publishOMPAnswer(a, omp.Event{Type: "text", TurnID: " "}, Event{Type: "assistant", Text: "not an answer"}, &fragments); !errors.Is(err, ErrOMPMissingTurnID) {
		t.Fatalf("missing ID: %v", err)
	}
	if got[0].Final || got[0].FragmentSeq != 0 {
		t.Fatalf("missing ID refusal gained fragment metadata: %+v", got[0])
	}
	if err := publishOMPAnswer(a, omp.Event{Type: "error", TurnID: "failed-turn"}, Event{Type: "assistant", IsError: true}, &fragments); err != nil {
		t.Fatal(err)
	}
	if !got[1].Final || got[1].FragmentSeq != 1 {
		t.Fatalf("error terminal event = %+v, want final seq=1", got[1])
	}
}
