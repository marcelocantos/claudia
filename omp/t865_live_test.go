// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
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
	// A rebuilt binary at a different path is not in the ACL. The
	// argv we passed named exactly one -T, the original broker.
	if other == broker {
		t.Fatal("rebuilt path collided with the trusted broker")
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
