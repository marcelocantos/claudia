// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSaveTrustsOnlyTheBroker(t *testing.T) {
	resetKeychainShot()
	var cmds []string
	var stdin []byte
	s := Store{
		BrokerPath: "/usr/local/bin/claudia",
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			cmds = append(cmds, name+" "+strings.Join(args, " "))
			return nil, nil
		},
		RunStdin: func(_ context.Context, in []byte, name string, args ...string) ([]byte, error) {
			cmds = append(cmds, name+" "+strings.Join(args, " "))
			stdin = append([]byte{}, in...)
			return nil, nil
		},
	}
	if err := Open(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(context.Background(), Item{Records: map[string]Record{
		Anthropic: {AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(time.Hour)},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := Flush(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(cmds, "\n")
	if strings.Contains(argv, "delete-generic-password") {
		t.Fatalf("Save must not delete the item; that drops Always Allow: %s", argv)
	}
	for _, c := range cmds {
		if strings.Contains(c, "-i") && (strings.Contains(c, " -w ") || strings.HasSuffix(c, " -w")) {
			t.Fatalf("password must not be in argv: %s", c)
		}
	}
	if !strings.Contains(argv, "-i") {
		t.Fatalf("Flush must use security -i: %s", argv)
	}
	body := string(stdin)
	if !strings.Contains(body, "-U") {
		t.Fatalf("Save must update in place: %s", body)
	}
	if !strings.Contains(body, "-T /usr/local/bin/claudia") {
		t.Fatalf("ACL = %s", body)
	}
	if strings.Contains(body, "-A") || strings.Contains(body, "jevonsd") || strings.Contains(body, "bun") {
		t.Fatalf("ACL trusts more than the broker: %s", body)
	}
	if strings.Contains(body, " -p ") || strings.Contains(body, " -P ") || strings.Contains(body, "-p ") {
		t.Fatalf("item has a passphrase flag: %s", body)
	}
	if strings.Contains(body, "openai-api-key") || strings.Contains(body, "xai-api-key") {
		t.Fatalf("wrote a pay-as-you-go item: %s", body)
	}
	if !strings.Contains(body, "-s "+KeychainService) {
		t.Fatalf("service = %s", body)
	}
	if !strings.Contains(body, "-X ") {
		t.Fatalf("secret must travel as -X hex on stdin: %s", body)
	}
	add := 0
	for _, c := range cmds {
		if strings.Contains(c, "-i") {
			add++
		}
	}
	if add != 1 {
		t.Fatalf("Save must add once, got %s", argv)
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
	resetKeychainShot()
	exp := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	blob := `{"records":{"anthropic":{"refresh_token":"old-r","access_token":"old","expiry":"` + exp + `"}}}`
	s := Store{
		BrokerPath: "/usr/local/bin/claudia",
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "security" && len(args) > 0 && args[0] == "find-generic-password" {
				return []byte(blob), nil
			}
			t.Fatalf("refresh failure wrote the keychain: %s %v", name, args)
			return nil, nil
		},
	}
	if err := Open(context.Background(), s); err != nil {
		t.Fatal(err)
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
	resetKeychainShot()
	now := time.Now().Add(time.Hour)
	saved := `{"records":{}}`
	var reads, writes int
	s := Store{
		BrokerPath: "/usr/local/bin/claudia",
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "security" && len(args) > 0 && args[0] == "find-generic-password" {
				reads++
				return []byte(saved), nil
			}
			return nil, nil
		},
		RunStdin: func(_ context.Context, in []byte, name string, args ...string) ([]byte, error) {
			writes++
			saved = string(itemJSONFromFlushStdin(t, in))
			return nil, nil
		},
	}
	if err := Open(context.Background(), s); err != nil {
		t.Fatal(err)
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
	if reads != 1 || writes != 0 {
		t.Fatalf("before flush reads=%d writes=%d, want 1 and 0", reads, writes)
	}
	if err := Flush(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if reads != 1 || writes != 1 {
		t.Fatalf("keychain reads=%d writes=%d, want 1 and 1", reads, writes)
	}
	item, err := s.Load(context.Background())
	if err != nil {
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
			resetKeychainShot()
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
					return nil, nil
				},
				RunStdin: func(_ context.Context, in []byte, name string, args ...string) ([]byte, error) {
					saved = string(itemJSONFromFlushStdin(t, in))
					return nil, nil
				},
			}
			if err := Open(context.Background(), s); err != nil {
				t.Fatal(err)
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
			if err := Flush(context.Background(), s); err != nil {
				t.Fatal(err)
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
			resetKeychainShot()
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
			if err := Open(context.Background(), s); err != nil {
				t.Fatal(err)
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

func TestSealPathRefusesOtherBinary(t *testing.T) {
	resetKeychainShot()
	s := Store{
		BrokerPath: "/usr/local/bin/jevons-broker",
		SealPath:   true,
		Run: func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("untrusted binary must not call security")
			return nil, nil
		},
	}
	err := Open(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "did not approve this binary") {
		t.Fatalf("err = %v", err)
	}
	self, _ := os.Executable()
	if resolvePath(self) == resolvePath("/usr/local/bin/jevons-broker") {
		t.Fatal("test binary collided with the sealed broker path")
	}
}

func itemJSONFromFlushStdin(t *testing.T, stdin []byte) []byte {
	t.Helper()
	fields := strings.Fields(string(stdin))
	for i, f := range fields {
		if f == "-X" && i+1 < len(fields) {
			raw, err := hex.DecodeString(fields[i+1])
			if err != nil {
				t.Fatalf("flush stdin -X: %v (%s)", err, stdin)
			}
			return raw
		}
	}
	t.Fatalf("no -X hex in flush stdin: %s", stdin)
	return nil
}
