// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/marcelocantos/claudia/internal/broker"
)

func TestAuthRecoverBrokerRequestKeepsErrorsAndRejectsUnknownPlans(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	_ = rawSeat(t, f.sock, "healthy")
	called := 0
	f.d.authRecover = func(_ context.Context, provider string) error {
		called++
		if provider != "anthropic" {
			t.Fatalf("provider = %q", provider)
		}
		if called == 1 {
			return errors.New("sign-in was cancelled")
		}
		return nil
	}
	c, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	response := rawCall(t, c, &broker.Request{ID: "recover", Type: broker.TypeAuthRecover,
		AuthRecover: &broker.NamedRequest{Name: "anthropic"}})
	if response.Type != broker.TypeError || response.Error.Code != broker.CodeAgentFailed ||
		response.Error.Message != "sign-in was cancelled" || called != 1 {
		t.Fatalf("recovery response=%+v called=%d", response, called)
	}
	response = rawCall(t, c, &broker.Request{ID: "unknown", Type: broker.TypeAuthRecover,
		AuthRecover: &broker.NamedRequest{Name: "not-a-plan"}})
	if response.Type != broker.TypeError || response.Error.Code != broker.CodeUnsupportedValue || called != 1 {
		t.Fatalf("unsupported provider response=%+v called=%d", response, called)
	}
	response = rawCall(t, c, &broker.Request{ID: "retry", Type: broker.TypeAuthRecover,
		AuthRecover: &broker.NamedRequest{Name: "anthropic"}})
	if response.Type != broker.TypeAuthRecovered || response.AuthRecovered.Name != "anthropic" || called != 2 {
		t.Fatalf("retry response=%+v called=%d", response, called)
	}
	if !f.owned("healthy") || f.seat(0) == nil {
		t.Fatalf("auth recovery disturbed an unrelated live seat: owned=%v seat=%v", f.owned("healthy"), f.seat(0))
	}
}
