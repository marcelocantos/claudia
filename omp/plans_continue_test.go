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

func TestRefreshPlansContinuesAfterOneBadRefreshToken(t *testing.T) {
	resetKeychainShot()
	fresh := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	saved := mustJSON(Item{Records: map[string]Record{
		Anthropic:   {RefreshToken: "bad-a", AccessToken: "old-a", Expiry: time.Now().Add(-time.Hour)},
		OpenAICodex: {RefreshToken: "r-o", AccessToken: "old-o", Expiry: time.Now().Add(-time.Hour)},
		Cursor:      {RefreshToken: "r-c", AccessToken: "old-c", Expiry: time.Now().Add(-time.Hour)},
		XAIOAuth:    {RefreshToken: "r-x", AccessToken: "old-x", Expiry: time.Now().Add(-time.Hour)},
	}})
	var saw []string
	s := Store{
		BrokerPath: "/usr/local/bin/jevons-broker",
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "security" && len(args) > 0 && args[0] == "find-generic-password" {
				return []byte(saved), nil
			}
			if name == "security" && len(args) > 0 && args[0] == "add-generic-password" {
				for i, a := range args {
					if a == "-w" && i+1 < len(args) {
						saved = args[i+1]
					}
				}
			}
			return nil, nil
		},
	}
	if err := Open(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	login := Login{
		Script: "auth.ts",
		Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if len(args) < 3 {
				t.Fatalf("args %v", args)
			}
			saw = append(saw, args[2])
			if args[2] == Anthropic {
				return nil, errors.New("invalid_grant")
			}
			return []byte(`{"refresh_token":"n-` + args[2] + `","access_token":"fresh-` + args[2] + `","expiry":"` + fresh + `"}`), nil
		},
	}
	refreshed, skipped, err := RefreshPlans(context.Background(), s, login)
	if err == nil || !strings.Contains(err.Error(), Anthropic) {
		t.Fatalf("err = %v, want the anthropic failure", err)
	}
	if strings.Join(saw, ",") != strings.Join(PlanIDs, ",") {
		t.Fatalf("pi-ai stopped early: %v", saw)
	}
	want := []string{OpenAICodex, Cursor, XAIOAuth}
	if strings.Join(refreshed, ",") != strings.Join(want, ",") {
		t.Fatalf("refreshed = %v, want %v (skipped %v)", refreshed, want, skipped)
	}
	item, err := s.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if item.Records[OpenAICodex].AccessToken != "fresh-"+OpenAICodex {
		t.Fatalf("openai-codex was not saved: %+v", item.Records[OpenAICodex])
	}
	if item.Records[XAIOAuth].AccessToken != "fresh-"+XAIOAuth {
		t.Fatalf("xai-oauth was not saved: %+v", item.Records[XAIOAuth])
	}
	if item.Records[Anthropic].AccessToken != "old-a" {
		t.Fatalf("failed anthropic refresh overwrote the record: %+v", item.Records[Anthropic])
	}
}
