// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

func TestAuthRecoverBrokerRequestKeepsErrorsAndRejectsUnknownPlans(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	_ = rawSeat(t, f.sock, "healthy")
	called := 0
	var logins []bool
	f.d.authRecover = func(_ context.Context, r *claudia.OMPPlanRecovery) error {
		called++
		if r.Plan != "anthropic" {
			t.Fatalf("plan = %q", r.Plan)
		}
		logins = append(logins, r.Login)
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
		AuthRecover: &broker.AuthRecoverRequest{Name: "anthropic"}})
	if response.Type != broker.TypeError || response.Error.Code != broker.CodeAgentFailed ||
		response.Error.Message != "sign-in was cancelled" || called != 1 {
		t.Fatalf("recovery response=%+v called=%d", response, called)
	}
	response = rawCall(t, c, &broker.Request{ID: "unknown", Type: broker.TypeAuthRecover,
		AuthRecover: &broker.AuthRecoverRequest{Name: "not-a-plan"}})
	if response.Type != broker.TypeError || response.Error.Code != broker.CodeUnsupportedValue || called != 1 {
		t.Fatalf("unsupported provider response=%+v called=%d", response, called)
	}
	response = rawCall(t, c, &broker.Request{ID: "retry", Type: broker.TypeAuthRecover,
		AuthRecover: &broker.AuthRecoverRequest{Name: "anthropic", Login: true}})
	if response.Type != broker.TypeAuthRecovered || response.AuthRecovered.Name != "anthropic" || called != 2 {
		t.Fatalf("retry response=%+v called=%d", response, called)
	}
	// 🎯T165: a request that does not say a person is at the keyboard may
	// not open a sign-in; one that does passes that through.
	if len(logins) != 2 || logins[0] || !logins[1] {
		t.Fatalf("login permission reaching the recovery = %v, want [false true]", logins)
	}
	if !f.owned("healthy") || f.seat(0) == nil {
		t.Fatalf("auth recovery disturbed an unrelated live seat: owned=%v seat=%v", f.owned("healthy"), f.seat(0))
	}
}

// 🎯T924: auth_status answers from the store reader and never starts a
// recovery.
func TestT924AuthStatusReportsPlanHealthWithoutRecovering(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	f.d.authRecover = func(context.Context, *claudia.OMPPlanRecovery) error {
		t.Fatal("auth_status started a recovery")
		return nil
	}
	f.d.authStatus = func(context.Context) ([]claudia.PlanLoginHealth, error) {
		return []claudia.PlanLoginHealth{{Provider: "anthropic", State: "ok"}, {Provider: "cursor", State: "missing"}}, nil
	}
	c, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	response := rawCall(t, c, &broker.Request{ID: "status", Type: broker.TypeAuthStatus, AuthStatus: &broker.AuthStatusRequest{}})
	if response.Type != broker.TypeAuthStatusResult || len(response.AuthStatus.Plans) != 2 ||
		response.AuthStatus.Plans[1] != (broker.PlanAuth{Provider: "cursor", State: "missing"}) {
		t.Fatalf("auth_status response=%+v", response)
	}
}
