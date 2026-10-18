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
// broker serve uses. An existing control file is bound, so a seat-pump
// receipt lands in that file and a second open can read it. Closing the
// daemon unbinds the pump: a later receipt does not.
func TestT10749ExistingFilePublishesReceipt(t *testing.T) {
	if os.Getenv("T1074_9_CHILD") == "1" {
		t.Skip("restart child")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, controlplane.ClaudiaDBFile)
	plane, err := controlplane.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := plane.Close(); err != nil {
		t.Fatal(err)
	}

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

func readStoredReceipt(t *testing.T, path, seat, session, request string) claudia.RequestReceipt {
	t.Helper()
	db, err := sqlpipe.OpenDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
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
	db, err := sqlpipe.OpenDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
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
