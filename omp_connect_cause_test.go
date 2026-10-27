// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"sync"
	"testing"
)

// Cursor reports its quota refusal through the OMP sidecar as a turn whose
// assistant message has stopReason "error" and a Connect errorMessage. The
// specimen is jevons-po's failed turn of 2026-10-27T06:21:31Z, byte for byte
// in the error text. The answer event must carry a typed cause so Jevons
// raises a provider wall instead of reading an untyped failure. The seat is a
// Claude subscription seat only so the fake sidecar needs no plan token (a
// cursor seat starts a real login); the pump's refusal path is the same.
func TestOMPCursorResourceExhaustedIsRateLimit(t *testing.T) {
	const msg = `Connect error resource_exhausted: Error [details: aiserver.v1.ErrorDetails: {\"error\":\"ERROR_RESOURCE_EXHAUSTED\",\"details\":{\"title\":\"Unable to reach the model provider\",\"detail\":\"We're having trouble connecting to the model provider. This might be temporary - please try again in a moment.\"},\"isExpected\":true}]`
	reply := `{"type":"turn_end","snapshot":{"messages":[{"role":"assistant","stopReason":"error","errorMessage":"` + msg + `"}]}}`
	startFakeSidecar(t, []string{reply})
	ctx := context.Background()
	agent, err := startDirectContext(ctx, Config{
		Provider: SubscriptionSeatProvider(ProviderClaude), Name: "cursor-wall-seat",
		Model: "m", WorkDir: t.TempDir(), TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Stop()
	if err := agent.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var errs []Event
	agent.SubscribeEvents(func(ev Event) {
		if ev.IsError {
			mu.Lock()
			errs = append(errs, ev)
			mu.Unlock()
		}
	})
	if err := agent.Send("spawn the worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.WaitForResponse(ctx); err == nil {
		t.Fatal("a refused turn returned no error")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(errs) == 0 || errs[len(errs)-1].Reason != string(CauseRateLimit) {
		t.Fatalf("error events %+v; want Reason %q", errs, CauseRateLimit)
	}
}

func TestCauseFromConnectError(t *testing.T) {
	for in, want := range map[string]ProviderCause{
		"Connect error resource_exhausted: Error [details: …]": CauseRateLimit,
		"Connect error unauthenticated: token expired":          CauseAuth,
		"Connect error unavailable: upstream down":              CauseBackendUnavailable,
		"Connect error internal: boom":                          "",
		"the model said resource_exhausted in prose":            "",
		"":                                                       "",
	} {
		if got := CauseFromConnectError(in); got != want {
			t.Errorf("CauseFromConnectError(%q) = %q, want %q", in, got, want)
		}
	}
}
