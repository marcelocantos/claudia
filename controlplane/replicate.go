// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package controlplane is the shared Jevons–Claudia sqlpipe pair.
// Each table has one writer. Claudia writes usage, receipts, seat status,
// and control outcomes. Jevons writes desired seats, grants, and control
// intents. A cross-write is rejected. Per-seat MCP pairs are not this file.
package controlplane

import (
	"encoding/json"
	"fmt"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

const (
	// UsageTable is the Claudia-owned stream of uniquely identified usage facts.
	UsageTable = "claudia_usage_fact"
	// ReceiptTable is the Claudia-owned receipt for one logical request.
	ReceiptTable = "claudia_request_receipt"
	// StatusTable is the Claudia-owned current seat projection. One row per seat.
	StatusTable = "claudia_seat_status"
	// OutcomeTable is the Claudia-owned result of one Jevons control intent.
	OutcomeTable = "claudia_control_outcome"
	// DesiredSeatTable is the Jevons-owned desired seat. One row per seat.
	DesiredSeatTable = "jevons_desired_seat"
	// GrantTable is the Jevons-owned grant. Credentials are not stored here.
	GrantTable = "jevons_grant"
	// IntentTable is the Jevons-owned control intent. One row per operation.
	IntentTable = "jevons_control_intent"
	// ClaudiaDBFile is the Claudia-hosted file name inside a shared state directory.
	// The library owns the spelling. This name is the same constant.
	ClaudiaDBFile = claudia.ClaudiaDBFile
	// JevonsDBFile is the Jevons-hosted file name beside ClaudiaDBFile.
	JevonsDBFile = "control-replica.db"
)

// Schema is applied on both sides of the pair before the peers handshake.
// sqlpipe diff sync addresses rows by rowid, so each table has an integer
// primary key. The seat identity stays a separate unique key.
const Schema = `
CREATE TABLE IF NOT EXISTS claudia_usage_fact (
  id INTEGER PRIMARY KEY,
  seat TEXT NOT NULL,
  session_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  call_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  fact_json TEXT NOT NULL,
  UNIQUE (seat, session_id, seq)
);
CREATE TABLE IF NOT EXISTS claudia_request_receipt (
  id INTEGER PRIMARY KEY,
  seat TEXT NOT NULL,
  session_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  state TEXT NOT NULL,
  receipt_json TEXT NOT NULL,
  UNIQUE (seat, session_id, request_id)
);
CREATE TABLE IF NOT EXISTS claudia_seat_status (
  id INTEGER PRIMARY KEY,
  seat TEXT NOT NULL UNIQUE,
  session_id TEXT NOT NULL,
  generation INTEGER NOT NULL,
  revision INTEGER NOT NULL,
  runtime_id TEXT NOT NULL,
  state TEXT NOT NULL,
  status_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS claudia_control_outcome (
  id INTEGER PRIMARY KEY,
  operation_id TEXT NOT NULL UNIQUE,
  seat TEXT NOT NULL,
  generation INTEGER NOT NULL,
  outcome TEXT NOT NULL,
  reason TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS jevons_desired_seat (
  id INTEGER PRIMARY KEY,
  seat TEXT NOT NULL UNIQUE,
  generation INTEGER NOT NULL,
  revision INTEGER NOT NULL,
  provider TEXT NOT NULL,
  model TEXT NOT NULL,
  work_dir TEXT NOT NULL,
  config_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS jevons_grant (
  id INTEGER PRIMARY KEY,
  seat TEXT NOT NULL,
  grant_id TEXT NOT NULL,
  revision INTEGER NOT NULL,
  scope TEXT NOT NULL,
  UNIQUE (seat, grant_id)
);
CREATE TABLE IF NOT EXISTS jevons_control_intent (
  id INTEGER PRIMARY KEY,
  operation_id TEXT NOT NULL UNIQUE,
  seat TEXT NOT NULL,
  generation INTEGER NOT NULL,
  kind TEXT NOT NULL,
  payload_json TEXT NOT NULL
);
`

// Owns reports whether name is a table Claudia writes on this pair.
func Owns(name string) bool {
	switch name {
	case UsageTable, ReceiptTable, StatusTable, OutcomeTable:
		return true
	default:
		return false
	}
}

// JevonsOwns reports whether name is a table Jevons writes on this pair.
func JevonsOwns(name string) bool {
	switch name {
	case DesiredSeatTable, GrantTable, IntentTable:
		return true
	default:
		return false
	}
}

// ClaudiaTables is the ownership claim the Claudia peer presents.
func ClaudiaTables() map[string]bool {
	return map[string]bool{
		UsageTable: true, ReceiptTable: true, StatusTable: true, OutcomeTable: true,
	}
}

// EnsureSchema creates the shared-pair tables if they are absent.
func EnsureSchema(db *sqlpipe.Database) error {
	if db == nil {
		return fmt.Errorf("control plane: nil database")
	}
	return db.Exec(Schema)
}

// PublishUsageFacts validates each fact and upserts it by seat, session, and seq.
// A later observation of the same seq replaces the stored JSON. An invalid fact
// writes nothing from that fact onward; earlier facts in the slice stay written.
func PublishUsageFacts(db *sqlpipe.Database, facts []claudia.UsageFact) error {
	if db == nil {
		return fmt.Errorf("control plane: nil database")
	}
	for i := range facts {
		f := &facts[i]
		if err := f.ValidateIdentity(f.Seat, f.SessionID); err != nil {
			return err
		}
		body, err := json.Marshal(f)
		if err != nil {
			return err
		}
		if err := ExecOwned(db, WriterClaudia, UsageTable, `INSERT INTO claudia_usage_fact (seat, session_id, seq, call_id, kind, fact_json)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(seat, session_id, seq) DO UPDATE SET
			  call_id = excluded.call_id, kind = excluded.kind, fact_json = excluded.fact_json`,
			f.Seat, f.SessionID, int64(f.Seq), f.CallID, f.Kind, string(body)); err != nil {
			return err
		}
	}
	return nil
}

// PublishReceipt validates the sidecar receipt and upserts it by logical request.
func PublishReceipt(db *sqlpipe.Database, receipt *claudia.RequestReceipt) error {
	if db == nil || receipt == nil {
		return fmt.Errorf("control plane: nil receipt")
	}
	if err := receipt.ValidateIdentity(receipt.Seat, receipt.SessionID, receipt.RequestID); err != nil {
		return err
	}
	body, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	return ExecOwned(db, WriterClaudia, ReceiptTable, `INSERT INTO claudia_request_receipt (seat, session_id, request_id, state, receipt_json)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(seat, session_id, request_id) DO UPDATE SET
		  state = excluded.state, receipt_json = excluded.receipt_json`,
		receipt.Seat, receipt.SessionID, receipt.RequestID, string(receipt.State), string(body))
}
