// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// 🎯T924: every plan's login health comes from the store alone. A saved
// login that renews is ok; no login is missing; a dead access token with
// nothing to renew it is expired; a login the provider refused stays
// rejected until a new one is saved.
func TestT924PlanHealthReadsTheStoreAndNeverLogsIn(t *testing.T) {
	ResetKeychainShot()
	t.Cleanup(ResetKeychainShot)
	t.Cleanup(func() { clearRejected(XAIOAuth) })
	clock := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	store := Store{BrokerPath: "/test/claudia", DataPath: filepath.Join(t.TempDir(), "plan.enc"),
		Now: func() time.Time { return clock },
		Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if args[0] == "find-generic-password" {
				return []byte(`{"records":{` +
					`"anthropic":{"refresh_token":"r","access_token":"a","expiry":"2026-09-29T11:00:00Z"},` +
					`"openai-codex":{"refresh_token":"undefined","access_token":"a","expiry":"2026-09-29T11:00:00Z"},` +
					`"xai-oauth":{"refresh_token":"r","access_token":"a","expiry":"2026-09-29T13:00:00Z"}}}`), nil
			}
			return nil, nil
		}}
	if err := Open(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	MarkRejected(XAIOAuth, "refresh failed:\n invalid_grant")

	got := healthByPlan(t, store)
	want := map[string]string{Anthropic: HealthOK, OpenAICodex: HealthExpired, Cursor: HealthMissing, XAIOAuth: HealthRejected}
	for id, state := range want {
		if got[id].State != state {
			t.Errorf("%s: state %q, want %q (%+v)", id, got[id].State, state, got[id])
		}
	}
	if got[XAIOAuth].Detail != "refresh failed: invalid_grant" || got[XAIOAuth].Since.IsZero() {
		t.Errorf("rejection carries no one-line cause or time: %+v", got[XAIOAuth])
	}

	// The owner's Reauth saves a new login: the rejection is gone.
	if err := store.Put(context.Background(), XAIOAuth, Record{RefreshToken: "r2", AccessToken: "a2", Expiry: clock.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if s := healthByPlan(t, store)[XAIOAuth].State; s != HealthOK {
		t.Fatalf("after a new login xai-oauth is %q, want ok", s)
	}
}

func healthByPlan(t *testing.T, store Store) map[string]PlanHealth {
	t.Helper()
	plans, err := Health(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != len(PlanIDs) {
		t.Fatalf("%d plans reported, want %d: %+v", len(plans), len(PlanIDs), plans)
	}
	out := map[string]PlanHealth{}
	for _, p := range plans {
		out[p.Provider] = p
	}
	return out
}
