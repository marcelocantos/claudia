// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// TestT223SidecarServerProcessLifetimeFixture is the 🎯T223 Slice B oracle:
// real sidecar/server.ts, real child+grandchild through production seat Bash,
// not runCommand unit tests alone.
func TestT223SidecarServerProcessLifetimeFixture(t *testing.T) {
	t.Run("consumer_disconnect_continues_seat_owned_work", func(t *testing.T) {
		ctx, sock, sidecar, supervisor, dir := startProcessLifetimeSidecar(t)
		grand, parent, conn := runProcessLifetimeHold(t, ctx, sock, dir)
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(150 * time.Millisecond)
		if !ProcessAlive(grand) {
			t.Fatalf("grandchild %d died on consumer disconnect", grand)
		}
		if !ProcessAlive(parent) {
			t.Fatalf("seat-owned parent %d died on consumer disconnect", parent)
		}
		if !ProcessAlive(supervisor) {
			t.Fatal("unrelated supervisor job died on consumer disconnect")
		}
		if !ProcessAlive(sidecar.Process.Pid) {
			t.Fatal("sidecar supervisor died on consumer disconnect")
		}
		second, err := omp.Dial(ctx, sock)
		if err != nil {
			t.Fatal(err)
		}
		defer second.Close()
		if err := second.Send(omp.Message{Op: omp.OpAdopt, Seat: plSeat, Provider: omp.Anthropic, Token: "fixture", SessionID: plSession}); err != nil {
			t.Fatal(err)
		}
		ev, err := second.Recv()
		if err != nil || ev.Type != "ready" {
			t.Fatalf("adopt after disconnect = %+v %v", ev, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "release"), []byte("go\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "hold bash finished after release", func() bool {
			_, err := os.Stat(filepath.Join(dir, "effect"))
			return err == nil
		})
		if !ProcessAlive(supervisor) || !ProcessAlive(sidecar.Process.Pid) {
			t.Fatal("supervisor or sidecar died while seat work finished after reconnect")
		}
	})

	t.Run("seat_abort_reaps_owned_process_group", func(t *testing.T) {
		ctx, sock, sidecar, supervisor, dir := startProcessLifetimeSidecar(t)
		grand, parent, conn := runProcessLifetimeHold(t, ctx, sock, dir)
		if err := conn.Send(omp.Message{Op: omp.OpAbort, Seat: plSeat}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "grandchild reaped on seat abort", func() bool { return !ProcessAlive(grand) })
		if ProcessAlive(parent) {
			t.Fatalf("seat-owned parent %d still alive after abort", parent)
		}
		if !ProcessAlive(supervisor) || !ProcessAlive(sidecar.Process.Pid) {
			t.Fatal("seat abort must not kill broker supervisor or sidecar")
		}
	})

	t.Run("tool_maxbuffer_reaps_owned_process_group", func(t *testing.T) {
		ctx, sock, sidecar, supervisor, dir := startProcessLifetimeSidecar(t)
		grand, conn := runProcessLifetimeFlood(t, ctx, sock, dir)
		_ = conn.Close()
		waitFor(t, "grandchild reaped on maxBuffer", func() bool { return !ProcessAlive(grand) })
		if !ProcessAlive(supervisor) || !ProcessAlive(sidecar.Process.Pid) {
			t.Fatal("tool maxBuffer abort must not kill broker supervisor or sidecar")
		}
	})

	t.Run("seat_op_drop_reaps_owned_process_group", func(t *testing.T) {
		ctx, sock, sidecar, supervisor, dir := startProcessLifetimeSidecar(t)
		grand, parent, conn := runProcessLifetimeHold(t, ctx, sock, dir)
		if err := conn.Send(omp.Message{Op: omp.OpDrop, Seat: plSeat}); err != nil {
			t.Fatal(err)
		}
		dropped := false
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			ev, err := conn.Recv()
			if err != nil {
				break
			}
			if ev.Type == "dropped" && ev.Seat == plSeat {
				dropped = true
				break
			}
		}
		if !dropped {
			t.Fatal("seat OpDrop did not emit dropped on wire (production stop/reap path)")
		}
		waitFor(t, "grandchild reaped on seat drop", func() bool { return !ProcessAlive(grand) })
		waitFor(t, "seat-owned parent reaped on seat drop", func() bool { return !ProcessAlive(parent) })
		if !ProcessAlive(supervisor) || !ProcessAlive(sidecar.Process.Pid) {
			t.Fatal("seat OpDrop must not kill broker supervisor or sidecar")
		}
	})

	t.Run("leader_exit_redirected_child_survives_until_seat_stop", func(t *testing.T) {
		runLeaderExitOwnershipStop(t, omp.OpDrop)
	})

	t.Run("leader_exit_redirected_child_survives_until_seat_abort", func(t *testing.T) {
		runLeaderExitOwnershipStop(t, omp.OpAbort)
	})
}

