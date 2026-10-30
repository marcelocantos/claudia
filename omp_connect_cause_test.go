// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"strings"
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
		"Connect error unauthenticated: token expired":         CauseAuth,
		"Connect error unavailable: upstream down":             CauseBackendUnavailable,
		"Connect error internal: boom":                         "",
		"the model said resource_exhausted in prose":           "",
		"": "",
	} {
		if got := CauseFromConnectError(in); got != want {
			t.Errorf("CauseFromConnectError(%q) = %q, want %q", in, got, want)
		}
	}
}

// 🎯T242: Anthropic puts its error type in parentheses, not after
// "Connect error ". The 2026-10-31 09:12 specimen must be rate_limit,
// never client_bug. Prose, malformed, unknown, and non-rate-limit codes
// must not spuriously map.
func TestCauseFromConnectErrorAnthropicStream(t *testing.T) {
	const specimen = `Anthropic stream error (rate_limit_error): Rate limited`
	cases := []struct {
		in   string
		want ProviderCause
	}{
		{specimen, CauseRateLimit},
		{`Anthropic stream error (authentication_error): invalid x-api-key`, CauseAuth},
		{`Anthropic stream error (overloaded_error): overloaded`, CauseBackendUnavailable},
		{`Anthropic stream error (invalid_request_error): bad request`, ""},
		{`Anthropic stream error (not_a_real_code): boom`, ""},
		{`Anthropic stream error (): Rate limited`, ""},
		{`Anthropic stream error (rate_limit_error) Rate limited`, ""},
		{`Anthropic stream error (rate_limit_error)`, ""},
		{`Anthropic stream error (rate_limit_error) : Rate limited`, ""},
		{`Anthropic stream error (rate_limit_error extra): Rate limited`, ""},
		{`the model said Anthropic stream error (rate_limit_error): Rate limited`, ""},
		{`Anthropic stream error (rate_limit_error): prose Connect error unavailable: upstream down`, CauseRateLimit},
		{`Anthropic stream error (rate_limit_error`, ""},
		{`Anthropic stream error rate_limit_error: Rate limited`, ""},
		{`the model said rate_limit_error in prose`, ""},
		{`Rate limited because of rate_limit_error`, ""},
		{`Connect error resource_exhausted: Error [details: …]`, CauseRateLimit},
		{`Connect error unauthenticated: token expired`, CauseAuth},
	}
	for _, tc := range cases {
		got := CauseFromConnectError(tc.in)
		if got != tc.want {
			t.Errorf("CauseFromConnectError(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.Contains(tc.in, "rate_limit_error") && !strings.HasPrefix(tc.in, "Anthropic stream error (rate_limit_error)") {
			if got == CauseRateLimit {
				t.Errorf("prose/malformed %q mapped to rate_limit", tc.in)
			}
		}
		if got == CauseClientBug {
			t.Errorf("CauseFromConnectError(%q) = client_bug", tc.in)
		}
	}
}

// Production sidecar turn_end: error field plus snapshot errorMessage.
// A Claude subscription seat only so the fake sidecar needs no Anthropic
// login; the pump's refusal path is the same.
func TestOMPAnthropicRateLimitErrorIsRateLimit(t *testing.T) {
	const msg = `Anthropic stream error (rate_limit_error): Rate limited`
	reason, errs := lastOMPRefusalReason(t, "anthropic-rate-seat", msg)
	if reason != string(CauseRateLimit) {
		t.Fatalf("error events %+v; want Reason %q, never %q", errs, CauseRateLimit, CauseClientBug)
	}
}

func TestOMPAnthropicRateLimitProseIsNotRateLimit(t *testing.T) {
	reason, errs := lastOMPRefusalReason(t, "anthropic-prose-seat", `the model said rate_limit_error in prose`)
	if reason == string(CauseRateLimit) || reason == string(CauseClientBug) {
		t.Fatalf("error events %+v; prose must not be rate_limit or client_bug", errs)
	}
}

func TestOMPAnthropicMalformedRefusalsAreNotRateLimit(t *testing.T) {
	for _, tc := range []struct {
		name, msg string
	}{
		{"embedded-prose", `the model said Anthropic stream error (rate_limit_error): Rate limited`},
		{"missing-colon", `Anthropic stream error (rate_limit_error) Rate limited`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, errs := lastOMPRefusalReason(t, "anthropic-"+tc.name+"-seat", tc.msg)
			if reason == string(CauseRateLimit) || reason == string(CauseClientBug) {
				t.Fatalf("error events %+v; malformed/prose refusal must not be rate_limit or client_bug", errs)
			}
		})
	}
}

func TestOMPAnthropicNonRateLimitIsNotRateLimit(t *testing.T) {
	reason, errs := lastOMPRefusalReason(t, "anthropic-auth-seat", `Anthropic stream error (authentication_error): invalid x-api-key`)
	if reason == string(CauseRateLimit) {
		t.Fatalf("error events %+v; authentication_error must not be rate_limit", errs)
	}
	if reason != string(CauseAuth) {
		t.Fatalf("error events %+v; want Reason %q", errs, CauseAuth)
	}
}

func lastOMPRefusalReason(t *testing.T, name, msg string) (string, []Event) {
	t.Helper()
	startFakeSidecar(t, []string{ompTurnEndRefusal(msg)})
	ctx := context.Background()
	agent, err := startDirectContext(ctx, Config{
		Provider: SubscriptionSeatProvider(ProviderClaude), Name: name,
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
	if len(errs) == 0 {
		t.Fatal("no error events")
	}
	return errs[len(errs)-1].Reason, errs
}

func ompTurnEndRefusal(msg string) string {
	b, err := json.Marshal(msg)
	if err != nil {
		panic(err)
	}
	s := string(b)
	return `{"type":"turn_end","error":` + s + `,"snapshot":{"messages":[{"role":"assistant","stopReason":"error","errorMessage":` + s + `}]}}`
}

// A test binary refuses the interactive login verb before running bun, so no
// approval screen reaches the owner (2026-10-27).
func TestExecBunLoginRefusesInteractiveLoginInTests(t *testing.T) {
	_, err := execBunLogin(t.Context(), "bun", sidecarAuthScript(), "login", "cursor")
	if err == nil || !strings.Contains(err.Error(), "never starts an interactive plan login") {
		t.Fatalf("interactive login in a test binary: err=%v", err)
	}
}
