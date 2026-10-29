// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"strings"
	"testing"
)

// 🎯T236: an older sidecar that ended a turn as empty end_turn after an
// unanswered function call must still surface as WaitForResponse error,
// never a successful empty reply.
func TestT236EmptyTurnEndAfterUnpairedToolIsError(t *testing.T) {
	snap := `{"messages":[` +
		`{"role":"user","content":"hold"},` +
		`{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"Bash"}],"stopReason":"toolUse"},` +
		`{"role":"assistant","content":[{"type":"text","text":""}],"stopReason":"stop"}` +
		`]}`
	startFakeSidecar(t, []string{`{"type":"turn_end","snapshot":` + snap + `}`})
	ctx := context.Background()
	agent, err := startDirectContext(ctx, Config{
		Provider: SubscriptionSeatProvider(ProviderClaude), Name: "t236-seat",
		Model: "m", WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Stop()
	if err := agent.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if err := agent.Send("Reply with exactly: X"); err != nil {
		t.Fatal(err)
	}
	reply, err := agent.WaitForResponse(ctx)
	if err == nil || reply != "" {
		t.Fatalf("WaitForResponse = %q, %v; want an error and no reply", reply, err)
	}
	if !strings.Contains(err.Error(), "unanswered function call") {
		t.Fatalf("err = %v; want unanswered function call", err)
	}
}
