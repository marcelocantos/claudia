// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	t131Access  = "T131_FAKE_ACCESS_TOKEN_9b1d4c0ffee"
	t131Refresh = "T131_FAKE_REFRESH_TOKEN_9b1d4c0ffee"
)

func TestT131FlushKeepsTokensOutOfSpawnedArgv(t *testing.T) {
	resetKeychainShot()
	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv.log")
	fake := filepath.Join(dir, "security")
	script := fmt.Sprintf(`#!/bin/sh
{
  printf '%%s\n' "$0"
  for a in "$@"; do printf '%%s\n' "$a"; done
} >> %s
if [ "$1" = find-generic-password ]; then
  printf '%%s\n' '{"records":{}}'
  exit 0
fi
cat >/dev/null
exit 0
`, shQuote(argvPath))
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	s := Store{
		BrokerPath: "/usr/local/bin/claudia",
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, fake, args...)
			return cmd.Output()
		},
		RunStdin: func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
			for _, a := range append([]string{name}, args...) {
				if strings.Contains(a, t131Access) || strings.Contains(a, t131Refresh) {
					t.Fatalf("token in writer argv before spawn: %q", append([]string{name}, args...))
				}
			}
			cmd := exec.CommandContext(ctx, fake, args...)
			cmd.Stdin = bytes.NewReader(stdin)
			return cmd.CombinedOutput()
		},
	}
	if err := Open(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(context.Background(), Item{Records: map[string]Record{
		Anthropic: {AccessToken: t131Access, RefreshToken: t131Refresh, Expiry: time.Now().Add(time.Hour)},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := Flush(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	dump, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("writer was not spawned: %v", err)
	}
	body := string(dump)
	if !strings.Contains(body, "-i") {
		t.Fatalf("spawned argv missing -i:\n%s", body)
	}
	for _, tok := range []string{t131Access, t131Refresh} {
		if strings.Contains(body, tok) {
			t.Fatalf("token %q in spawned process argv:\n%s", tok, body)
		}
	}
}

func TestT131FlushWithoutRunStdinDoesNotPassSecretInArgv(t *testing.T) {
	resetKeychainShot()
	var saw [][]string
	s := Store{
		BrokerPath: "/usr/local/bin/claudia",
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			saw = append(saw, append([]string{name}, args...))
			if len(args) > 0 && args[0] == "find-generic-password" {
				return []byte(`{"records":{}}`), nil
			}
			return nil, nil
		},
	}
	if err := Open(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(context.Background(), Item{Records: map[string]Record{
		Anthropic: {AccessToken: t131Access, RefreshToken: t131Refresh, Expiry: time.Now().Add(time.Hour)},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := Flush(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	var wrote bool
	for _, a := range saw {
		joined := strings.Join(a, "\x00")
		if strings.Contains(joined, t131Access) || strings.Contains(joined, t131Refresh) {
			t.Fatalf("token in argv: %q", a)
		}
		if strings.Contains(joined, "-i") {
			wrote = true
		}
	}
	if !wrote {
		t.Fatal("Flush did not invoke the writer")
	}
}

func TestExecSecurityStdinRefusesPasswordArgv(t *testing.T) {
	_, err := ExecSecurityStdin(context.Background(), nil, "security",
		"add-generic-password", "-w", t131Access)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v", err)
	}
}

func TestExecSecurityStdinTimeoutNamesInteraction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ExecSecurityStdin(ctx, []byte("add-generic-password -U -a claudia -s claudia-plan-credentials -T /bin/ls -X 00\n"),
		"security", "-q", "-i")
	if err == nil || !strings.Contains(err.Error(), "keychain ACL interaction required") {
		t.Fatalf("err = %v", err)
	}
}

// TestT131DisposableKeychainWriteCompletesOrNamesInteraction is the 🎯T131
// disposable-Keychain gate.
// 🎯T97 exemption: a Keychain modal would hang the suite; the bound is a
// hang fuse, and a slow host cannot turn a timeout into a false write success.
func TestT131DisposableKeychainWriteCompletesOrNamesInteraction(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("disposable Keychain is macOS security(1)")
	}
	if _, err := exec.LookPath("security"); err != nil {
		t.Skip("security(1) not on PATH")
	}
	resetKeychainShot()
	kc := makeDisposableKeychain(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	s := Store{
		BrokerPath: os.Args[0],
		Keychain:   kc,
		Run:        runSecurityOutput,
		RunStdin:   ExecSecurityStdin,
	}
	if err := Open(ctx, s); err != nil {
		if isInteractionErr(err) {
			t.Skipf("%v", err)
		}
		t.Fatal(err)
	}
	if err := s.Save(ctx, Item{Records: map[string]Record{
		Anthropic: {AccessToken: t131Access, RefreshToken: t131Refresh, Expiry: time.Now().Add(time.Hour)},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := Flush(ctx, s); err != nil {
		if isInteractionErr(err) {
			t.Skipf("%v", err)
		}
		t.Fatal(err)
	}
	// Metadata only: find -w prompts for ACL and hangs a modal.
	findCtx, findCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer findCancel()
	out, err := exec.CommandContext(findCtx, "security", "find-generic-password",
		"-a", keychainAccount, "-s", KeychainService, kc).CombinedOutput()
	if findCtx.Err() != nil {
		t.Skipf("keychain ACL interaction required: %v", findCtx.Err())
	}
	if err != nil {
		t.Fatalf("read back disposable item: %s: %v", out, err)
	}
	if !strings.Contains(string(out), KeychainService) {
		t.Fatalf("disposable item missing %s: %s", KeychainService, out)
	}
}

func runSecurityOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return stderr.Bytes(), fmt.Errorf("%s: keychain ACL interaction required: %w", name, ctx.Err())
		}
		return out, fmt.Errorf("%s: %w: %s", name, err, stderr.String())
	}
	return out, nil
}

func isInteractionErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "keychain ACL interaction required")
}

func makeDisposableKeychain(t *testing.T) string {
	t.Helper()
	kc := filepath.Join(t.TempDir(), "t131.keychain-db")
	pass := "t131-pass"
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command("security", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("security %v: %s: %v", args, out, err)
		}
	}
	run("create-keychain", "-p", pass, kc)
	t.Cleanup(func() { _ = exec.Command("security", "delete-keychain", kc).Run() })
	run("set-keychain-settings", "-u", kc)
	run("unlock-keychain", "-p", pass, kc)
	return kc
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
