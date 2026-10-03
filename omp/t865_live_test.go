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

func TestT865LiveBrokerRefreshPlans(t *testing.T) {
	if os.Getenv("CLAUDIA_OMP_LIVE") == "" {
		t.Skip("CLAUDIA_OMP_LIVE not set")
	}
	h := newLiveHarness(t)
	plan := os.Getenv("CLAUDIA_OMP_TEST_PLAN")
	if !known(plan) {
		t.Fatal("set CLAUDIA_OMP_TEST_PLAN to one subscription plan")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	before, after, err := recoverLivePlan(ctx, func(ctx context.Context, args ...string) ([]byte, error) {
		return runLiveCommand(ctx, h.BrokerBinary, args...)
	}, plan)
	t.Logf("supported no-login recovery: %s -> %s; health does not prove token renewal", before, after)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatal(errLiveRenewalEvidence)
}

func liveBrokerBin(t *testing.T) string {
	t.Helper()
	p, err := explicitLiveBinary(os.Getenv("JEVONS_BROKER_BIN"))
	if err != nil {
		t.Fatal(err)
	}
	return p
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
	// Refuse before touching the selected runtime: supported CLI acknowledgements
	// cannot satisfy this test's behavioral oracle. Do not substitute fleet aliases.
	if err := liveSmokePrerequisite(os.Getenv("CLAUDIA_OMP_TEST_PROVIDER")); err != nil {
		t.Fatal(err)
	}

}

func TestT865LiveSidecarSurvivesJevonsdBounce(t *testing.T) {
	if os.Getenv("CLAUDIA_OMP_LIVE") == "" {
		t.Skip("CLAUDIA_OMP_LIVE not set")
	}
	if os.Getenv("CLAUDIA_OMP_BOUNCE_AUTHORIZED") != "restart-jevonsd" {
		t.Fatal(errLiveBouncePermit)
	}
	h := newLiveHarness(t)
	restart, ready, err := bounceCommands(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := runLiveCommand(ctx, restart[0], restart[1:]...); err != nil {
		t.Fatalf("authorized jevonsd restart: %v", err)
	}
	if err := waitLiveReady(ctx, func(ctx context.Context) bool {
		_, err := runLiveCommand(ctx, ready[0], ready[1:]...)
		return err == nil
	}); err != nil {
		t.Fatalf("jevonsd readiness: %v", err)
	}
	h.preflight(t) // same serving PIDs and start times, both sockets still listening
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