const plSeat = "pl-fixture"
const plSession = "generation"

func startProcessLifetimeSidecar(t *testing.T) (context.Context, string, *exec.Cmd, int, string) {
	t.Helper()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("fixture integration requires bun")
	}
	ctx, cancel := context.WithCancel(t.Context())
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	socketDir, err := os.MkdirTemp(os.TempDir(), "pl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir); cancel() })
	sock := filepath.Join(socketDir, "s.sock")
	script, _ := filepath.Abs("sidecar/server.ts")
	if err := omp.EnsureDeps(ctx, script); err != nil {
		t.Fatal(err)
	}
	entry, _ := filepath.Abs("sidecar/testdata/process-lifetime-provider.ts")
	cmd := exec.CommandContext(ctx, bun, entry, sock)
	cmd.Env = append(omp.ScrubEnv(os.Environ()), "CLAUDIA_OMP_SPOOL_DIR="+filepath.Join(dir, "spool"))
	var childLog bytes.Buffer
	cmd.Stdout, cmd.Stderr = &childLog, &childLog
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Log(childLog.String())
		}
	})
	waitFor(t, "process-lifetime sidecar listening", func() bool { return omp.Listening(ctx, sock) })
	if api, err := os.ReadFile(filepath.Join(socketDir, "mock-api-ready")); err != nil || string(api) != "mock\n" {
		t.Fatalf("fixture did not select mock API: %q %v", api, err)
	}
	supervisor := exec.Command("sleep", "3600")
	if err := supervisor.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Process.Kill(); _ = supervisor.Wait() })
	return ctx, sock, cmd, supervisor.Process.Pid, dir
}

func processLifetimePrompt(dir string, mode string) string {
	spec, _ := json.Marshal(map[string]string{"mode": mode, "dir": dir})
	return "PROCESS_LIFETIME_FIXTURE " + string(spec)
}

func loadAndPrompt(t *testing.T, ctx context.Context, sock, dir, text string) *omp.Conn {
	t.Helper()
	conn, err := omp.Dial(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	load := omp.Message{
		Op: omp.OpLoad, Seat: plSeat, Provider: omp.Anthropic, Model: "claude-sonnet-5",
		Token: "fixture", Cwd: dir, SessionID: plSession,
	}
	if err := conn.Send(load); err != nil {
		t.Fatal(err)
	}
	ev, err := conn.Recv()
	if err != nil || ev.Type != "ready" {
		t.Fatalf("load = %+v %v", ev, err)
	}
	prompt := omp.Message{
		Op: omp.OpPrompt, Seat: plSeat, Text: text, RequestID: "r-pl", SessionID: plSession,
		TurnID: "turn-pl", AttemptID: "attempt-pl",
	}
	if err := conn.Send(prompt); err != nil {
		t.Fatal(err)
	}
	return conn
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("%s = %q", path, raw)
	}
	return pid
}

