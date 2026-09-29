// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// 🎯T155: every refresh a broker makes is saved before it returns. A
// refresh spends the refresh token it replaces; on 2026-09-30 a broker saved
// only its first, and the next broker read a spent token and every
// Anthropic seat failed invalid_grant.
func TestT155EveryRefreshIsSavedForTheNextProcess(t *testing.T) {
	resetKeychainShot()
	t.Cleanup(resetKeychainShot)
	path := filepath.Join(t.TempDir(), "claudia", "plan.enc")
	kc := &fakeKeychain{value: mustJSON(Item{Records: map[string]Record{
		Anthropic: {AccessToken: "a0", RefreshToken: "r0", Expiry: time.Now().Add(-time.Minute)},
	}})}
	s := kc.store(t, path)
	ctx := context.Background()
	if err := Open(ctx, s); err != nil {
		t.Fatal(err)
	}
	n := 0
	login := Login{Script: "auth.ts", ForceRefresh: true, Run: func(context.Context, string, ...string) ([]byte, error) {
		n++
		return []byte(fmt.Sprintf(`{"refresh_token":"r%d","access_token":"a%d","expiry":"%s"}`,
			n, n, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))), nil
	}}
	// Two refreshes in one broker's life: a seat launch, then a seat whose
	// token was rejected. No shutdown flush follows.
	for i := 0; i < 2; i++ {
		if _, err := login.Refresh(ctx, s, Anthropic); err != nil {
			t.Fatal(err)
		}
	}

	// The next broker reads the store.
	resetKeychainShot()
	if err := Open(ctx, s); err != nil {
		t.Fatal(err)
	}
	item, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := item.Records[Anthropic].RefreshToken; got != "r2" {
		t.Fatalf("the next process reads refresh token %q, want the last one, r2", got)
	}
}
