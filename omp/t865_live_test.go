// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestT865LiveKeychainACLTrustsOnlyBroker(t *testing.T) {
	if _, err := exec.LookPath("security"); err != nil {
		t.Skip("security(1) not on PATH")
	}
	kc := filepath.Join(t.TempDir(), "t865.keychain-db")
	pass := "t865-pass"
	if out, err := exec.Command("security", "create-keychain", "-p", pass, kc).CombinedOutput(); err != nil {
		t.Fatalf("create-keychain: %s: %v", out, err)
	}
	t.Cleanup(func() { _ = exec.Command("security", "delete-keychain", kc).Run() })
	if out, err := exec.Command("security", "set-keychain-settings", "-u", kc).CombinedOutput(); err != nil {
		t.Fatalf("set-keychain-settings: %s: %v", out, err)
	}
	if out, err := exec.Command("security", "unlock-keychain", "-p", pass, kc).CombinedOutput(); err != nil {
		t.Fatalf("unlock-keychain: %s: %v", out, err)
	}

	broker := os.Args[0]
	args := []string{
		"add-generic-password", "-a", "claudia", "-s", KeychainService,
		"-T", broker, "-w", `{"records":{}}`, kc,
	}
	if err := trustedPathOnly(args[:len(args)-1], broker); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("security", args...).CombinedOutput(); err != nil {
		t.Fatalf("add-generic-password: %s: %v", out, err)
	}

	find, err := exec.Command("security", "find-generic-password", "-a", "claudia", "-s", KeychainService, kc).CombinedOutput()
	if err != nil {
		t.Fatalf("find-generic-password: %s: %v", find, err)
	}
	if !strings.Contains(string(find), KeychainService) {
		t.Fatalf("item missing: %s", find)
	}

	other := filepath.Join(t.TempDir(), "rebuilt-broker")
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, self, 0o755); err != nil {
		t.Fatal(err)
	}
	if other == broker {
		t.Fatal("rebuilt path collided with the trusted broker")
	}
	if err := trustedPathOnly([]string{
		"add-generic-password", "-a", "claudia", "-s", KeychainService, "-T", other,
	}, broker); err == nil {
		t.Fatal("rebuilt broker path was accepted as the ACL without re-approval")
	}
}

func TestT865AuthScriptIsPiAI(t *testing.T) {
	script := filepath.Join(filepath.Dir(ServerScript()), "auth.ts")
	src, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, `from "@oh-my-pi/pi-ai"`) {
		t.Fatal("auth.ts must import @oh-my-pi/pi-ai")
	}
	if !strings.Contains(body, "refreshOAuthToken") || !strings.Contains(body, "login") {
		t.Fatal("auth.ts must expose login and refresh")
	}
	for _, id := range []string{Anthropic, OpenAICodex, Cursor, XAIOAuth} {
		if id == "" {
			t.Fatal("empty provider")
		}
	}
}

func TestT865AuthScriptOpensLoginURL(t *testing.T) {
	script := filepath.Join(filepath.Dir(ServerScript()), "auth.ts")
	src, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `Bun.spawn(["open", url]`) {
		t.Fatal("auth.ts login must open the pi-ai URL")
	}
}

func TestT865LivePiAIRefreshWritesKeychain(t *testing.T) {
	if os.Getenv("CLAUDIA_OMP_LIVE") == "" {
		t.Skip("CLAUDIA_OMP_LIVE not set")
	}
	script := filepath.Join(filepath.Dir(ServerScript()), "auth.ts")
	if _, err := os.Stat(script); err != nil {
		t.Fatal(err)
	}
	store := Store{BrokerPath: os.Args[0], Run: execSecurity}
	item, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("load login keychain: %v", err)
	}
	login := Login{Command: "bun", Script: script, Run: execBun}
	refreshed := 0
	for _, id := range []string{Anthropic, OpenAICodex, Cursor, XAIOAuth} {
		if item.Records[id].RefreshToken == "" {
			t.Logf("%s has no stored refresh token; login requires a browser", id)
			continue
		}
		rec, err := login.Refresh(context.Background(), store, id)
		if err != nil {
			t.Fatalf("%s refresh: %v", id, err)
		}
		if rec.AccessToken == "" || rec.RefreshToken == "" || rec.Expiry.IsZero() {
			t.Fatalf("%s refresh omitted fields: %+v", id, rec)
		}
		got, err := store.AccessToken(context.Background(), id)
		if err != nil || got != rec.AccessToken {
			t.Fatalf("%s keychain write-back: %v %q", id, err, got)
		}
		refreshed++
	}
	if refreshed != 4 {
		t.Fatalf("refreshed %d/4 providers; each needs `jevons-broker login-plans` through pi-ai", refreshed)
	}
}

func execSecurity(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.Output()
}

func execBun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = ScrubEnv(os.Environ())
	cmd.Dir = filepath.Dir(ServerScript())
	return cmd.Output()
}

func TestT865LiveKeychainItemExists(t *testing.T) {
	if _, err := exec.LookPath("security"); err != nil {
		t.Skip("security(1) not on PATH")
	}
	out, err := exec.Command("security", "find-generic-password", "-a", "claudia", "-s", KeychainService).CombinedOutput()
	if err != nil {
		t.Fatalf("live %s item missing: %s: %v", KeychainService, out, err)
	}
	if !strings.Contains(string(out), KeychainService) {
		t.Fatalf("live item missing service name: %s", out)
	}
}

func TestT865ItemRoundTripJSON(t *testing.T) {
	raw := []byte(`{"records":{"anthropic":{"refresh_token":"r","access_token":"a","expiry":"2026-09-25T12:00:00Z"}}}`)
	var item Item
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}
	if item.Records[Anthropic].AccessToken != "a" {
		t.Fatalf("%+v", item)
	}
}
