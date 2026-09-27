// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// bigItem is larger than security -i's 4 KiB stdin line once hex-encoded,
// which is what a real four-plan login blob is.
func bigItem() Item {
	tok := func(c string) string { return strings.Repeat(c, 1500) }
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	return Item{Records: map[string]Record{
		Anthropic:   {AccessToken: tok("a"), RefreshToken: tok("A"), Expiry: exp},
		OpenAICodex: {AccessToken: tok("o"), RefreshToken: tok("O"), Expiry: exp},
		Cursor:      {AccessToken: tok("c"), RefreshToken: tok("C"), Expiry: exp},
		XAIOAuth:    {AccessToken: tok("x"), RefreshToken: tok("X"), Expiry: exp},
	}}
}

// fakeKeychain is one generic-password value behind Run and RunStdin.
type fakeKeychain struct {
	value  string
	writes int
	stdin  []string
}

func (k *fakeKeychain) store(t *testing.T, dataPath string) Store {
	return Store{
		BrokerPath: "/usr/local/bin/claudia",
		DataPath:   dataPath,
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "security" && len(args) > 0 && args[0] == "find-generic-password" {
				return []byte(k.value), nil
			}
			t.Fatalf("unexpected command %s %v", name, args)
			return nil, nil
		},
		RunStdin: func(_ context.Context, in []byte, _ string, _ ...string) ([]byte, error) {
			k.writes++
			k.stdin = append(k.stdin, string(in))
			k.value = string(itemJSONFromFlushStdin(t, in))
			return nil, nil
		},
	}
}

func TestPlanDataMigratesLegacyItemAndLaterFlushesSkipKeychain(t *testing.T) {
	resetKeychainShot()
	t.Cleanup(resetKeychainShot)
	path := filepath.Join(t.TempDir(), "claudia", "plan.enc")
	kc := &fakeKeychain{value: mustJSON(Item{Records: map[string]Record{
		Anthropic: {AccessToken: "legacy-a", RefreshToken: "legacy-r", Expiry: time.Now().Add(time.Hour)},
	}})}
	s := kc.store(t, path)
	ctx := context.Background()

	if err := Open(ctx, s); err != nil {
		t.Fatal(err)
	}
	item, _ := s.Load(ctx)
	if item.Records[Anthropic].AccessToken != "legacy-a" {
		t.Fatalf("legacy item not read: %+v", item)
	}
	big := bigItem()
	if err := s.Save(ctx, big); err != nil {
		t.Fatal(err)
	}
	if err := Flush(ctx, s); err != nil {
		t.Fatal(err)
	}
	if kc.writes != 1 {
		t.Fatalf("keychain writes = %d, want 1 (the key)", kc.writes)
	}
	line := kc.stdin[0]
	if len(line) >= 4096 {
		t.Fatalf("security -i line is %d bytes; it truncates at 4096", len(line))
	}
	for _, rec := range big.Records {
		if strings.Contains(kc.value, rec.AccessToken) || strings.Contains(line, rec.AccessToken) {
			t.Fatal("plan token reached the Keychain item or its stdin")
		}
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("plan data mode = %v, want 0600", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), big.Records[Anthropic].AccessToken[:64]) {
		t.Fatal("plan data file holds a token in the clear")
	}

	// A later process reads the key, decrypts the file, and a changed
	// item is written to the file alone.
	resetKeychainShot()
	if err := Open(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Load(ctx)
	if !sameItem(got, big) {
		t.Fatal("reopened item differs from the flushed one")
	}
	if err := s.Put(ctx, Cursor, Record{AccessToken: "new-c", RefreshToken: "new-r"}); err != nil {
		t.Fatal(err)
	}
	if err := Flush(ctx, s); err != nil {
		t.Fatal(err)
	}
	if kc.writes != 1 {
		t.Fatalf("keychain writes = %d after a keyed flush, want still 1", kc.writes)
	}
	resetKeychainShot()
	if err := Open(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Load(ctx)
	if got.Records[Cursor].AccessToken != "new-c" || got.Records[Anthropic].AccessToken != big.Records[Anthropic].AccessToken {
		t.Fatalf("second flush lost data: %+v", got.Records[Cursor])
	}
}

func TestPlanDataWrongKeyFailsLoudly(t *testing.T) {
	resetKeychainShot()
	t.Cleanup(resetKeychainShot)
	path := filepath.Join(t.TempDir(), "plan.enc")
	kc := &fakeKeychain{}
	s := kc.store(t, path)
	ctx := context.Background()
	if err := Open(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, bigItem()); err != nil {
		t.Fatal(err)
	}
	if err := Flush(ctx, s); err != nil {
		t.Fatal(err)
	}
	kc.value = `{"data_key":"` + strings.Repeat("00", dataKeyLen) + `"}`
	resetKeychainShot()
	err := Open(ctx, s)
	if err == nil || !strings.Contains(err.Error(), "does not match the Keychain key") {
		t.Fatalf("err = %v", err)
	}
}

// TestPlanDataLargeBlobDisposableKeychain is the regression for
// "omp: Keychain stdin write failed: exit status 1": security -i cut the
// hex plan blob at 4 KiB. A real-sized blob now flushes through the real
// security(1) into a disposable Keychain.
// 🎯T97 exemption: a Keychain modal would hang the suite; the bound is a
// hang fuse, and a slow host cannot turn a timeout into a false success.
func TestPlanDataLargeBlobDisposableKeychain(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("disposable Keychain is macOS security(1)")
	}
	if _, err := exec.LookPath("security"); err != nil {
		t.Skip("security(1) not on PATH")
	}
	resetKeychainShot()
	t.Cleanup(resetKeychainShot)
	kc := makeDisposableKeychain(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The ACL names security(1) itself, so reading the key back with -w
	// does not open a modal; the round trip is then decisive.
	s := Store{
		BrokerPath: "/usr/bin/security",
		Keychain:   kc,
		DataPath:   filepath.Join(t.TempDir(), "plan.enc"),
		Run:        runSecurityOutput,
		RunStdin:   ExecSecurityStdin,
	}
	if err := Open(ctx, s); err != nil {
		if isInteractionErr(err) {
			t.Skipf("%v", err)
		}
		t.Fatal(err)
	}
	big := bigItem()
	if err := s.Save(ctx, big); err != nil {
		t.Fatal(err)
	}
	if err := Flush(ctx, s); err != nil {
		if isInteractionErr(err) {
			t.Skipf("%v", err)
		}
		t.Fatal(err)
	}
	resetKeychainShot()
	readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer readCancel()
	if err := Open(readCtx, s); err != nil {
		t.Fatalf("read back: %v", err)
	}
	got, _ := s.Load(ctx)
	if !sameItem(got, big) {
		t.Fatal("disposable Keychain round trip lost the plan blob")
	}
}
