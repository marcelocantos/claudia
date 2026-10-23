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
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/controlplane"
	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

// TestT10749ExistingFilePublishesReceipt drives daemon.New, the constructor
// broker serve uses. The broker opens its own control file and binds it, so
// a seat-pump receipt lands in that file and a reader can see it. Closing
// the daemon unbinds the pump: a later receipt does not.
func TestT10749ExistingFilePublishesReceipt(t *testing.T) {
	if os.Getenv("T1074_9_CHILD") == "1" {
		t.Skip("restart child")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, controlplane.ClaudiaDBFile)

	sock := filepath.Join("/tmp", fmt.Sprintf("t10749-bind-%d.sock", os.Getpid()))
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

	accepted := &claudia.RequestReceipt{
		RequestID: "logical-A", SessionID: "generation", Seat: "seat",
		State: claudia.ReceiptAccepted, AttemptID: "attempt-A", TurnIDs: []string{},
		AcceptedAt: "2026-10-16T01:00:00.000000000Z",
		UpdatedAt:  "2026-10-16T01:00:00.000000000Z",
	}
	claudia.RecordObservedReceipt(accepted)
	got := readStoredReceipt(t, path, accepted.Seat, accepted.SessionID, accepted.RequestID)
	if got.RequestID != accepted.RequestID || got.State != claudia.ReceiptAccepted || got.AttemptID != accepted.AttemptID {
		t.Fatalf("stored receipt = %+v", got)
	}

	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	claudia.RecordObservedReceipt(&claudia.RequestReceipt{
		RequestID: "logical-B", SessionID: "generation", Seat: "seat",
		State: claudia.ReceiptAccepted, AttemptID: "attempt-B", TurnIDs: []string{},
		AcceptedAt: "2026-10-16T01:00:01.000000000Z",
		UpdatedAt:  "2026-10-16T01:00:01.000000000Z",
	})
	if n := countReceipts(t, path, "logical-B"); n != 0 {
		t.Fatalf("receipt after close = %d", n)
	}
	if n := countReceipts(t, path, accepted.RequestID); n != 1 {
		t.Fatalf("accepted receipt count = %d", n)
	}
}

// TestBrokerCloseCancelsStartedReceipts drives daemon.Close, the broker
// stop path. A started receipt becomes cancelled. An accepted receipt and
// a completed receipt stay as they were.
func TestBrokerCloseCancelsStartedReceipts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, controlplane.ClaudiaDBFile)
	sock := filepath.Join("/tmp", fmt.Sprintf("t1074-cancel-started-%d.sock", os.Getpid()))
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
	stamp := "2026-10-16T01:00:00.000000000Z"
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
	completed := &claudia.RequestReceipt{
		RequestID: "logical-C", SessionID: "generation", Seat: "seat",
		State: claudia.ReceiptCompleted, AttemptID: "attempt-C", TurnIDs: []string{"turn-C"},
		AcceptedAt: stamp, UpdatedAt: stamp,
	}
	claudia.RecordObservedReceipt(started)
	claudia.RecordObservedReceipt(accepted)
	claudia.RecordObservedReceipt(completed)
	if got := readStoredReceipt(t, path, started.Seat, started.SessionID, started.RequestID); got.State != claudia.ReceiptStarted {
		t.Fatalf("before close = %+v", got)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	got := readStoredReceipt(t, path, started.Seat, started.SessionID, started.RequestID)
	if got.State != claudia.ReceiptCancelled || got.Cause != claudia.ReceiptCauseCancellation || got.Reason != controlplane.ShutdownReceiptReason {
		t.Fatalf("started receipt after close = %+v", got)
	}
	if got.AttemptID != started.AttemptID || len(got.TurnIDs) != 1 || got.TurnIDs[0] != "turn-S" {
		t.Fatalf("identity after close = %+v", got)
	}
	if got.UpdatedAt == stamp {
		t.Fatalf("updated_at stayed %s", got.UpdatedAt)
	}
	if acc := readStoredReceipt(t, path, accepted.Seat, accepted.SessionID, accepted.RequestID); acc.State != claudia.ReceiptAccepted || acc.Cause != "" {
		t.Fatalf("accepted receipt after close = %+v", acc)
	}
	if comp := readStoredReceipt(t, path, completed.Seat, completed.SessionID, completed.RequestID); comp.State != claudia.ReceiptCompleted || comp.Cause != "" {
		t.Fatalf("completed receipt after close = %+v", comp)
	}
}

// controlReaderBusyTimeoutMS is how long a test's own reader of a live
// owner's control file waits for the owner's write lock. Without it the
// reader's connection has SQLite's default busy timeout of zero, so a read
// that lands while the owner is committing (the sidecar-exit hook settling
// receipts, a seat-pump publish) fails at once: "database is locked" from
// prepare, or, from a parameterised sqlpipe Query whose step hits
// SQLITE_BUSY, an empty result with no error. Either fails a correct build on
// a loaded host, where the owner's commit is slow. The owner holds the lock
// only for its commit, so this bounds a wait that always ends: a slow host
// can only make it wait longer, never change what it reads.
const controlReaderBusyTimeoutMS = 30_000

// openControlReader opens a second connection to a control file that a
// live daemon may be writing, waiting out the owner's commits.
func openControlReader(t *testing.T, path string) *sqlpipe.Database {
	t.Helper()
	db, err := sqlpipe.OpenDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(fmt.Sprintf("PRAGMA busy_timeout = %d", controlReaderBusyTimeoutMS)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	return db
}

func readStoredReceipt(t *testing.T, path, seat, session, request string) claudia.RequestReceipt {
	t.Helper()
	db := openControlReader(t, path)
	defer db.Close()
	qr, err := db.Query(`SELECT receipt_json FROM claudia_request_receipt WHERE seat = ? AND session_id = ? AND request_id = ?`,
		seat, session, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(qr.Rows) != 1 || len(qr.Rows[0]) != 1 {
		t.Fatalf("receipt rows = %+v", qr.Rows)
	}
	body, ok := qr.Rows[0][0].(string)
	if !ok {
		t.Fatalf("receipt_json type %T", qr.Rows[0][0])
	}
	var got claudia.RequestReceipt
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func countReceipts(t *testing.T, path, request string) int {
	t.Helper()
	db := openControlReader(t, path)
	defer db.Close()
	qr, err := db.Query(`SELECT COUNT(*) FROM claudia_request_receipt WHERE request_id = ?`, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(qr.Rows) != 1 || len(qr.Rows[0]) != 1 {
		t.Fatalf("count shape %+v", qr.Rows)
	}
	n, ok := qr.Rows[0][0].(int64)
	if !ok {
		t.Fatalf("count type %T", qr.Rows[0][0])
	}
	return int(n)
}
