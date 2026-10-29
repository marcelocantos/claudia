// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import "testing"

func TestT236EmptyTurnEndAfterUnpairedToolIsRefusal(t *testing.T) {
	snap := `{"messages":[` +
		`{"role":"user","content":"hold"},` +
		`{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"Bash"}],"stopReason":"toolUse"},` +
		`{"role":"assistant","content":[{"type":"text","text":""}],"stopReason":"stop"}` +
		`]}`
	ev := Event{Type: "turn_end", Snapshot: []byte(snap)}
	got := ev.Refusal()
	if got != "provider rejected an unanswered function call" {
		t.Fatalf("Refusal = %q", got)
	}

	aborted := Event{Type: "turn_end", StopReason: "aborted", Snapshot: []byte(snap)}
	if aborted.Refusal() != "" {
		t.Fatalf("aborted Refusal = %q", aborted.Refusal())
	}

	paired := Event{Type: "turn_end", Snapshot: []byte(`{"messages":[` +
		`{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"Bash"}],"stopReason":"toolUse"},` +
		`{"role":"toolResult","toolCallId":"c1","content":[{"type":"text","text":"aborted"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":""}],"stopReason":"stop"}` +
		`]}`)}
	if paired.Refusal() != "" {
		t.Fatalf("paired empty Refusal = %q", paired.Refusal())
	}

	withError := Event{Type: "turn_end", Error: "No tool output found for function call c1"}
	if withError.Refusal() != withError.Error {
		t.Fatalf("Error field Refusal = %q", withError.Refusal())
	}
}
