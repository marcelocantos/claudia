// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package controlplane is the Jevons–Claudia sqlpipe pair: two databases,
// one per process, each written only by its own process. The claudia broker
// owns ClaudiaDBFile and writes usage, receipts, seat status and control
// outcomes there. A consumer (Jevons) owns its own file (JevonsDBFile in its
// own state directory) and writes desired seats, grants and control intents
// there. Changesets cross between the two processes over the broker's
// control socket (claudia.ControlSocketPath, Replica.Link); neither process opens the other's file.
// A cross-write is rejected. Per-seat MCP pairs are not this file.
package controlplane

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/omp"
	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

// ShutdownReceiptReason is why a broker stop marks a receipt the sidecar
// left started. The publisher closer runs it as this process exits.
const ShutdownReceiptReason = "broker shutdown"

// SidecarExitReceiptReason is why the broker marks a receipt started when
// the serving sidecar process exits and this broker stays up. That child
// will not write a later terminal fact. Broker shutdown remains a separate
// reason for whatever is still started when the publisher closes.
const SidecarExitReceiptReason = "sidecar exited"

const (
	// UsageTable is the Claudia-owned stream of uniquely identified usage facts.
	UsageTable            = "claudia_usage_fact"
	PlanUsageTable        = "claudia_plan_usage"
	PlanUsageHistoryTable = "claudia_plan_usage_history"
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
	// ClaudiaDBFile is the broker's own file name inside the broker's state
	// directory. The library owns the spelling. This name is the same constant.
	ClaudiaDBFile = claudia.ClaudiaDBFile
	// JevonsDBFile is the consumer's own file name, in the consumer's own
	// state directory.
	JevonsDBFile = "control-replica.db"
)

