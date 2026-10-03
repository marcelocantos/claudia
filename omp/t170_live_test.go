// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// These two historical test names exercise the application seal. Only the
// separate direct native probe can establish an operating-system ACL refusal.
func TestT865LiveNonBrokerCannotRead(t *testing.T) {
	if os.Getenv("CLAUDIA_OMP_LIVE") == "" {
		t.Skip("CLAUDIA_OMP_LIVE not set")
	}
	t170AssertPathRefusal(t, liveBrokerBin(t))
}

func t170AssertPathRefusal(t *testing.T, broker string) {
	t.Helper()
	resetKeychainShot()
	t.Cleanup(resetKeychainShot)
	calls := 0
	store := Store{BrokerPath: broker, SealPath: true, Run: func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("unexpected Keychain call")
	}}
	err := Open(context.Background(), store)
	var refusal *BrokerPathError
	if !errors.As(err, &refusal) {
		t.Fatalf("want application BrokerPathError, got %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if refusal.Path != resolvePath(self) || refusal.Want != resolvePath(broker) || refusal.Path == refusal.Want {
		t.Fatalf("incorrect path refusal: %+v", refusal)
	}
	if calls != 0 {
		t.Fatalf("path seal made %d Keychain calls", calls)
	}
}

func TestT865LiveRebuiltBrokerRefused(t *testing.T) {
	if os.Getenv("CLAUDIA_OMP_LIVE") == "" {
		t.Skip("CLAUDIA_OMP_LIVE not set")
	}
	t170CopiedPathRefusal(t, liveBrokerBin(t))
}

// Execute the Store entry point in a relocated process, not a copied CLI that
// could silently delegate to an already trusted daemon over RPC.
func t170CopiedPathRefusal(t *testing.T, broker string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "relocated-store-process")
	if err := os.WriteFile(other, raw, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, other, "-test.run=^TestT170PathChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "CLAUDIA_T170_PATH_CHILD="+broker)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("relocated path guard: %v: %s", err, out)
	}
}

func TestT170PathChild(t *testing.T) {
	broker := os.Getenv("CLAUDIA_T170_PATH_CHILD")
	if broker == "" {
		return
	}
	t170AssertPathRefusal(t, broker)
}

func TestT170PathSealControls(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("refused-before-IO", func(t *testing.T) {
		t170AssertPathRefusal(t, filepath.Join(t.TempDir(), "trusted-broker"))
	})
	t.Run("relocated-process", func(t *testing.T) { t170CopiedPathRefusal(t, self) })
	t.Run("matching-path", func(t *testing.T) {
		if err := rejectUntrustedBroker(Store{BrokerPath: self, SealPath: true}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("flush-refused-before-IO", func(t *testing.T) {
		resetKeychainShot()
		t.Cleanup(resetKeychainShot)
		shot.opened = true
		shot.initial = Item{Records: map[string]Record{}}
		shot.item = Item{Records: map[string]Record{Anthropic: {AccessToken: "synthetic"}}}
		store := Store{BrokerPath: filepath.Join(t.TempDir(), "trusted-broker"), SealPath: true,
			Run: func(context.Context, string, ...string) ([]byte, error) {
				t.Fatal("refused Flush must not call Keychain")
				return nil, nil
			},
			RunStdin: func(context.Context, []byte, string, ...string) ([]byte, error) {
				t.Fatal("refused Flush must not write Keychain")
				return nil, nil
			},
		}
		var refusal *BrokerPathError
		if err := Flush(context.Background(), store); !errors.As(err, &refusal) {
			t.Fatalf("want typed Flush refusal, got %v", err)
		}
	})
	t.Run("disabled-seal", func(t *testing.T) {
		if err := rejectUntrustedBroker(Store{BrokerPath: "/other", SealPath: false}); err != nil {
			t.Fatal(err)
		}
	})
}
