// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 🎯T940: with CLAUDIA_OMP_NO_REFRESH set, fetch must never spawn the login
// helper — that helper is what actually contacts the OAuth provider and
// consumes (rotates) the refresh token. An isolated journey broker sets this
// so it can never invalidate the shared refresh token a production broker
// also holds, even when it races a real broker's own refresh.
func TestT940NoRefreshEnvRefusesWithoutContactingTheProvider(t *testing.T) {
	t.Setenv(NoNetworkEnv, "1")
	called := false
	login := Login{Run: func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}}
	existing := Record{RefreshToken: "shared-refresh-token", AccessToken: "stale", Expiry: time.Now().Add(-time.Hour)}
	_, err := login.fetch(context.Background(), Anthropic, existing)
	if err == nil {
		t.Fatal("fetch succeeded with CLAUDIA_OMP_NO_REFRESH set; want a refusal")
	}
	if called {
		t.Fatal("fetch spawned the login helper (would contact the OAuth provider) despite CLAUDIA_OMP_NO_REFRESH")
	}
}

// Unset (the production broker's shape), fetch runs the helper as before.
func TestT940NoRefreshEnvUnsetStillRefreshes(t *testing.T) {
	called := false
	login := Login{Script: "sidecar/auth.ts", Run: func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return []byte(`{"access_token":"new","refresh_token":"new-r","expiry":"` +
			time.Now().Add(time.Hour).Format(time.RFC3339) + `"}`), nil
	}}
	existing := Record{RefreshToken: "shared-refresh-token", AccessToken: "stale", Expiry: time.Now().Add(-time.Hour)}
	rec, err := login.fetch(context.Background(), Anthropic, existing)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !called {
		t.Fatal("fetch did not call the login helper; the control case must still refresh")
	}
	if rec.AccessToken != "new" {
		t.Fatalf("rec = %+v, want the refreshed record", rec)
	}
}

// The whole point: a bulk RefreshPlans pass (what serve runs at startup) on
// an isolated store must leave the shared refresh token completely alone —
// not read, not sent anywhere, not rotated — when the env is set.
func TestT940RefreshPlansNeverTouchesProviderWhenIsolated(t *testing.T) {
	t.Setenv(NoNetworkEnv, "1")
	resetKeychainShot()
	t.Cleanup(resetKeychainShot)
	store := Store{DataPath: t.TempDir() + "/plan.enc", Run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("SecItemNotFound: The specified item could not be found in the keychain.")
	}}
	if err := Open(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), Item{Records: map[string]Record{
		Anthropic: {RefreshToken: "shared-refresh-token", AccessToken: "stale", Expiry: time.Now().Add(-time.Hour)},
	}}); err != nil {
		t.Fatal(err)
	}
	called := false
	login := Login{Run: func(context.Context, string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}}
	refreshed, _, err := RefreshPlans(context.Background(), store, login)
	if called {
		t.Fatal("RefreshPlans contacted the OAuth provider despite CLAUDIA_OMP_NO_REFRESH")
	}
	if len(refreshed) != 0 {
		t.Fatalf("refreshed = %v, want none", refreshed)
	}
	if err == nil {
		t.Fatal("want the isolated refusal reported as an error, not silently skipped")
	}
}