// Schema is applied on both sides of the pair before the peers handshake.
// sqlpipe diff sync addresses rows by rowid, so each table has an integer
// primary key. The seat identity stays a separate unique key.
const Schema = `
CREATE TABLE IF NOT EXISTS claudia_plan_usage (
 id INTEGER PRIMARY KEY, provider TEXT NOT NULL UNIQUE,
 status TEXT NOT NULL, reason TEXT NOT NULL, attempted_at TEXT NOT NULL,
 success_at TEXT NOT NULL, next_attempt_at TEXT NOT NULL,
 refusals INTEGER NOT NULL DEFAULT 0, reading_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS claudia_plan_usage_history (
 id INTEGER PRIMARY KEY, provider TEXT NOT NULL, observed_at TEXT NOT NULL,
 reading_json TEXT NOT NULL, UNIQUE(provider, observed_at)
);
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
	case UsageTable, ReceiptTable, StatusTable, OutcomeTable, PlanUsageTable, PlanUsageHistoryTable:
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
		UsageTable: true, ReceiptTable: true, StatusTable: true, OutcomeTable: true, PlanUsageTable: true, PlanUsageHistoryTable: true,
	}
}

// EnsureSchema creates the shared-pair tables if they are absent.
func EnsureSchema(db *sqlpipe.Database) error {
	if db == nil {
		return fmt.Errorf("control plane: nil database")
	}
	return db.Exec(Schema)
}

// PublishUsageFacts validates each fact and inserts it by seat, session, and
// seq. The control file mirrors the seat's conversation journal, which owns
// the seq: a seq is written once. Republishing the same fact is a no-op; a
// different fact at a stored seq, or a fact that breaks the per-call
// transition contract against a stored fact of the same call, is refused
// with omp.ValidateUsageTransition, the rule every consumer replays with.
// An invalid fact writes nothing from that fact onward; earlier facts in the
// slice stay written.
func PublishUsageFacts(db *sqlpipe.Database, facts []claudia.UsageFact) error {
	if db == nil {
		return fmt.Errorf("control plane: nil database")
	}
	for i := range facts {
		f := &facts[i]
		if err := f.ValidateIdentity(f.Seat, f.SessionID); err != nil {
			return err
		}
		qr, err := db.Query(`SELECT fact_json FROM `+UsageTable+` WHERE seat = ? AND session_id = ? AND (seq = ? OR call_id = ?)`,
			f.Seat, f.SessionID, int64(f.Seq), f.CallID)
		if err != nil {
			return err
		}
		duplicate := false
		for _, row := range qr.Rows {
			if len(row) != 1 {
				return fmt.Errorf("control plane: usage fact row shape")
			}
			body, ok := row[0].(string)
			if !ok {
				return fmt.Errorf("control plane: fact_json type %T", row[0])
			}
			var stored claudia.UsageFact
			if err := json.Unmarshal([]byte(body), &stored); err != nil {
				return fmt.Errorf("control plane: stored usage fact json: %w", err)
			}
			// The transition contract takes the older fact first.
			previous, incoming := &stored, f
			if f.Seq < stored.Seq {
				previous, incoming = f, &stored
			}
			if _, err := omp.ValidateUsageTransition(previous, incoming); err != nil {
				return fmt.Errorf("control plane: usage fact seq %d call %s: %w", f.Seq, f.CallID, err)
			}
			if stored.Seq == f.Seq {
				duplicate = true
			}
		}
		if duplicate {
			continue
		}
		body, err := json.Marshal(f)
		if err != nil {
			return err
		}
		if err := ExecOwned(db, WriterClaudia, UsageTable, `INSERT INTO claudia_usage_fact (seat, session_id, seq, call_id, kind, fact_json)
			VALUES (?, ?, ?, ?, ?, ?)`,
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

// SettleStartedObligations cancels receipts left started. Accepted
// receipts stay accepted. Completed, failed, already-cancelled, and unknown
// receipts stay. The publisher closer and the serving-sidecar exit hook are
// the callers.
//
// Usage facts are not settled here, or anywhere outside the seat's journal.
// A usage fact's seq belongs to that journal and is written once; an exit is
// not an observation of any provider call. A call whose terminal fact was
// never appended stays started, which the usage contract defines as
// incomplete with unknown consumption (docs/usage-accounting.md).
func SettleStartedObligations(db *sqlpipe.Database, reason string) error {
	return CancelStartedReceipts(db, reason)
}

// CancelStartedReceipts marks every started receipt cancelled. Accepted,
// completed, failed, and already-cancelled rows stay as they are. The
// broker calls this from the publisher closer, and when the serving
// sidecar exits while the broker stays up.
func CancelStartedReceipts(db *sqlpipe.Database, reason string) error {
	if db == nil {
		return fmt.Errorf("control plane: nil database")
	}
	if reason == "" {
		reason = ShutdownReceiptReason
	}
	qr, err := db.Query(`SELECT receipt_json FROM `+ReceiptTable+` WHERE state = ?`, string(claudia.ReceiptStarted))
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, row := range qr.Rows {
		if len(row) != 1 {
			return fmt.Errorf("control plane: started receipt row shape")
		}
		body, ok := row[0].(string)
		if !ok {
			return fmt.Errorf("control plane: receipt_json type %T", row[0])
		}
		var receipt claudia.RequestReceipt
		if err := json.Unmarshal([]byte(body), &receipt); err != nil {
			return fmt.Errorf("control plane: started receipt json: %w", err)
		}
		if receipt.State != claudia.ReceiptStarted {
			continue
		}
		receipt.State = claudia.ReceiptCancelled
		receipt.Cause = claudia.ReceiptCauseCancellation
		receipt.Reason = reason
		if prev, perr := time.Parse(time.RFC3339Nano, receipt.UpdatedAt); perr == nil && !now.After(prev) {
			receipt.UpdatedAt = prev.Add(time.Nanosecond).Format(time.RFC3339Nano)
		} else {
			receipt.UpdatedAt = now.Format(time.RFC3339Nano)
		}
		if err := PublishReceipt(db, &receipt); err != nil {
			return err
		}
	}
	return nil
}
