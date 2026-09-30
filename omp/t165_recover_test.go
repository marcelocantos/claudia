// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// t165Store is an opened plan store holding one anthropic record.
func t165Store(t *testing.T, access string, expiry time.Time) Store {
	t.Helper()
	ResetKeychainShot()
	t.Cleanup(ResetKeychainShot)
	t.Cleanup(func() { clearRejected(Anthropic) })
	blob := `{"records":{"anthropic":{"refresh_token":"r1","access_token":"` + access + `","expiry":"` + expiry.UTC().Format(time.RFC3339) + `"}}}`
	store := Store{BrokerPath: "/test/claudia", DataPath: filepath.Join(t.TempDir(), "plan.enc"), Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "find-generic-password" {
			return []byte(blob), nil
		}
		return nil, nil
	}}
	if err := Open(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	return store
}

// t165Login records each pi-ai verb and answers refresh with refresh.
func t165Login(verbs *[]string, refresh func() ([]byte, error)) Login {
	return Login{Script: "auth.ts", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		*verbs = append(*verbs, args[1])
		if args[1] == "refresh" {
			return refresh()
		}
		return []byte(`{"refresh_token":"login","access_token":"login","expiry":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`), nil
	}}
}

// 🎯T165: a plan whose login is healthy has nothing to repair. Another
// refresh already replaced the token that was refused; refreshing again
// would rotate it under every seat on the plan.
func TestT165RecoverLeavesAHealthyPlanAlone(t *testing.T) {
	store := t165Store(t, "live", time.Now().Add(time.Hour))
	var verbs []string
	login := t165Login(&verbs, func() ([]byte, error) { return nil, errors.New("must not refresh") })
	for _, interactive := range []bool{false, true} {
		login.NoLogin = !interactive
		if err := RecoverPlan(context.Background(), store, login, Anthropic); err != nil {
			t.Fatal(err)
		}
	}
	if len(verbs) != 0 {
		t.Fatalf("a healthy plan was repaired: %v", verbs)
	}
}

// 🎯T165: an unattended repair whose refresh is refused never opens a
// sign-in. It marks the plan rejected for the owner's Reauth and says so.
func TestT165UnattendedRecoverNeverSignsIn(t *testing.T) {
	store := t165Store(t, "revoked", time.Now().Add(time.Hour))
	MarkRejected(Anthropic, "OAuth access token has been revoked")
	var verbs []string
	login := t165Login(&verbs, func() ([]byte, error) {
		return nil, errors.New(`anthropic token refresh failed: 400 {"error": "invalid_grant"}`)
	})
	login.NoLogin = true
	err := RecoverPlan(context.Background(), store, login, Anthropic)
	if !errors.Is(err, ErrNeedsSignIn) {
		t.Fatalf("RecoverPlan = %v, want ErrNeedsSignIn", err)
	}
	if len(verbs) != 1 || verbs[0] != "refresh" {
		t.Fatalf("verbs = %v, want one refresh and no login", verbs)
	}
	health, err := Health(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range health {
		if h.Provider == Anthropic && h.State != HealthRejected {
			t.Fatalf("anthropic health = %+v, want rejected", h)
		}
	}

	// The owner at the keyboard may sign in.
	verbs = nil
	login.NoLogin = false
	if err := RecoverPlan(context.Background(), store, login, Anthropic); err != nil {
		t.Fatal(err)
	}
	if len(verbs) != 2 || verbs[1] != "login" {
		t.Fatalf("verbs = %v, want refresh then login", verbs)
	}
}

// A fetch that would need a sign-in refuses outright under NoLogin, whatever
// the caller: a plan with no refresh token cannot be refreshed.
func TestT165NoLoginRefusesASignInFetch(t *testing.T) {
	var verbs []string
	login := t165Login(&verbs, nil)
	login.NoLogin = true
	if _, err := login.fetch(context.Background(), Anthropic, Record{}); !errors.Is(err, ErrNeedsSignIn) {
		t.Fatalf("fetch = %v, want ErrNeedsSignIn", err)
	}
	if len(verbs) != 0 {
		t.Fatalf("pi-ai ran %v", verbs)
	}
}

// 🎯T165: a successful refresh answers a rejection, so the next recovery
// finds the plan healthy instead of rotating it again.
func TestT165RefreshClearsTheRejection(t *testing.T) {
	store := t165Store(t, "revoked", time.Now().Add(time.Hour))
	MarkRejected(Anthropic, "OAuth access token has been revoked")
	var verbs []string
	login := t165Login(&verbs, func() ([]byte, error) {
		return []byte(`{"refresh_token":"r2","access_token":"fresh","expiry":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`), nil
	})
	login.ForceRefresh = true
	login.NoLogin = true
	if _, err := login.Refresh(context.Background(), store, Anthropic); err != nil {
		t.Fatal(err)
	}
	if err := RecoverPlan(context.Background(), store, login, Anthropic); err != nil {
		t.Fatal(err)
	}
	if len(verbs) != 1 {
		t.Fatalf("verbs = %v, want the one seat refresh and no second rotation", verbs)
	}
}
