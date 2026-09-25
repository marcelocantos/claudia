// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSaveTrustsOnlyTheBroker(t *testing.T) {
	var cmds []string
	s := Store{
		BrokerPath: "/usr/local/bin/claudia",
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			cmds = append(cmds, name+" "+strings.Join(args, " "))
			return nil, nil
		},
	}
	err := s.Save(context.Background(), Item{Records: map[string]Record{
		Anthropic: {AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(time.Hour)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.Join(cmds, "\n")
	if !strings.Contains(blob, "delete-generic-password -a claudia -s "+KeychainService) {
		t.Fatalf("Save must recreate the item so -T is the whole ACL: %s", blob)
	}
	if !strings.Contains(blob, "-T /usr/local/bin/claudia") {
		t.Fatalf("ACL = %s", blob)
	}
	if strings.Contains(blob, "-U") {
		t.Fatalf("Save must not -U an old ACL: %s", blob)
	}
	if strings.Contains(blob, "-A") || strings.Contains(blob, "jevonsd") || strings.Contains(blob, "bun") {
		t.Fatalf("ACL trusts more than the broker: %s", blob)
	}
	if strings.Contains(blob, " -p ") || strings.Contains(blob, " -P ") || strings.Contains(blob, "-p ") {
		t.Fatalf("item has a passphrase flag: %s", blob)
	}
	if strings.Contains(blob, "openai-api-key") || strings.Contains(blob, "xai-api-key") {
		t.Fatalf("wrote a pay-as-you-go item: %s", blob)
	}
	if !strings.Contains(blob, "-s "+KeychainService) {
		t.Fatalf("service = %s", blob)
	}
	add := 0
	for _, c := range cmds {
		if strings.Contains(c, "add-generic-password") {
			add++
		}
	}
	if add != 1 {
		t.Fatalf("Save must add once after delete, got %s", blob)
	}
}

func TestSaveRefusesPayAsYouGoService(t *testing.T) {
	err := trustedPathOnly([]string{
		"add-generic-password", "-a", "claudia", "-s", "openai-api-key",
		"-T", "/usr/local/bin/claudia", "-w", "{}",
	}, "/usr/local/bin/claudia")
	if err == nil || !strings.Contains(err.Error(), "openai-api-key") {
		t.Fatalf("err = %v", err)
	}
	err = trustedPathOnly([]string{
		"add-generic-password", "-a", "claudia", "-s", "xai-api-key",
		"-T", "/usr/local/bin/claudia", "-w", "{}",
	}, "/usr/local/bin/claudia")
	if err == nil || !strings.Contains(err.Error(), "xai-api-key") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefreshFailureDoesNotFallThrough(t *testing.T) {
	s := Store{
		BrokerPath: "/usr/local/bin/claudia",
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "security" && len(args) > 0 && args[0] == "find-generic-password" {
				return nil, errors.New("The specified item could not be found in the keychain.")
			}
			t.Fatalf("refresh failure wrote the keychain: %s %v", name, args)
			return nil, nil
		},
	}
	login := Login{
		Script: "auth.ts",
		Run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("oauth refresh rejected")
		},
	}
	_, err := login.Refresh(context.Background(), s, Anthropic)
	if err == nil || !strings.Contains(err.Error(), "refresh failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestItemHoldsFourPlanRecords(t *testing.T) {
	now := time.Now().Add(time.Hour)
	saved := `{"records":{}}`
	s := Store{
		BrokerPath: "/usr/local/bin/claudia",
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
	for _, id := range []string{Anthropic, OpenAICodex, Cursor, XAIOAuth} {
		if err := s.Put(context.Background(), id, Record{
			RefreshToken: "r-" + id,
			AccessToken:  "a-" + id,
			Expiry:       now,
		}); err != nil {
			t.Fatalf("Put %s: %v", id, err)
		}
	}
	var item Item
	if err := json.Unmarshal([]byte(saved), &item); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{Anthropic, OpenAICodex, Cursor, XAIOAuth} {
		rec := item.Records[id]
		if rec.AccessToken != "a-"+id || rec.RefreshToken != "r-"+id || rec.Expiry.IsZero() {
			t.Fatalf("%s record = %+v, want refresh/access/expiry", id, rec)
		}
	}
}

func TestEnsureRefreshesExpiredRecord(t *testing.T) {
	expired := time.Now().Add(-time.Hour)
	fresh := time.Now().Add(time.Hour)
	for _, id := range []string{Anthropic, OpenAICodex, Cursor, XAIOAuth} {
		t.Run(id, func(t *testing.T) {
			blob := mustJSON(Item{Records: map[string]Record{
				id: {RefreshToken: "r", AccessToken: "old", Expiry: expired},
			}})
			var saved string
			s := Store{
				BrokerPath: "/usr/local/bin/claudia",
				Now:        func() time.Time { return time.Now() },
				Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
					if name == "security" && len(args) > 0 && args[0] == "find-generic-password" {
						if saved != "" {
							return []byte(saved), nil
						}
						return []byte(blob), nil
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
					if len(args) < 3 || args[1] != "refresh" || args[2] != id {
						t.Fatalf("expected refresh %s, got %v", id, args)
					}
					return []byte(`{"refresh_token":"nr","access_token":"fresh","expiry":"` + fresh.UTC().Format(time.RFC3339) + `"}`), nil
				},
			}
			tok, err := s.Ensure(context.Background(), id, login)
			if err != nil {
				t.Fatal(err)
			}
			if tok != "fresh" {
				t.Fatalf("token = %q", tok)
			}
			var item Item
			if err := json.Unmarshal([]byte(saved), &item); err != nil {
				t.Fatal(err)
			}
			if item.Records[id].AccessToken != "fresh" || item.Records[id].RefreshToken != "nr" || item.Records[id].Expiry.IsZero() {
				t.Fatalf("keychain write-back = %+v", item.Records[id])
			}
		})
	}
}

func TestEnsureMissingRecordUsesLogin(t *testing.T) {
	fresh := time.Now().Add(time.Hour)
	for _, id := range []string{Anthropic, OpenAICodex, Cursor, XAIOAuth} {
		t.Run(id, func(t *testing.T) {
			var saved string
			s := Store{
				BrokerPath: "/usr/local/bin/claudia",
				Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
					if name == "security" && len(args) > 0 && args[0] == "find-generic-password" {
						if saved != "" {
							return []byte(saved), nil
						}
						return []byte(`{"records":{}}`), nil
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
					if len(args) < 3 || args[1] != "login" || args[2] != id {
						t.Fatalf("expected login %s, got %v", id, args)
					}
					return []byte(`{"refresh_token":"nr","access_token":"fresh","expiry":"` + fresh.UTC().Format(time.RFC3339) + `"}`), nil
				},
			}
			tok, err := s.Ensure(context.Background(), id, login)
			if err != nil {
				t.Fatal(err)
			}
			if tok != "fresh" {
				t.Fatalf("token = %q", tok)
			}
		})
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestScrubEnvDropsPlanKeys(t *testing.T) {
	got := ScrubEnv([]string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk",
		"OPENAI_API_KEY=sk",
		"XAI_API_KEY=sk",
		"CURSOR_ACCESS_TOKEN=tok",
		"HOME=/Users/me",
	})
	blob := strings.Join(got, "\n")
	for _, banned := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY", "CURSOR_ACCESS_TOKEN"} {
		if strings.Contains(blob, banned) {
			t.Fatalf("sidecar env still has %s: %s", banned, blob)
		}
	}
	if !strings.Contains(blob, "PATH=/usr/bin") {
		t.Fatalf("dropped unrelated env: %s", blob)
	}
}
