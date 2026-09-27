// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRecoverPlanRetriesMissedKeychainReadThenReauthenticates(t *testing.T) {
	ResetKeychainShot()
	t.Cleanup(ResetKeychainShot)
	reads, writes := 0, 0
	stored := `{"records":{"anthropic":{"refresh_token":"old-refresh","access_token":"old-access","expiry":"2026-09-26T00:00:00Z"}}}`
	store := Store{BrokerPath: "/test/claudia", Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "security" {
			t.Fatalf("command = %q", name)
		}
		if len(args) > 0 && args[0] == "find-generic-password" {
			reads++
			if reads == 1 {
				return nil, errors.New("user interaction is not allowed")
			}
			return []byte(stored), nil
		}
		writes++
		return nil, nil
	}}
	if err := Open(context.Background(), store); err == nil {
		t.Fatal("first Keychain read unexpectedly succeeded")
	}
	var verbs []string
	login := Login{Script: "auth.ts", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		verbs = append(verbs, args[1])
		if args[1] == "refresh" {
			return nil, errors.New("invalid_grant: Refresh token not found or invalid")
		}
		return []byte(`{"refresh_token":"new-refresh","access_token":"new-access","expiry":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`), nil
	}}
	if err := RecoverPlan(context.Background(), store, login, Anthropic); err != nil {
		t.Fatal(err)
	}
	if reads != 2 || writes != 1 || strings.Join(verbs, ",") != "refresh,login" {
		t.Fatalf("reads=%d writes=%d verbs=%v", reads, writes, verbs)
	}
	item, err := store.Load(context.Background())
	if err != nil || item.Records[Anthropic].AccessToken != "new-access" {
		t.Fatalf("stored login = %+v, %v", item.Records[Anthropic], err)
	}
}

func TestRecoverPlanDoesNotRereadHealthyKeychainOrOpenLoginWhenRefreshWorks(t *testing.T) {
	ResetKeychainShot()
	t.Cleanup(ResetKeychainShot)
	reads := 0
	store := Store{BrokerPath: "/test/claudia", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "find-generic-password" {
			reads++
			return []byte(`{"records":{"anthropic":{"refresh_token":"old","access_token":"old","expiry":"2026-09-26T00:00:00Z"}}}`), nil
		}
		return nil, nil
	}}
	if err := Open(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	login := Login{Script: "auth.ts", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[1] != "refresh" {
			t.Fatalf("unexpected interactive %q", args[1])
		}
		return []byte(`{"refresh_token":"new","access_token":"new","expiry":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`), nil
	}}
	if err := RecoverPlan(context.Background(), store, login, Anthropic); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("healthy Keychain reread %d times", reads)
	}
}
