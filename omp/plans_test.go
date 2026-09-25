// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRefreshPlansWritesAllFour(t *testing.T) {
	fresh := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	saved := mustJSON(Item{Records: map[string]Record{
		Anthropic:   {RefreshToken: "r-a", AccessToken: "old-a", Expiry: time.Now().Add(-time.Hour)},
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
				if err := trustedPathOnly(args, "/usr/local/bin/jevons-broker"); err != nil {
					t.Fatal(err)
				}
				for i, a := range args {
					if a == "-w" && i+1 < len(args) {
						saved = args[i+1]
					}
				}
			}
			return nil, nil
		},
	}
	login := Login{
		Script: "auth.ts",
		Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if len(args) < 3 || args[1] != "refresh" {
				t.Fatalf("want refresh, got %v", args)
			}
			saw = append(saw, args[2])
			return []byte(`{"refresh_token":"n-` + args[2] + `","access_token":"fresh-` + args[2] + `","expiry":"` + fresh + `"}`), nil
		},
	}
	refreshed, skipped, err := RefreshPlans(context.Background(), s, login)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v", skipped)
	}
	if strings.Join(refreshed, ",") != strings.Join(PlanIDs, ",") {
		t.Fatalf("refreshed = %v, want %v", refreshed, PlanIDs)
	}
	if strings.Join(saw, ",") != strings.Join(PlanIDs, ",") {
		t.Fatalf("pi-ai ids = %v", saw)
	}
	var item Item
	if err := json.Unmarshal([]byte(saved), &item); err != nil {
		t.Fatal(err)
	}
	for _, id := range PlanIDs {
		rec := item.Records[id]
		if rec.AccessToken != "fresh-"+id || rec.RefreshToken != "n-"+id || rec.Expiry.IsZero() {
			t.Fatalf("%s write-back = %+v", id, rec)
		}
	}
}

func TestLoginPlansRunsPiAILogin(t *testing.T) {
	fresh := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	saved := `{"records":{}}`
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
	login := Login{
		Script: "auth.ts",
		Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if len(args) < 3 || args[1] != "login" {
				t.Fatalf("want login, got %v", args)
			}
			saw = append(saw, args[2])
			return []byte(`{"refresh_token":"n-` + args[2] + `","access_token":"fresh-` + args[2] + `","expiry":"` + fresh + `"}`), nil
		},
	}
	n, err := LoginPlans(context.Background(), s, login)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 || strings.Join(saw, ",") != strings.Join(PlanIDs, ",") {
		t.Fatalf("n=%d saw=%v", n, saw)
	}
}

func TestRefreshPlansSkipsMissingRefreshToken(t *testing.T) {
	s := Store{
		BrokerPath: "/usr/local/bin/jevons-broker",
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "security" && len(args) > 0 && args[0] == "find-generic-password" {
				return []byte(`{"records":{"anthropic":{"access_token":"a"}}}`), nil
			}
			t.Fatalf("unexpected %s %v", name, args)
			return nil, nil
		},
	}
	login := Login{
		Script: "auth.ts",
		Run: func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("login must not open a browser on serve")
			return nil, nil
		},
	}
	refreshed, skipped, err := RefreshPlans(context.Background(), s, login)
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshed) != 0 {
		t.Fatalf("refreshed = %v", refreshed)
	}
	if strings.Join(skipped, ",") != strings.Join(PlanIDs, ",") {
		t.Fatalf("skipped = %v", skipped)
	}
}
