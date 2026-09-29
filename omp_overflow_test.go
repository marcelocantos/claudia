// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 🎯T148 (jevons 🎯T926/T927): a seat whose provider refused the turn as
// longer than the model's window, after the sidecar's own compaction and
// single retry, reports a terminal error a caller can tell apart from a
// transient refusal — so it stops resending, instead of retrying every 30s
// with the prompt growing each time. Any other refusal stays as it was.
func TestT148ContextOverflowIsTerminal(t *testing.T) {
	const overflow = "400 prompt is too long: 1058579 tokens > 1000000 maximum"
	const limit = "429 usage_limit_reached: weekly limit resets 2026-10-03T23:20Z"
	for _, tc := range []struct {
		name     string
		reply    string
		terminal bool
		wantErr  string
	}{
		{
			name:     "overflow",
			reply:    `{"type":"turn_end","error":"` + overflow + `","reason":"context_overflow","snapshot":{"messages":[]}}`,
			terminal: true,
			wantErr:  "provider refused the turn: " + overflow,
		},
		{
			// The reason alone, on a turn that was not refused, is not an
			// overflow.
			name:  "reason without a refusal",
			reply: `{"type":"turn_end","reason":"context_overflow","snapshot":{"messages":[]}}`,
		},
		{
			name:    "usage limit",
			reply:   `{"type":"turn_end","error":"` + limit + `","snapshot":{"messages":[]}}`,
			wantErr: "provider refused the turn: " + limit,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startFakeTransferSidecar(t, []string{`{"type":"text","text":"x"}`, tc.reply})
			_, err := SummarizeForMigration(context.Background(), MigrationTransferArgs{
				Destination: ProviderClaude, Goal: "T148", Transcript: "user: finish T148\n",
			})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("SummarizeForMigration err = %v; want none", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("SummarizeForMigration err = %v; want it to contain %q", err, tc.wantErr)
			}
			if got := errors.Is(err, ErrContextOverflow); got != tc.terminal {
				t.Fatalf("errors.Is(err, ErrContextOverflow) = %v, want %v (err: %v)", got, tc.terminal, err)
			}
		})
	}
}

// The classification survives the broker hop: a consumer of a brokered seat
// reads the same Reason a direct one does.
func TestT148OverflowReasonCrossesBrokerWire(t *testing.T) {
	raw, err := EncodeEventWire(Event{Type: "assistant", Text: "provider refused the turn: x", IsError: true, StopReason: "end_turn", Reason: ReasonContextOverflow})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := DecodeEventWire(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.IsError || ev.Reason != ReasonContextOverflow {
		t.Fatalf("decoded %+v; want IsError and Reason %q", ev, ReasonContextOverflow)
	}
}
