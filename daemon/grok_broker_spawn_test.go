// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia"
)

// TestBrokerGrokStartSurfacesSidecarHandshake is the broker acquire path:
// claudia.Start dials this daemon, the daemon spawns ProviderGrok with the
// real start function, and a helper that answers the ready handshake with
// "error" comes back as agent_failed plus an operator diagnosis.
func TestBrokerGrokStartSurfacesSidecarHandshake(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake grok uses a POSIX shell")
	}
	bin := filepath.Join(t.TempDir(), "grok")
	script := "#!/bin/sh\necho 'omp: sidecar said \"error\", want ready' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_BIN", bin)
	t.Setenv(claudia.EnvGrokConnect, "")

	f := newFixture(t)
	opts := f.options(nil)
	opts.launchers = nil
	opts.DisableResume = true
	opts.DisableMCPHost = true
	opts.DisableIntel = true
	f.bootWith(t, opts)

	_, err := claudia.Start(claudia.Config{
		Name:        "grok-sidecar",
		Provider:    claudia.ProviderGrok,
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err == nil {
		t.Fatal("broker Start succeeded; want the sidecar handshake failure")
	}
	msg := err.Error()
	for _, want := range []string{
		"agent_failed",
		"omp",
		`sidecar said "error", want ready`,
		"brew services restart claudia",
		"~/.grok/bin",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

// TestBrokerCursorStartSurfacesSidecarHandshake is the same omp refusal on
// a Cursor broker seat. ProviderClaude and ProviderCodex do not start omp.
func TestBrokerCursorStartSurfacesSidecarHandshake(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake cursor agent uses a POSIX shell")
	}
	bin := filepath.Join(t.TempDir(), "agent")
	script := "#!/bin/sh\necho 'omp: sidecar said \"error\", want ready' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CURSOR_BIN", bin)

	f := newFixture(t)
	opts := f.options(nil)
	opts.launchers = nil
	opts.DisableResume = true
	opts.DisableMCPHost = true
	opts.DisableIntel = true
	f.bootWith(t, opts)

	_, err := claudia.Start(claudia.Config{
		Name:        "cursor-sidecar",
		Provider:    claudia.ProviderCursor,
		WorkDir:     t.TempDir(),
		TermLogPath: "-",
	})
	if err == nil {
		t.Fatal("broker Start succeeded; want the sidecar handshake failure")
	}
	msg := err.Error()
	for _, want := range []string{
		"agent_failed",
		"omp",
		`sidecar said "error", want ready`,
		"setsid",
		"Cursor",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}
