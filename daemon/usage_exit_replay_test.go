// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/controlplane"
	"github.com/marcelocantos/claudia/omp"
)

// A usage fact's seq belongs to the seat's conversation journal, and the
// control file mirrors that journal. A sidecar or broker exit is not a new
// observation of any call, so it must leave every mirrored fact exactly as
// the producer published it. A call that never finished stays started: the
// contract's "incomplete, unknown consumption". These tests replay the
// mirror the way a consumer does, before and after a real exit, through
// Claudia's own validation and transition rules.

// exitUsageFacts publishes, through the seat pump's entry point, a call left
// started (call-A), a call that finished (call-B: started then completed),
// and a second call left started (call-C).
func exitUsageFacts(t *testing.T) []claudia.UsageFact {
	t.Helper()
	base := claudia.UsageFact{
		Seat: "seat", SessionID: "generation", Kind: "model", Provider: "openai", Model: "gpt-5.5",
		Requests:      []claudia.UsageRequestRef{{RequestID: "logical-U", AttemptID: "attempt-U"}},
		StartedAt:     "2026-10-16T01:00:00Z",
		CounterSource: "adapter_normalized_positive",
		Coverage:      "engine_invocation",
	}
	at := func(seq uint64, call, state string) claudia.UsageFact {
		f := base
		f.Seq, f.CallID, f.State = seq, call, state
		if state != "started" {
			f.FinishedAt = "2026-10-16T01:00:01Z"
			in := int64(11)
			f.Counters.InputTokens = &in
		}
		return f
	}
	facts := []claudia.UsageFact{
		at(1, "call-A", "started"),
		at(2, "call-B", "started"),
		at(3, "call-B", "completed"),
		at(4, "call-C", "started"),
	}
	for i := range facts {
		claudia.RecordObservedUsage(&facts[i])
	}
	return facts
}

