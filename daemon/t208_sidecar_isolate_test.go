// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/omp"
)

// 🎯T208: a broker with its own -state-dir starts a sidecar under that tree
// and does not disturb a sidecar already serving another socket (hermetic
// acceptance #3). Uses only temp HOME trees — never ~/.local/state/claudia.
func TestT208StateDirBrokerIsolatesSidecar(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun not installed")
	}
	if _, err := os.Stat(omp.ServerScript()); err != nil {
		t.Skip("sidecar/server.ts missing")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	sidecarScript := filepath.Join(filepath.Dir(file), "..", "sidecar", "server.ts")
	t.Setenv("CLAUDIA_OMP_SERVER", sidecarScript)
	if err := omp.EnsureDeps(t.Context(), sidecarScript); err != nil {
		t.Fatal(err)
	}

	root, err := os.MkdirTemp("/tmp", "t208-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	prodHome := filepath.Join(root, "prod-home")
	prodState := filepath.Join(prodHome, ".local", "state", "claudia")
	if err := os.MkdirAll(prodState, 0o700); err != nil {
		t.Fatal(err)
	}
	prodSocket, err := omp.SocketInStateDir(prodState)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", prodHome)
	t.Setenv(omp.SocketEnv, prodSocket)
	ctx := t.Context()
	if _, err := omp.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	prodPID := sidecarPID(t, prodSocket)
	t.Cleanup(func() { _ = omp.StopSidecar(prodSocket) })

	isolatedState := filepath.Join(root, "isolated-state")
	isolatedSocket, err := omp.SocketInStateDir(isolatedState)
	if err != nil {
		t.Fatal(err)
	}
	isolatedHome := filepath.Join(root, "isolated-home")
	t.Setenv("HOME", isolatedHome)
	t.Setenv(omp.SocketEnv, "")
	if err := omp.EnsureSocketEnv(isolatedState); err != nil {
		t.Fatal(err)
	}

	d, err := New(Options{
		SocketPath:     filepath.Join(root, "broker.sock"),
		StateDir:       isolatedState,
		UsageFetch:     func(context.Context) ([]claudia.PlanUsage, error) { return nil, nil },
		DisableResume:  true,
		DisableIntel:   true,
		DisableMCPHost: true,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		StartSidecar: func(ctx context.Context) error {
			_, err := omp.Ensure(ctx)
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = omp.StopSidecar(isolatedSocket)
		_ = d.Close()
	})

	if got := strings.TrimSpace(os.Getenv(omp.SocketEnv)); got != isolatedSocket {
		t.Fatalf("CLAUDIA_OMP_SOCKET = %q, want %q", got, isolatedSocket)
	}
	if !omp.Listening(ctx, isolatedSocket) {
		t.Fatal("isolated broker's sidecar is not listening")
	}
	for _, rel := range []string{"omp.sock", "omp.sock.pid", "omp-sidecar.log"} {
		if _, err := os.Stat(filepath.Join(isolatedState, rel)); err != nil {
			t.Fatalf("expected %s under isolated state dir: %v", rel, err)
		}
	}
	if got := sidecarPID(t, prodSocket); got != prodPID {
		t.Fatalf("production sidecar pid = %d, want unchanged %d", got, prodPID)
	}
	if syscall.Kill(prodPID, 0) != nil {
		t.Fatalf("production sidecar %d is not alive", prodPID)
	}
	defaultTree := filepath.Join(isolatedHome, ".local", "state", "claudia")
	if _, err := os.Stat(defaultTree); err == nil {
		t.Fatalf("isolated broker wrote the default state tree %s", defaultTree)
	}
}
