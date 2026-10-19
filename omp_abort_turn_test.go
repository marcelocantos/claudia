// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

// TestOMPAbortThenSendThroughRealSidecar is jevons J3-cancel-and-send
// (2026-10-19) on the real production path: the unchanged sidecar server
// (its model catalog row replaced by the mock API, no provider transport),
// its private socket, and this package's event pump and publishOMPAnswer.
// An owner abort must end the running turn as that turn, not as an
// identity-less answer the pump refuses, and the next send must get its own
// turn and reply.
func TestOMPAbortThenSendThroughRealSidecar(t *testing.T) {
	for _, waited := range []bool{true, false} {
		name := "first-turn-awaited"
		if !waited {
			name = "first-turn-abandoned"
		}
		t.Run(name, func(t *testing.T) { ompAbortThenSend(t, waited) })
	}
}

func ompAbortThenSend(t *testing.T, waitFirst bool) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("fixture integration requires bun; no protocol evidence without it")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	socketDir, err := os.MkdirTemp(os.TempDir(), "j3-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
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
	defer func() {
		_ = life.Close()
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			t.Log(childLog.String())
		}
	}()
	waitFor(t, "disposable sidecar listening", func() bool { return omp.Listening(ctx, sock) })
	if api, err := os.ReadFile(filepath.Join(socketDir, "mock-api-ready")); err != nil || string(api) != "mock\n" {
		t.Fatalf("fixture did not select the mock API: %q %v", api, err)
	}
	t.Setenv("CLAUDIA_OMP_SOCKET", sock)
	var refreshes atomic.Int32
	t141Plan(t, "fixture", &refreshes, "fixture")

	a, err := StartDirect(Config{Name: "j3-owner", Provider: Provider(omp.Anthropic), Model: "claude-sonnet-5", WorkDir: dir, TermLogPath: "-"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop()
	if err := a.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if !a.SupportsReceipts() {
		t.Fatal("sidecar seat does not support receipts")
	}

	var mu sync.Mutex
	var events []Event
	started := make(chan string, 4)
	finals := make(chan Event, 8)
	sub := a.SubscribeEvents(func(e Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
		if e.Type == "receipt" && e.Receipt != nil && e.Receipt.State == ReceiptStarted && len(e.Receipt.TurnIDs) > 0 {
			started <- e.RequestID + "=" + e.Receipt.TurnIDs[len(e.Receipt.TurnIDs)-1]
		}
		if e.Type == "assistant" && (e.Final || e.IsError) {
			finals <- e
		}
	})
	defer a.UnsubscribeEvents(sub)

	if err := a.SendWithRequestID("HOLD the first owner message", "r1"); err != nil {
		t.Fatal(err)
	}
	var first string
	select {
	case s := <-started:
		id, turn, _ := strings.Cut(s, "=")
		if id != "r1" {
			t.Fatalf("first started receipt is %s", s)
		}
		first = turn
	case <-ctx.Done():
		t.Fatal("r1 never started")
	}
	firstDone := make(chan error, 1)
	if waitFirst {
		go func() { _, err := a.WaitForResponse(ctx); firstDone <- err }()
	}

	if err := a.Interrupt(); err != nil {
		t.Fatal(err)
	}
	// The aborted turn ends as itself, and not as an error.
	select {
	case end := <-finals:
		if end.IsError || end.TurnID != first {
			t.Fatalf("aborted turn ended as %+v; want a non-error final for turn %s", end, first)
		}
	case <-ctx.Done():
		t.Fatal("aborted turn never ended")
	}
	if waitFirst {
		select {
		case err := <-firstDone:
			if err != nil {
				t.Fatalf("the aborted turn's wait failed: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("the aborted turn's wait never returned")
		}
	}
	r1, err := a.Receipt(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if r1.State != ReceiptCancelled || r1.Cause != ReceiptCauseCancellation {
		t.Fatalf("aborted receipt = %+v, want cancelled by cancellation", r1)
	}

	if err := a.SendWithRequestID("second owner message", "r2"); err != nil {
		t.Fatal(err)
	}
	reply, err := a.WaitForResponse(ctx)
	if err != nil {
		t.Fatalf("second send: %v", err)
	}
	if !strings.Contains(reply, "reply: second owner message") || strings.Contains(reply, "aborted") {
		t.Fatalf("second send's reply = %q", reply)
	}
	r2, err := a.Receipt(ctx, "r2")
	if err != nil {
		t.Fatal(err)
	}
	if r2.State != ReceiptCompleted || len(r2.TurnIDs) != 1 || r2.TurnIDs[0] == first {
		t.Fatalf("second receipt = %+v, want completed on its own turn", r2)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, e := range events {
		if e.IsError || strings.Contains(e.Text, ErrOMPMissingTurnID.Error()) {
			t.Errorf("error event published: %+v", e)
		}
		if e.Type == "assistant" && e.TurnID == "" {
			t.Errorf("answer without turn identity: %+v", e)
		}
	}
}