// storedUsageRows reads the mirror in the order a consumer catches up.
func storedUsageRows(t *testing.T, path string) []claudia.UsageFact {
	t.Helper()
	db := openControlReader(t, path)
	defer db.Close()
	qr, err := db.Query(`SELECT fact_json FROM claudia_usage_fact ORDER BY seat, session_id, seq`)
	if err != nil {
		t.Fatal(err)
	}
	var out []claudia.UsageFact
	for _, row := range qr.Rows {
		body, ok := row[0].(string)
		if !ok {
			t.Fatalf("fact_json type %T", row[0])
		}
		var f claudia.UsageFact
		if err := json.Unmarshal([]byte(body), &f); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

// replayUsage applies rows to a per-call projection with Claudia's own
// transition contract, the projection every consumer is told to keep.
func replayUsage(projection map[string]claudia.UsageFact, rows []claudia.UsageFact) error {
	for i := range rows {
		f := rows[i]
		var previous *claudia.UsageFact
		if old, ok := projection[f.CallID]; ok {
			previous = &old
		}
		apply, err := claudia.ValidateUsageTransition(previous, &f)
		if err != nil {
			return fmt.Errorf("seq %d call %s (%s): %w", f.Seq, f.CallID, f.State, err)
		}
		if apply {
			projection[f.CallID] = f
		}
	}
	return nil
}

// assertUsageReplayable is the oracle: the rows after exit are the rows the
// producer published, form a valid contiguous page, replay cleanly from
// scratch, and replay cleanly on top of a consumer that already ingested
// the rows from before the exit. Calls left started stay started.
func assertUsageReplayable(t *testing.T, published, before, after []claudia.UsageFact) {
	t.Helper()
	page := claudia.UsagePage{Seat: "seat", SessionID: "generation", Facts: after, Next: uint64(len(after))}
	if err := page.Validate("seat", "generation", 0, claudia.UsagePageLimit); err != nil {
		t.Fatalf("mirror after exit is not a valid usage page: %v", err)
	}
	if err := replayUsage(map[string]claudia.UsageFact{}, after); err != nil {
		t.Fatalf("fresh replay of the mirror after exit: %v", err)
	}
	consumer := map[string]claudia.UsageFact{}
	if err := replayUsage(consumer, before); err != nil {
		t.Fatalf("replay before exit: %v", err)
	}
	if err := replayUsage(consumer, after); err != nil {
		t.Fatalf("consumer catch-up across exit: %v", err)
	}
	if !reflect.DeepEqual(after[:len(published)], published) {
		t.Fatalf("exit rewrote mirrored usage facts:\n got %+v\nwant %+v", after[:len(published)], published)
	}
	for _, call := range []string{"call-A", "call-C"} {
		if got := consumer[call]; got.State != "started" || got.FinishedAt != "" || got.Cause != "" {
			t.Fatalf("abandoned %s = %+v, want started (incomplete, unknown consumption)", call, got)
		}
	}
	if got := consumer["call-B"]; got.State != "completed" || got.Seq != 3 {
		t.Fatalf("finished call-B = %+v", got)
	}
}

// TestSidecarExitLeavesUsageReplayable kills the real serving sidecar under
// daemon.New's publisher. The exit hook still settles the started receipt;
// it must not touch usage facts.
func TestSidecarExitLeavesUsageReplayable(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		t.Fatal("sidecar exit test requires Bun: ", err)
	}
	if err := omp.EnsureDeps(t.Context(), omp.ServerScript()); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "sc-usage-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "omp.sock")
	t.Setenv(omp.SocketEnv, socket)
	t.Setenv("CLAUDIA_OMP_SPOOL_DIR", filepath.Join(dir, "spool"))
	t.Setenv("CLAUDIA_OMP_NO_REFRESH", "1")
	path := filepath.Join(dir, controlplane.ClaudiaDBFile)
	d, err := New(Options{
		SocketPath:     filepath.Join(dir, "broker.sock"),
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
	claudia.RecordObservedReceipt(started)
	published := exitUsageFacts(t)
	before := storedUsageRows(t, path)

	if err := syscall.Kill(sidecarPID(t, socket), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for readStoredReceipt(t, path, started.Seat, started.SessionID, started.RequestID).State != claudia.ReceiptCancelled {
		// 🎯T97 exemption: a failsafe on the exit hook settling the receipt. The stored state is the verdict; the clock only turns a hang into a failure.
		if time.Now().After(deadline) {
			t.Fatal("sidecar exit hook never settled the started receipt")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The hook's writes and this publish share the owner's write lock, so
	// once this returns every write the hook made is committed.
	later := claudia.UsageFact{
		Seq: 5, CallID: "call-D", Seat: "seat", SessionID: "generation", Kind: "model", State: "started",
		Provider: "openai", Model: "gpt-5.5", Requests: []claudia.UsageRequestRef{},
		StartedAt: "2026-10-16T01:00:02Z", CounterSource: "adapter_normalized_positive", Coverage: "engine_invocation",
	}
	claudia.RecordObservedUsage(&later)
	after := storedUsageRows(t, path)
	if len(after) != len(published)+1 || !reflect.DeepEqual(after[len(published)], later) {
		t.Fatalf("publisher after sidecar exit: rows = %+v", after)
	}
	assertUsageReplayable(t, published, before, after)
}

// TestBrokerCloseLeavesUsageReplayable drives daemon.Close, the broker stop
// path, which settles started receipts. It must not touch usage facts.
func TestBrokerCloseLeavesUsageReplayable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, controlplane.ClaudiaDBFile)
	sock := filepath.Join("/tmp", fmt.Sprintf("usage-close-%d.sock", os.Getpid()))
	_ = os.Remove(sock)
	t.Cleanup(func() { _ = os.Remove(sock) })
	d, err := New(Options{
		SocketPath:     sock,
		StateDir:       dir,
		UsageFetch:     func(context.Context) ([]claudia.PlanUsage, error) { return nil, nil },
		DisableResume:  true,
		DisableIntel:   true,
		DisableMCPHost: true,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	published := exitUsageFacts(t)
	before := storedUsageRows(t, path)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	assertUsageReplayable(t, published, before, storedUsageRows(t, path))
}
