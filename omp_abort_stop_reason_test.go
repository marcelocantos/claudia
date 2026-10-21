// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"testing"
)

// An aborted turn ends with no reply and says how in Reason. On 2026-10-21
// the sidecar sent the word "aborted" as the turn_end's text, and a consumer
// painted it as the overseer's answer to the owner's next message. The turn
// still ends with StopReason "end_turn", so waiters see it finish.
func TestAbortedTurnEndsWithoutTextAndSaysSoInReason(t *testing.T) {
	for _, tc := range []struct {
		name, reply, wantReason string
	}{
		{"aborted", `{"type":"turn_end","stop_reason":"aborted","snapshot":{"messages":[]}}`, ReasonAborted},
		{"normal end", `{"type":"turn_end","snapshot":{"messages":[]}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startFakeSidecar(t, []string{tc.reply})
			ctx := context.Background()
			agent, err := startDirectContext(ctx, Config{
				Provider: SubscriptionSeatProvider(ProviderClaude), Name: "abort-seat",
				Model: "m", WorkDir: t.TempDir(), TermLogPath: "-",
			})
			if err != nil {
				t.Fatal(err)
			}
			defer agent.Stop()
			if err := agent.WaitReady(ctx); err != nil {
				t.Fatal(err)
			}
			finals := make(chan Event, 4)
			agent.SubscribeEvents(func(ev Event) {
				if ev.Type == "assistant" && ev.Final {
					finals <- ev
				}
			})
			if err := agent.Send("hold"); err != nil {
				t.Fatal(err)
			}
			// No wall clock: go test -timeout bounds the wait (🎯T97).
			ev := <-finals
			if ev.Text != "" {
				t.Fatalf("final event Text = %q; an ended turn with no reply must carry no text", ev.Text)
			}
			if !ev.IsTerminalStop() {
				t.Fatalf("final event StopReason = %q; the turn must still end for every waiter", ev.StopReason)
			}
			if ev.Reason != tc.wantReason {
				t.Fatalf("final event Reason = %q, want %q", ev.Reason, tc.wantReason)
			}
			if ev.IsError {
				t.Fatal("an aborted turn is not an error")
			}
		})
	}
}
