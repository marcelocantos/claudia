// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if !strings.Contains(body, "Bun.stdin") {
		t.Fatal("auth.ts must read the record from stdin, not only argv")
	}
	if !strings.Contains(body, "OMP_FORCE_LOGIN") {
		t.Fatal("auth.ts must allow a named re-login without rebuilding the broker")
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

func TestT865LiveNonBrokerCannotRead(t *testing.T) {
	resetKeychainShot()
	if os.Getenv("CLAUDIA_OMP_LIVE") == "" {
		t.Skip("CLAUDIA_OMP_LIVE not set")
	}
	store := Store{BrokerPath: liveBrokerBin(t), Run: execSecurity, SealPath: true}
	err := Open(context.Background(), store)
	if err == nil {
		t.Fatal("test binary must be refused by the live ACL")
	}
	if !strings.Contains(err.Error(), "44") && !strings.Contains(err.Error(), "errSecAuthFailed") {
		t.Fatalf("want ACL refusal, got %v", err)
	}
}

func TestT865LiveBrokerRefreshPlans(t *testing.T) {
	if os.Getenv("CLAUDIA_OMP_LIVE") == "" {
		t.Skip("CLAUDIA_OMP_LIVE not set")
	}
	broker := liveBrokerBin(t)
	out, err := exec.Command(broker, "refresh-plans").CombinedOutput()
	if err != nil {
		t.Fatalf("jevons-broker refresh-plans: %s: %v", out, err)
	}
	body := string(out)
	for _, id := range []string{Anthropic, OpenAICodex, Cursor, XAIOAuth} {
		if !strings.Contains(body, id) {
			t.Fatalf("refresh-plans omitted %s: %s", id, body)
		}
	}
}

func TestT865LiveRebuiltBrokerRefused(t *testing.T) {
	if os.Getenv("CLAUDIA_OMP_LIVE") == "" {
		t.Skip("CLAUDIA_OMP_LIVE not set")
	}
	broker := liveBrokerBin(t)
	other := filepath.Join(t.TempDir(), "rebuilt-jevons-broker")
	self, err := os.ReadFile(broker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, self, 0o755); err != nil {
		t.Fatal(err)
	}
	if other == broker {
		t.Fatal("rebuilt path collided with the trusted broker")
	}
	cmd := exec.Command(other, "refresh-plans")
	cmd.Env = append(os.Environ(), "JEVONS_BROKER_BIN="+broker)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("rebuilt broker was accepted by the Keychain ACL: %s", out)
	}
	body := strings.ToLower(string(out) + err.Error())
	if !strings.Contains(body, "44") && !strings.Contains(body, "errsecauthfailed") &&
		!strings.Contains(body, "acl") && !strings.Contains(body, "keychain") {
		t.Fatalf("want ACL refusal, got %s: %v", out, err)
	}
}

func liveBrokerBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("JEVONS_BROKER_BIN"); p != "" {
		return p
	}
	candidates := []string{
		filepath.Join("..", "..", "jevons", "bin", "jevons-broker"),
		filepath.Join("..", "jevons", "bin", "jevons-broker"),
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			abs, err := filepath.Abs(p)
			if err != nil {
				t.Fatal(err)
			}
			return abs
		}
	}
	t.Fatal("jevons-broker not found; set JEVONS_BROKER_BIN")
	return ""
}

func execSecurity(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.Output()
}

func execBun(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdin []byte
	if len(args) >= 4 {
		stdin = []byte(args[len(args)-1])
		args = args[:len(args)-1]
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = ScrubEnv(os.Environ())
	cmd.Dir = filepath.Dir(ServerScript())
	return cmd.Output()
}

func TestT865LiveBrokerSmokeLaunchVerbs(t *testing.T) {
	if os.Getenv("CLAUDIA_OMP_LIVE") == "" {
		t.Skip("CLAUDIA_OMP_LIVE not set")
	}
	broker := liveBrokerBin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, broker, "smoke")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("jevons-broker smoke: %s: %v", out, err)
	}
	body := string(out)
	if !strings.Contains(body, "launch send steer abort") {
		t.Fatalf("smoke did not land Launch/Send/steer/abort: %s", body)
	}
	if !strings.Contains(body, "jevons_*") {
		t.Fatalf("smoke did not arm jevons_*: %s", body)
	}
}

func TestT865LiveSidecarSurvivesJevonsdBounce(t *testing.T) {
	if os.Getenv("CLAUDIA_OMP_LIVE") == "" {
		t.Skip("CLAUDIA_OMP_LIVE not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	socket, err := SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if !Listening(ctx, socket) {
		if _, err := Ensure(ctx); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(pidPath(socket))
	if err != nil {
		t.Fatal(err)
	}
	before := strings.TrimSpace(string(raw))
	if before == "" {
		t.Fatal("sidecar pid file empty")
	}
	bounce := exec.Command("supervisorctl", "restart", "jevonsd")
	if out, err := bounce.CombinedOutput(); err != nil {
		t.Fatalf("jevonsd bounce: %s: %v", out, err)
	}
	time.Sleep(2 * time.Second)
	if !Listening(context.Background(), socket) {
		t.Fatal("sidecar stopped listening after a jevonsd bounce")
	}
	after, err := os.ReadFile(pidPath(socket))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(after)); got != before {
		t.Fatalf("sidecar pid %s → %s; a jevonsd bounce must leave it running", before, got)
	}
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
