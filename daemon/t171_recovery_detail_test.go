// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

func TestT171DetailedRecoveryDispatch(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	calls := 0
	f.d.authRecover = func(context.Context, *claudia.OMPPlanRecovery) error {
		t.Error("new operation called legacy interactive seam")
		return nil
	}
	f.d.authRecoverDetail = func(_ context.Context, p string) claudia.OMPRecoveryResult {
		calls++
		if f.d.reauthMu.TryLock() {
			f.d.reauthMu.Unlock()
			t.Error("recovery ran outside daemon lock")
		}
		return claudia.OMPRecoveryResult{Provider: p, Outcome: "healthy_no_op", Classification: "none"}
	}
	c, err := broker.Dial(f.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	call := func(provider string) *broker.Response {
		return rawCall(t, c, &broker.Request{ID: "detail", Type: broker.TypeAuthRecoverDetail, AuthDetail: &broker.NamedRequest{Name: provider}})
	}
	got := call("anthropic")
	if got.AuthDetail == nil || got.AuthDetail.Outcome != "healthy_no_op" || calls != 1 {
		t.Fatalf("result %+v; calls %d", got, calls)
	}
	got = call("unknown")
	if got.AuthDetail == nil || got.AuthDetail.Classification != "unsupported_provider" || calls != 1 {
		t.Fatal("unknown provider reached recovery")
	}
	f.d.reauthMu.Lock()
	got = call("cursor")
	f.d.reauthMu.Unlock()
	if got.AuthDetail == nil || got.AuthDetail.Classification != "busy" || calls != 1 {
		t.Fatal("concurrent recovery not refused")
	}
}
