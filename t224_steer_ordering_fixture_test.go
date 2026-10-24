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

type steerOrderingEvent struct {
	ModelCall      int  `json:"modelCall"`
	SteerInContext bool `json:"steerInContext"`
	ToolResults    int  `json:"toolResults"`
	IssuingTool2   bool `json:"issuingTool2"`
}

// TestT224SteerOrderingFixture is the decisive 🎯T224 oracle: after native
// tool1 completes, steer is delivered and we record whether it was ABSORBED
// into provider context before native tool2 starts in the same stream.
func TestT224SteerOrderingFixture(t *testing.T) {
	const steerNeedle = "STEER_INSTRUCTION_T224_ORDERING"
	ctx, sock, dir := startSteerOrderingSidecar(t)
	conn, err := omp.Dial(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	spec, _ := json.Marshal(map[string]string{"dir": dir})
	load := omp.Message{
		Op: omp.OpLoad, Seat: "ord-seat", Provider: omp.Anthropic, Model: "claude-sonnet-5",
		Token: "fixture", Cwd: dir, SessionID: "gen",
	}
	if err := conn.Send(load); err != nil {
		t.Fatal(err)
	}
	if ev, err := conn.Recv(); err != nil || ev.Type != "ready" {
		t.Fatalf("load = %+v %v", ev, err)
	}
	prompt := omp.Message{
		Op: omp.OpPrompt, Seat: "ord-seat", Text: "STEER_ORDERING_FIXTURE " + string(spec),
		RequestID: "r-ord", SessionID: "gen", TurnID: "turn-ord", AttemptID: "attempt-ord",
	}
	if err := conn.Send(prompt); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "tool1 entered", func() bool {
		_, err := os.Stat(filepath.Join(dir, "tool1-entered"))
		return err == nil
	})
	if err := os.WriteFile(filepath.Join(dir, "release"), []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "tool1 model result observed", func() bool {
		return fileContains(dir, "events.jsonl", `"toolResults":1`)
	})
	steerAt := time.Now()
	if err := conn.Send(omp.Message{
		Op: omp.OpSteer, Seat: "ord-seat", Text: steerNeedle,
		RequestID: "steer-ord", SessionID: "gen", TurnID: "turn-ord", AttemptID: "steer-ord-a",
	}); err != nil {
		t.Fatal(err)
	}
	queued := false
	for time.Since(steerAt) < 3*time.Second {
		ev, err := conn.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == "accepted" && ev.RequestID == "steer-ord" {
			queued = true
			break
		}
	}
	if !queued {
		t.Fatal("steer was not QUEUED (no accepted receipt on wire)")
	}
	waitFor(t, "tool2 entered", func() bool {
		_, err := os.Stat(filepath.Join(dir, "tool2-entered"))
		return err == nil
	})
	events := readSteerOrderingEvents(t, dir)
	var tool2Issue *steerOrderingEvent
	for i := range events {
		if events[i].IssuingTool2 {
			tool2Issue = &events[i]
			break
		}
	}
	if tool2Issue == nil {
		t.Fatal("fixture never issued native tool2 in the same stream")
	}
	absorbedBeforeTool2 := tool2Issue.SteerInContext
	if absorbedBeforeTool2 {
		t.Log("T224 verdict: steer ABSORBED in provider context before native tool2 started")
	} else {
		// Decisive oracle: documents current gap (QUEUED on wire ≠ in model context before tool2).
		t.Log("T224 verdict: steer QUEUED on wire but NOT absorbed before native tool2 model call")
	}
}

func startSteerOrderingSidecar(t *testing.T) (context.Context, string, string) {
	t.Helper()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("fixture integration requires bun")
	}
	ctx, cancel := context.WithCancel(t.Context())
	dir := t.TempDir()
	socketDir, err := os.MkdirTemp(os.TempDir(), "ord-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir); cancel() })
	sock := filepath.Join(socketDir, "s.sock")
	script, _ := filepath.Abs("sidecar/server.ts")
	if err := omp.EnsureDeps(ctx, script); err != nil {
		t.Fatal(err)
	}
	entry, _ := filepath.Abs("sidecar/testdata/steer-ordering-provider.ts")
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
	waitFor(t, "steer-ordering sidecar listening", func() bool { return omp.Listening(ctx, sock) })
	return ctx, sock, dir
}

func fileContains(dir, name, needle string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	return err == nil && strings.Contains(string(raw), needle)
}

func readSteerOrderingEvents(t *testing.T, dir string) []steerOrderingEvent {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []steerOrderingEvent
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var ev steerOrderingEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("events.jsonl line: %v", err)
		}
		out = append(out, ev)
	}
	return out
}
