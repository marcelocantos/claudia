// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package controlplane writes Claudia-owned rows into one file-backed
// sqlpipe database. Jevons subscribes; it does not write these tables.
// This is the usage and receipt fact path, not seat desired-state,
// grants, or per-seat MCP intent pairs.
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
`

// Owns reports whether name is a table Claudia writes on this pair.
func Owns(name string) bool {
	return name == UsageTable || name == ReceiptTable
}

// EnsureSchema creates the Claudia-owned tables if they are absent.
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
		if err := db.Exec(`INSERT INTO claudia_usage_fact (seat, session_id, seq, call_id, kind, fact_json)
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
	return db.Exec(`INSERT INTO claudia_request_receipt (seat, session_id, request_id, state, receipt_json)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(seat, session_id, request_id) DO UPDATE SET
		  state = excluded.state, receipt_json = excluded.receipt_json`,
		receipt.Seat, receipt.SessionID, receipt.RequestID, string(receipt.State), string(body))
}
