// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"path/filepath"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

func TestPublishRejectsInvalidUsageFact(t *testing.T) {
	db := openFileDB(t)
	bad := claudia.UsageFact{Seat: "seat", SessionID: "generation"}
	if err := PublishUsageFacts(db, []claudia.UsageFact{bad}); err == nil {
		t.Fatal("invalid usage fact was published")
	}
	if n := countRows(t, db, UsageTable); n != 0 {
		t.Fatalf("invalid usage fact left %d rows", n)
	}
	if err := PublishReceipt(db, nil); err == nil {
		t.Fatal("nil receipt was published")
	}
}

func TestPublishUsageFactAndReceiptRoundTrip(t *testing.T) {
	db := openFileDB(t)
	fact := claudia.UsageFact{
		Seq: 1, CallID: "call-1", Seat: "seat", SessionID: "generation",
		Kind: "model", State: "started", Provider: "openai", Model: "gpt-5.5",
		Requests:      []claudia.UsageRequestRef{},
		StartedAt:     "2026-10-16T01:00:00Z",
		CounterSource: "adapter_normalized_positive",
		Coverage:      "engine_invocation",
	}
	if err := PublishUsageFacts(db, []claudia.UsageFact{fact}); err != nil {
		t.Fatal(err)
	}
	receipt := &claudia.RequestReceipt{
		RequestID: "logical-A", SessionID: "generation", Seat: "seat",
		State: claudia.ReceiptAccepted, AttemptID: "attempt-A", TurnIDs: []string{},
		AcceptedAt: "2026-10-16T01:00:00.000000000Z",
		UpdatedAt:  "2026-10-16T01:00:00.000000000Z",
	}
	if err := PublishReceipt(db, receipt); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, UsageTable); n != 1 {
		t.Fatalf("usage rows = %d", n)
	}
	if n := countRows(t, db, ReceiptTable); n != 1 {
		t.Fatalf("receipt rows = %d", n)
	}
	if err := PublishUsageFacts(db, []claudia.UsageFact{fact}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, UsageTable); n != 1 {
		t.Fatalf("republish changed usage row count to %d", n)
	}
}

func openFileDB(t *testing.T) *sqlpipe.Database {
	t.Helper()
	db, err := sqlpipe.OpenDatabase(filepath.Join(t.TempDir(), "claudia.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func countRows(t *testing.T, db *sqlpipe.Database, table string) int {
	t.Helper()
	if table != UsageTable && table != ReceiptTable {
		t.Fatalf("unexpected table %s", table)
	}
	qr, err := db.Query("SELECT COUNT(*) FROM " + table)
	if err != nil {
		t.Fatal(err)
	}
	if len(qr.Rows) != 1 || len(qr.Rows[0]) != 1 {
		t.Fatalf("%s count shape %+v", table, qr)
	}
	n, ok := qr.Rows[0][0].(int64)
	if !ok {
		t.Fatalf("%s count type %T", table, qr.Rows[0][0])
	}
	return int(n)
}