func runProcessLifetimeHold(t *testing.T, ctx context.Context, sock, dir string) (grand int, parent int, conn *omp.Conn) {
	t.Helper()
	conn = loadAndPrompt(t, ctx, sock, dir, processLifetimePrompt(dir, "hold"))
	waitFor(t, "grandchild marker", func() bool {
		_, err := os.Stat(filepath.Join(dir, "grand.pid"))
		return err == nil
	})
	waitFor(t, "parent marker", func() bool {
		_, err := os.Stat(filepath.Join(dir, "parent.pid"))
		return err == nil
	})
	grand = readPIDFile(t, filepath.Join(dir, "grand.pid"))
	parent = readPIDFile(t, filepath.Join(dir, "parent.pid"))
	time.Sleep(100 * time.Millisecond)
	return grand, parent, conn
}

// runLeaderExitOwnershipStop is the mandatory 🎯T219 ownership oracle on real
// server.ts: leader exits 0 after spawning a redirected background child; scope
// is empty while the child lives; production stop must reap it (or this fails RED).
func runLeaderExitOwnershipStop(t *testing.T, stopOp string) {
	t.Helper()
	ctx, sock, sidecar, supervisor, dir := startProcessLifetimeSidecar(t)
	conn := loadAndPrompt(t, ctx, sock, dir, processLifetimePrompt(dir, "leader_exit"))
	waitFor(t, "background child PID recorded", func() bool {
		_, err := os.Stat(filepath.Join(dir, "grand.pid"))
		return err == nil
	})
	waitFor(t, "shell leader exited 0 (tool complete)", func() bool {
		_, err := os.Stat(filepath.Join(dir, "leader.done"))
		return err == nil
	})
	waitFor(t, "provider saw tool result after leader exit", func() bool {
		raw, err := os.ReadFile(filepath.Join(dir, "provider-calls"))
		return err == nil && strings.Contains(string(raw), "result")
	})
	bgChild := readPIDFile(t, filepath.Join(dir, "grand.pid"))
	if !ProcessAlive(bgChild) {
		t.Fatalf("background child %d not alive after leader exit 0", bgChild)
	}
	leaderShell := readPIDFile(t, filepath.Join(dir, "parent.pid"))
	if ProcessAlive(leaderShell) {
		t.Fatalf("shell leader %d still alive after exit 0", leaderShell)
	}
	if _, err := os.Stat(filepath.Join(dir, "child.out")); err != nil {
		t.Fatalf("expected redirected stdout file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "child.err")); err != nil {
		t.Fatalf("expected redirected stderr file: %v", err)
	}
	if err := conn.Send(omp.Message{Op: stopOp, Seat: plSeat}); err != nil {
		t.Fatal(err)
	}
	if stopOp == omp.OpDrop { // production stop/reap wire op
		dropped := false
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			ev, err := conn.Recv()
			if err != nil {
				break
			}
			if ev.Type == "dropped" && ev.Seat == plSeat {
				dropped = true
				break
			}
		}
		if !dropped {
			t.Fatal("seat OpDrop did not emit dropped on wire")
		}
	}
	waitFor(t, "redirected background child reaped on seat stop", func() bool { return !ProcessAlive(bgChild) })
	if !ProcessAlive(supervisor) {
		t.Fatal("separately supervised harness job must survive seat stop")
	}
	if !ProcessAlive(sidecar.Process.Pid) {
		t.Fatal("sidecar broker must survive seat stop")
	}
}

func runProcessLifetimeFlood(t *testing.T, ctx context.Context, sock, dir string) (grand int, conn *omp.Conn) {
	t.Helper()
	conn = loadAndPrompt(t, ctx, sock, dir, processLifetimePrompt(dir, "flood"))
	waitFor(t, "grandchild marker", func() bool {
		_, err := os.Stat(filepath.Join(dir, "grand.pid"))
		return err == nil
	})
	grand = readPIDFile(t, filepath.Join(dir, "grand.pid"))
	return grand, conn
}
