// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// damagedItem is the shape the split -X write left on 2026-09-28: the
// first byte is 0x07 instead of '{', the value stops inside the third
// record, and security -w prints it as hex because of that byte.
func damagedItem() string {
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	blob := `{"records":{` +
		`"anthropic":{"refresh_token":"keep-ra","access_token":"keep-aa","expiry":"` + exp + `"},` +
		`"cursor":{"refresh_token":"keep-rc","access_token":"keep-ac","expiry":"` + exp + `"},` +
		`"openai-codex":{"refresh_token":"lost-ro","access_token":"lost-a`
	b := []byte(blob)
	b[0] = 0x07
	return hex.EncodeToString(b)
}

func TestOpenRefusesDamagedItem(t *testing.T) {
	resetKeychainShot()
	t.Cleanup(resetKeychainShot)
	kc := &fakeKeychain{value: damagedItem()}
	err := Open(context.Background(), kc.store(t, filepath.Join(t.TempDir(), "plan.enc")))
	if err == nil || !strings.Contains(err.Error(), "not the plan blob") {
		t.Fatalf("startup read of a damaged item must fail loudly, err = %v", err)
	}
}

func TestReauthSalvagesDamagedItemAndReplacesIt(t *testing.T) {
	resetKeychainShot()
	t.Cleanup(resetKeychainShot)
	kc := &fakeKeychain{value: damagedItem()}
	s := kc.store(t, filepath.Join(t.TempDir(), "plan.enc"))
	ctx := context.Background()
	if err := Open(ctx, s); err == nil {
		t.Fatal("startup read unexpectedly accepted the damaged item")
	}
	var verbs []string
	login := Login{Script: "auth.ts", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		verbs = append(verbs, args[1]+" "+args[2])
		return []byte(`{"refresh_token":"new-rx","access_token":"new-ax","expiry":"` +
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`), nil
	}}
	if err := RecoverPlan(ctx, s, login, XAIOAuth); err != nil {
		t.Fatal(err)
	}
	if strings.Join(verbs, ",") != "login "+XAIOAuth {
		t.Fatalf("pi-ai calls = %v, want one xai login", verbs)
	}
	if kc.writes != 1 || !strings.Contains(kc.value, "data_key") {
		t.Fatalf("damaged item not replaced by a data key: writes=%d", kc.writes)
	}
	resetKeychainShot()
	if err := Open(ctx, s); err != nil {
		t.Fatalf("reopen after recovery: %v", err)
	}
	got, _ := s.Load(ctx)
	if got.Records[Anthropic].RefreshToken != "keep-ra" || got.Records[Cursor].RefreshToken != "keep-rc" {
		t.Fatalf("complete records were not kept: %+v", got.Records)
	}
	if got.Records[XAIOAuth].AccessToken != "new-ax" {
		t.Fatalf("new login not saved: %+v", got.Records[XAIOAuth])
	}
	if _, ok := got.Records[OpenAICodex]; ok {
		t.Fatalf("truncated record was kept: %+v", got.Records[OpenAICodex])
	}
}
