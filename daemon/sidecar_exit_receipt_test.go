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
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/controlplane"
	"github.com/marcelocantos/claudia/omp"
)

// TestServingSidecarExitCancelsStartedReceipt drives daemon.New, which binds
// the publisher, and the production StartSidecar path, which calls
// omp.Ensure. Killing that sidecar while the daemon is still open must
// cancel a receipt the sidecar left started. An accepted receipt stays.
// Closing the daemon is not what performs this cancel.
func TestServingSidecarExitCancelsStartedReceipt(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun not installed")
	}
	if _, err := os.Stat(omp.ServerScript()); err != nil {
		t.Skip("sidecar/server.ts missing")
	}
	if err := omp.EnsureDeps(t.Context(), omp.ServerScript()); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "sc-exit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "omp.sock")
	t.Setenv(omp.SocketEnv, socket)
	t.Setenv("CLAUDIA_OMP_SPOOL_DIR", filepath.Join(dir, "spool"))
	t.Setenv("CLAUDIA_OMP_NO_REFRESH", "1")

	path := filepath.Join(dir, controlplane.ClaudiaDBFile)
	brokerSock := filepath.Join(dir, "broker.sock")
	d, err := New(Options{
		SocketPath:     brokerSock,
		StateDir:       dir,
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
		if raw, err := os.ReadFile(socket + ".pid"); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 1 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		_ = d.Close()
	})

	stamp := "2026-10-18T01:00:00.000000000Z"
	started := &claudia.RequestReceipt{
		RequestID: "logical-S", SessionID: "generation", Seat: "seat",
		State: claudia.ReceiptStarted, AttemptID: "attempt-S", TurnIDs: []string{"turn-S"},
		AcceptedAt: stamp, UpdatedAt: stamp,
	}
	accepted := &claudia.RequestReceipt{
		RequestID: "logical-A", SessionID: "generation", Seat: "seat",
		State: claudia.ReceiptAccepted, AttemptID: "attempt-A", TurnIDs: []string{},
		AcceptedAt: stamp, UpdatedAt: stamp,
	}
	claudia.RecordObservedReceipt(started)
	claudia.RecordObservedReceipt(accepted)
	recordUsagePair(t)

	pid := sidecarPID(t, socket)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var got claudia.RequestReceipt
	for {
		got = readStoredReceipt(t, path, started.Seat, started.SessionID, started.RequestID)
		if got.State == claudia.ReceiptCancelled {
			break
		}
		// 🎯T97 exemption: a failsafe on the receipt becoming cancelled. The stored state is the verdict; the clock only turns a hang into a failure.
		if time.Now().After(deadline) {
			t.Fatalf("started receipt after sidecar kill = %+v, want cancelled", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Cause != claudia.ReceiptCauseCancellation || got.Reason != controlplane.SidecarExitReceiptReason {
		t.Fatalf("cancelled receipt = %+v", got)
	}
	if got.AttemptID != started.AttemptID || len(got.TurnIDs) != 1 || got.TurnIDs[0] != "turn-S" {
		t.Fatalf("identity after sidecar exit = %+v", got)
	}
	if acc := readStoredReceipt(t, path, accepted.Seat, accepted.SessionID, accepted.RequestID); acc.State != claudia.ReceiptAccepted || acc.Cause != "" {
		t.Fatalf("accepted receipt after sidecar exit = %+v", acc)
	}
	assertUsageSettled(t, path, controlplane.SidecarExitReceiptReason)
	// The daemon is still the publisher. A later receipt lands without
	// closing it, which is the opposite of broker shutdown.
	later := &claudia.RequestReceipt{
		RequestID: "logical-L", SessionID: "generation", Seat: "seat",
		State: claudia.ReceiptAccepted, AttemptID: "attempt-L", TurnIDs: []string{},
		AcceptedAt: stamp, UpdatedAt: stamp,
	}
	claudia.RecordObservedReceipt(later)
	if n := countReceipts(t, path, later.RequestID); n != 1 {
		t.Fatalf("receipt while daemon still open = %d", n)
	}
}

func sidecarPID(t *testing.T, socket string) int {
	t.Helper()
	raw, err := os.ReadFile(socket + ".pid")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		t.Fatalf("sidecar pid %q", raw)
	}
	return pid
}
