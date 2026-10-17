// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia"
)

// armPoolTmux puts a fake tmux on PATH. Acquire must not run it.
func armPoolTmux(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cpl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("CLAUDIA_TMUX_SOCKET", filepath.Join(dir, "tmux.sock"))
	marker := filepath.Join(dir, "ran")
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

func assertPoolDidNotExec(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("pool path executed tmux: %v", err)
	}
}

// TestAcquireSharesOneWarmSeatAcrossConsumers is 🎯T64. Acquire refuses
// a CLI pool seat, so neither consumer starts a window.
func TestAcquireSharesOneWarmSeatAcrossConsumers(t *testing.T) {
	marker := armPoolTmux(t)
	work := t.TempDir()
	if _, err := claudia.Acquire(context.Background(), claudia.Config{WorkDir: work, TermLogPath: "-"}); !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("Acquire = %v, want legacy refusal", err)
	}
	if _, err := claudia.AcquireDirect(context.Background(), claudia.Config{WorkDir: work, TermLogPath: "-"}); !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("AcquireDirect = %v, want legacy refusal", err)
	}
	assertPoolDidNotExec(t, marker)
}

// TestAcquiredSeatReturnsWhenConsumerLeaves (🎯T64). A refused acquire
// leaves no held window behind.
func TestAcquiredSeatReturnsWhenConsumerLeaves(t *testing.T) {
	marker := armPoolTmux(t)
	work := t.TempDir()
	if _, err := claudia.Acquire(context.Background(), claudia.Config{WorkDir: work, TermLogPath: "-"}); !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("Acquire = %v, want legacy refusal", err)
	}
	if _, err := claudia.AcquireDirect(context.Background(), claudia.Config{WorkDir: work, TermLogPath: "-"}); !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("AcquireDirect = %v, want legacy refusal", err)
	}
	assertPoolDidNotExec(t, marker)
}

// TestAcquiredHandleOpsKeepAliveAndDirect (🎯T64). Resize, keep-alive,
// and AcquireDirect do not reach a tmux window.
func TestAcquiredHandleOpsKeepAliveAndDirect(t *testing.T) {
	marker := armPoolTmux(t)
	work := t.TempDir()
	if _, err := claudia.Acquire(context.Background(), claudia.Config{WorkDir: work, TermLogPath: "-"}); !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("Acquire = %v, want legacy refusal", err)
	}
	if _, err := claudia.AcquireDirect(context.Background(), claudia.Config{WorkDir: work, TermLogPath: "-"}); !errors.Is(err, claudia.ErrLegacyCLISeatTransport) {
		t.Fatalf("AcquireDirect = %v, want legacy refusal", err)
	}
	assertPoolDidNotExec(t, marker)
}
