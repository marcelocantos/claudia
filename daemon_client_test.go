// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/daemon"
)

// TestLookupChainFallback verifies that LookupChain returns
// ErrDaemonUnavailable when no daemon is running. It temporarily
// points XDG_STATE_HOME at a scratch directory so the dial target
// is a path that definitely isn't being served.
func TestLookupChainFallback(t *testing.T) {
	scratch := t.TempDir()
	t.Setenv("XDG_STATE_HOME", scratch)

	_, _, err := LookupChain("sid-unknown")
	if !errors.Is(err, ErrDaemonUnavailable) {
		t.Fatalf("expected ErrDaemonUnavailable, got %v", err)
	}
}

// TestLookupChainRoundTrip starts a daemon on a short /tmp path,
// registers a session via the client, and calls LookupChain — it
// should return the chain via the real client → real daemon path.
func TestLookupChainRoundTrip(t *testing.T) {
	// Short socket path because macOS limits sun_path to ~104 chars
	// and t.TempDir lives under /var/folders/... — too long.
	sockDir, err := os.MkdirTemp("/tmp", "cdt-")
	if err != nil {
		t.Fatalf("mkdir temp sock dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "d.sock")

	// Point DaemonSocketPath at our scratch socket.
	t.Setenv("XDG_STATE_HOME", sockDir)
	// claudiad writes to $XDG_STATE_HOME/claudia/claudiad.sock — so
	// point the server at the same location by leaving SocketPath
	// empty and letting daemonproto.SocketPath() resolve it. But we
	// also want a short path, so override explicitly with sockPath
	// and point XDG to the dir containing sockPath's parent.
	// Simpler: pass SocketPath directly; override XDG so the client
	// dials the same place via daemonproto.SocketPath().
	_ = sockPath // unused; we use the XDG-derived path below

	projects := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(projects, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}

	srv, err := daemon.NewServer(daemon.Config{
		// Leaving SocketPath empty makes the server resolve it via
		// daemonproto.SocketPath() using our overridden XDG_STATE_HOME
		// — which means the library client will dial the same path.
		ProjectsDir: projects,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})

	// Register a session via a one-shot client dial.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := dialDaemon(ctx)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Use our own pid so the default liveness check sees it as alive.
	if err := client.registerSession(ctx, "/tmp/round", "sid-first", os.Getpid()); err != nil {
		t.Fatalf("register: %v", err)
	}
	client.Close()

	// Public LookupChain should find the chain.
	chain, conf, err := LookupChain("sid-first")
	if err != nil {
		t.Fatalf("LookupChain: %v", err)
	}
	if len(chain) != 1 || chain[0] != "sid-first" {
		t.Fatalf("expected [sid-first], got %v", chain)
	}
	if conf == "" {
		t.Fatalf("expected non-empty confidence")
	}
}
