// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"encoding/json"
	"path/filepath"
	"reflect"
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

func usageAt(seq uint64, call, state string) claudia.UsageFact {
	f := claudia.UsageFact{
		Seq: seq, CallID: call, Seat: "seat", SessionID: "generation",
		Kind: "model", State: state, Provider: "openai", Model: "gpt-5.5",
		Requests:      []claudia.UsageRequestRef{{RequestID: "logical-U", AttemptID: "attempt-U"}},
		StartedAt:     "2026-10-16T01:00:00Z",
		CounterSource: "adapter_normalized_positive",
		Coverage:      "engine_invocation",
	}
	if state != "started" {
		f.FinishedAt = "2026-10-16T01:00:01Z"
	}
	return f
}

// TestSettleLeavesUsageFactsAsPublished: an exit settles receipts, never
// usage facts. A call left started stays started (incomplete, unknown
// consumption), and a started row whose call finished at a later seq is
// not rewritten either.
func TestSettleLeavesUsageFactsAsPublished(t *testing.T) {
	db := openFileDB(t)
	facts := []claudia.UsageFact{usageAt(1, "call-1", "started"), usageAt(2, "call-2", "started"), usageAt(3, "call-2", "completed")}
	if err := PublishUsageFacts(db, facts); err != nil {
		t.Fatal(err)
	}
	if err := SettleStartedObligations(db, SidecarExitReceiptReason); err != nil {
		t.Fatal(err)
	}
	for _, want := range facts {
		if got := readUsage(t, db, int64(want.Seq)); !reflect.DeepEqual(got, want) {
			t.Fatalf("seq %d after settle = %+v, want %+v", want.Seq, got, want)
		}
	}
}

// TestPublishRefusesUsageRewrite: the mirror's writer enforces the same
// transition contract consumers replay with, so a rewrite like the one
// v0.58.0's exit settle made cannot be stored.
func TestPublishRefusesUsageRewrite(t *testing.T) {
	db := openFileDB(t)
	started := usageAt(1, "call-1", "started")
	if err := PublishUsageFacts(db, []claudia.UsageFact{started}); err != nil {
		t.Fatal(err)
	}
	rewrite := started
	rewrite.State, rewrite.Cause, rewrite.FinishedAt = "cancelled", SidecarExitReceiptReason, "2026-10-16T01:00:01Z"
	if err := PublishUsageFacts(db, []claudia.UsageFact{rewrite}); err == nil {
		t.Fatal("a different fact at a stored seq was published")
	}
	if got := readUsage(t, db, 1); !reflect.DeepEqual(got, started) {
		t.Fatalf("refused rewrite changed the row: %+v", got)
	}
	// The terminal fact is a later seq of the same call, with the same identity.
	done := usageAt(2, "call-1", "completed")
	if err := PublishUsageFacts(db, []claudia.UsageFact{done, done}); err != nil {
		t.Fatal(err)
	}
	// A second terminal for the same call is a conflict, at any seq.
	again := usageAt(3, "call-1", "failed")
	if err := PublishUsageFacts(db, []claudia.UsageFact{again}); err == nil {
		t.Fatal("second terminal fact for one call was published")
	}
	// A terminal fact seen first, then its own older start, is valid replay.
	late := usageAt(4, "call-2", "completed")
	early := usageAt(3, "call-2", "started")
	if err := PublishUsageFacts(db, []claudia.UsageFact{late, early}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, UsageTable); n != 4 {
		t.Fatalf("usage rows = %d, want 4", n)
	}
}

func readUsage(t *testing.T, db *sqlpipe.Database, seq int64) claudia.UsageFact {
	t.Helper()
	qr, err := db.Query(`SELECT fact_json FROM `+UsageTable+` WHERE seq = ?`, seq)
	if err != nil {
		t.Fatal(err)
	}
	if len(qr.Rows) != 1 || len(qr.Rows[0]) != 1 {
		t.Fatalf("usage rows = %+v", qr.Rows)
	}
	body, ok := qr.Rows[0][0].(string)
	if !ok {
		t.Fatalf("fact_json type %T", qr.Rows[0][0])
	}
	var fact claudia.UsageFact
	if err := json.Unmarshal([]byte(body), &fact); err != nil {
		t.Fatal(err)
	}
	return fact
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
