// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"testing"
	"time"
)

// An aborted turn ends with no reply and says so in StopReason. On
// 2026-10-21 the sidecar sent the word "aborted" as the turn_end's text, and
// a consumer painted it as the overseer's answer to the owner's next message.
func TestAbortedTurnEndsWithoutTextAndSaysSoInStopReason(t *testing.T) {
	for _, tc := range []struct {
		name, reply, wantStop string
	}{
		{"aborted", `{"type":"turn_end","stop_reason":"aborted","snapshot":{"messages":[]}}`, StopReasonAborted},
		{"normal end", `{"type":"turn_end","snapshot":{"messages":[]}}`, "end_turn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startFakeSidecar(t, []string{tc.reply})
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
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
			select {
			case ev := <-finals:
				if ev.Text != "" {
					t.Fatalf("final event Text = %q; an ended turn with no reply must carry no text", ev.Text)
				}
				if ev.StopReason != tc.wantStop {
					t.Fatalf("final event StopReason = %q, want %q", ev.StopReason, tc.wantStop)
				}
				if ev.IsError {
					t.Fatal("an aborted turn is not an error")
				}
			case <-ctx.Done():
				t.Fatal("no final assistant event")
			}
		})
	}
}
