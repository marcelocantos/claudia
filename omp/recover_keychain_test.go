// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestRecoverPlanDisposableKeychainAfterMissedRead exercises the real
// security(1) read/write boundary. Run it only while an operator can answer
// a macOS test-Keychain prompt:
// CLAUDIA_RECOVERY_KEYCHAIN_LIVE=1 go test ./omp -run '^TestRecoverPlanDisposableKeychainAfterMissedRead$' -count=1 -v
func TestRecoverPlanDisposableKeychainAfterMissedRead(t *testing.T) {
	if os.Getenv("CLAUDIA_RECOVERY_KEYCHAIN_LIVE") != "1" {
		t.Skip("set CLAUDIA_RECOVERY_KEYCHAIN_LIVE=1 with an operator present")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("disposable Keychain requires macOS")
	}
	if _, err := exec.LookPath("security"); err != nil {
		t.Skip("security(1) not on PATH")
	}
	resetKeychainShot()
	t.Cleanup(resetKeychainShot)
	kc := makeDisposableKeychain(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reads := 0
	store := Store{
		BrokerPath: os.Args[0],
		Keychain:   kc,
		RunStdin:   ExecSecurityStdin,
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "security" && len(args) > 0 && args[0] == "find-generic-password" {
				reads++
				if reads == 1 {
					return nil, errors.New("test: startup Keychain read missed")
				}
			}
			return runSecurityOutput(ctx, name, args...)
		},
	}
	if err := Open(ctx, store); err == nil {
		t.Fatal("simulated startup read unexpectedly succeeded")
	}
	logins := 0
	login := Login{Script: "auth.ts", Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		logins++
		if len(args) < 2 || args[1] != "login" {
			t.Fatalf("expected login after missing credential, args=%v", args[:min(len(args), 2)])
		}
		return []byte(`{"access_token":"test-recovered-access","refresh_token":"test-recovered-refresh","expiry":"` +
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`), nil
	}}
	if err := RecoverPlan(ctx, store, login, Anthropic); err != nil {
		t.Fatalf("disposable Keychain recovery: %v", err)
	}
	if reads != 2 || logins != 1 {
		t.Fatalf("reads=%d logins=%d, want one missed read, one retry, one login", reads, logins)
	}
	item, err := store.Load(ctx)
	if err != nil || item.Records[Anthropic].AccessToken != "test-recovered-access" {
		t.Fatalf("recovered in-memory record = %+v, %v", item.Records[Anthropic], err)
	}
	// Metadata only: -w can open a separate ACL prompt even after Flush.
	findCtx, findCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer findCancel()
	out, err := exec.CommandContext(findCtx, "security", "find-generic-password",
		"-a", keychainAccount, "-s", KeychainService, kc).CombinedOutput()
	if err != nil || !strings.Contains(string(out), KeychainService) {
		t.Fatalf("recovered item not persisted: %v: %s", err, out)
	}
}
