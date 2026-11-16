// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// make test-compiled-sidecar runs this from the standing sidecar gate. It
// exercises Ensure's actual executable branch with no Bun on PATH, an empty
// HOME, and synthetic tokens. No prompt or provider request is submitted.
func TestCompiledSidecarLoadsProvidersWithoutBun(t *testing.T) {
	bin := os.Getenv("CLAUDIA_TEST_COMPILED_DIR")
	if bin == "" {
		t.Skip("run make test-compiled-sidecar to build the native helpers")
	}
	dir, err := os.MkdirTemp("", "omp-compiled-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	t.Setenv(SocketEnv, socket)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("CLAUDIA_OMP_SERVER", filepath.Join(bin, "claudia-omp-server"))
	t.Setenv("CLAUDIA_OMP_SPOOL_DIR", filepath.Join(dir, "spool"))
	if _, err := Ensure(t.Context()); err != nil {
		log, _ := os.ReadFile(filepath.Join(dir, "omp-sidecar.log"))
		t.Fatalf("Ensure compiled helper: %v\n%s", err, log)
	}
	t.Cleanup(func() { _ = StopSidecar(socket) })
	pid := t145Pid(t, socket)
	if parent := t166Parent(t, pid); parent != os.Getpid() {
		t.Fatalf("sidecar parent = %d, want broker %d", parent, os.Getpid())
	}
	if !CursorExecWired() {
		t.Fatal("compiled sidecar lost Cursor native tool capability")
	}
	conn, err := Dial(t.Context(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, tc := range []struct{ provider, model string }{
		{Anthropic, "claude-sonnet-4-6"},
		{OpenAICodex, "gpt-5.5"},
		{XAIOAuth, "grok-4.6"},
		{Cursor, "default"},
	} {
		if err := conn.Send(Message{Op: OpLoad, Seat: tc.provider, Provider: tc.provider, Model: tc.model, Token: "synthetic", Cwd: dir}); err != nil {
			t.Fatal(err)
		}
		ev, err := conn.Recv()
		if err != nil || ev.Type != "ready" {
			t.Fatalf("compiled %s load: %+v, %v", tc.provider, ev, err)
		}
	}
}

func TestCompiledSidecarLifeline(t *testing.T) {
	bin := os.Getenv("CLAUDIA_TEST_COMPILED_DIR")
	if bin == "" {
		t.Skip("run make test-compiled-sidecar to build the native helpers")
	}
	dir, err := os.MkdirTemp("", "omp-compiled-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("HOME", dir)
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("CLAUDIA_OMP_SPOOL_DIR", filepath.Join(dir, "spool"))
	socket := filepath.Join(dir, "s")
	cmd := exec.CommandContext(t.Context(), filepath.Join(bin, "claudia-omp-server"), socket, LifelineArg)
	hold, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	for !Listening(t.Context(), socket) {
		if err := t.Context().Err(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := hold.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("compiled sidecar did not exit cleanly on lifeline EOF: %v", err)
	}
	if Listening(t.Context(), socket) {
		t.Fatal("compiled sidecar still serves after lifeline EOF")
	}
}
