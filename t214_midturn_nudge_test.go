// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/omp"
)

// 🎯T214: ResumeAll nudges only OMP seats whose restored transcript ended mid-turn.
func TestT214ResumeAllNudgesOnlyMidTurnOMPSeats(t *testing.T) {
	ctx, socketDir := startT214Sidecar(t)
	t.Setenv(broker.NoBrokerEnv, "1")

	const usage = `{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}}`
	writeOMPStore(t, socketDir, "idle-seat", "sid-idle",
		`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-10-22T00:00:00.000Z","message":{"role":"user","content":"done","timestamp":1}}`,
		fmt.Sprintf(`{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-10-22T00:00:01.000Z","message":{"role":"assistant","content":[{"type":"text","text":"finished"}],"api":"anthropic-messages","provider":"anthropic","model":"claude-sonnet-5","stopReason":"stop","timestamp":2,"usage":%s}}`, usage),
	)
	writeOMPStore(t, socketDir, "mid-seat", "sid-mid",
		`{"type":"message","id":"u2","parentId":null,"timestamp":"2026-10-22T00:00:00.000Z","message":{"role":"user","content":"go","timestamp":1}}`,
		fmt.Sprintf(`{"type":"message","id":"a2","parentId":"u2","timestamp":"2026-10-22T00:00:01.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"Read","arguments":{"path":"x"}}],"api":"anthropic-messages","provider":"anthropic","model":"claude-sonnet-5","stopReason":"toolUse","timestamp":2,"usage":%s}}`, usage),
	)

	root := t.TempDir()
	path := filepath.Join(root, "agents.json")
	reg, err := NewRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.StopAll)
	work := root
	for _, def := range []AgentDef{
		{Name: "idle-seat", SessionID: "sid-idle", Provider: ProviderClaude, Model: "claude-sonnet-5", WorkDir: work, TermLogPath: "-", AutoStart: true, Materialized: true, OMP: true},
		{Name: "mid-seat", SessionID: "sid-mid", Provider: ProviderClaude, Model: "claude-sonnet-5", WorkDir: work, TermLogPath: "-", AutoStart: true, Materialized: true, OMP: true},
	} {
		if err := reg.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 22, 18, 29, 0, 0, time.UTC)
	out := reg.ResumeAll(ctx, &ResumeArgs{Now: now, Concurrency: 2, NudgeSpacing: 0})
	byName := map[string]ResumeOutcome{}
	for _, o := range out {
		byName[o.Name] = o
	}
	if len(byName) != 2 {
		t.Fatalf("outcomes = %+v", out)
	}
	if byName["idle-seat"].Err != nil || byName["mid-seat"].Err != nil {
		t.Fatalf("resume errors: idle=%v mid=%v", byName["idle-seat"].Err, byName["mid-seat"].Err)
	}
	if byName["idle-seat"].Nudged {
		t.Fatal("idle-seat was nudged")
	}
	if !byName["mid-seat"].Nudged {
		t.Fatal("mid-seat was not nudged")
	}
}

func startT214Sidecar(t *testing.T) (context.Context, string) {
	t.Helper()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("fixture integration requires bun")
	}
	ctx, cancel := context.WithCancel(t.Context())
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	socketDir, err := os.MkdirTemp(os.TempDir(), "t214-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	sock := filepath.Join(socketDir, "s.sock")
	script, _ := filepath.Abs("sidecar/server.ts")
	if err := omp.EnsureDeps(ctx, script); err != nil {
		t.Fatal(err)
	}
	entry, _ := filepath.Abs("sidecar/testdata/abort-provider.ts")
	cmd := exec.CommandContext(ctx, bun, entry, sock, "--lifeline=stdin")
	cmd.Env = append(omp.ScrubEnv(os.Environ()), "CLAUDIA_OMP_SPOOL_DIR="+filepath.Join(dir, "spool"))
	var childLog bytes.Buffer
	cmd.Stdout, cmd.Stderr = &childLog, &childLog
	life, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = life.Close()
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			t.Log(childLog.String())
		}
	})
	waitFor(t, "disposable sidecar listening", func() bool { return omp.Listening(ctx, sock) })
	if api, err := os.ReadFile(filepath.Join(socketDir, "mock-api-ready")); err != nil || string(api) != "mock\n" {
		t.Fatalf("fixture did not select the mock API: %q %v", api, err)
	}
	t.Setenv(omp.SocketEnv, sock)
	var refreshes atomic.Int32
	t141Plan(t, "fixture", &refreshes, "fixture")
	return ctx, socketDir
}

func writeOMPStore(t *testing.T, socketDir, seat, session string, lines ...string) {
	t.Helper()
	dir := filepath.Join(socketDir, "omp-seats", seat)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	header := fmt.Sprintf("{\"type\":\"claudia_seat\",\"v\":1,\"seat\":%q,\"session\":%q}\n", seat, session)
	var body string
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, session+".jsonl"), []byte(header+body), 0o600); err != nil {
		t.Fatal(err)
	}
}
