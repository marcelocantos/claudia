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
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// TestT224SteerMidstreamFixture records QUEUED vs ABSORBED while a native
// Bash tool is in-flight on the real sidecar/server path (bounded repro).
func TestT224SteerMidstreamFixture(t *testing.T) {
	ctx, sock, dir := startSteerMidstreamSidecar(t)
	conn, err := omp.Dial(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	spec, _ := json.Marshal(map[string]string{"dir": dir})
	load := omp.Message{
		Op: omp.OpLoad, Seat: "sm-seat", Provider: omp.Anthropic, Model: "claude-sonnet-5",
		Token: "fixture", Cwd: dir, SessionID: "gen",
	}
	if err := conn.Send(load); err != nil {
		t.Fatal(err)
	}
	if ev, err := conn.Recv(); err != nil || ev.Type != "ready" {
		t.Fatalf("load = %+v %v", ev, err)
	}
	prompt := omp.Message{
		Op: omp.OpPrompt, Seat: "sm-seat", Text: "STEER_MIDSTREAM_FIXTURE " + string(spec),
		RequestID: "r-sm", SessionID: "gen", TurnID: "turn-sm", AttemptID: "attempt-sm",
	}
	if err := conn.Send(prompt); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "bash tool entered", func() bool {
		_, err := os.Stat(filepath.Join(dir, "tool-entered"))
		return err == nil
	})
	steerAt := time.Now()
	if err := conn.Send(omp.Message{
		Op: omp.OpSteer, Seat: "sm-seat", Text: steerNeedleT224,
		RequestID: "steer-r", SessionID: "gen", TurnID: "turn-sm", AttemptID: "steer-a",
	}); err != nil {
		t.Fatal(err)
	}
	queuedAt := time.Time{}
	for time.Since(steerAt) < 3*time.Second {
		ev, err := conn.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == "accepted" && ev.RequestID == "steer-r" {
			queuedAt = time.Now()
			break
		}
	}
	if queuedAt.IsZero() {
		t.Fatal("steer was not QUEUED (no accepted receipt on wire)")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if strings.Contains(string(raw), steerNeedleT224) {
		t.Fatal("ABSORBED mid-stream: steer appeared in model context before tool finished")
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn completes after tool release", func() bool {
		raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
		return err == nil && strings.Count(string(raw), "toolResults") >= 1
	})
	raw, _ = os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if !strings.Contains(string(raw), steerNeedleT224) {
		t.Log("T224 finding: steer QUEUED during tool; ABSORBED only after tool completion (if at all in this fixture)")
	}
	t.Logf("T224 timestamps: steer_sent=%s queued=%s delta_ms=%d", steerAt.Format(time.RFC3339Nano), queuedAt.Format(time.RFC3339Nano), queuedAt.Sub(steerAt).Milliseconds())
}

const steerNeedleT224 = "STEER_INSTRUCTION_T224"

func startSteerMidstreamSidecar(t *testing.T) (context.Context, string, string) {
	t.Helper()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("fixture integration requires bun")
	}
	ctx, cancel := context.WithCancel(t.Context())
	dir := t.TempDir()
	socketDir, err := os.MkdirTemp(os.TempDir(), "sm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir); cancel() })
	sock := filepath.Join(socketDir, "s.sock")
	script, _ := filepath.Abs("sidecar/server.ts")
	if err := omp.EnsureDeps(ctx, script); err != nil {
		t.Fatal(err)
	}
	entry, _ := filepath.Abs("sidecar/testdata/steer-midstream-provider.ts")
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
	waitFor(t, "steer-midstream sidecar listening", func() bool { return omp.Listening(ctx, sock) })
	return ctx, sock, dir
}
