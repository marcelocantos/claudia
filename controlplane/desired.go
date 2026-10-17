// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

const (
	// WriterClaudia is the only writer of Claudia-owned tables.
	WriterClaudia = "claudia"
	// WriterJevons is the only writer of Jevons-owned tables.
	WriterJevons = "jevons"
)

// ErrCrossWrite means a writer touched a table it does not own.
var ErrCrossWrite = errors.New("control plane: cross-write rejected")

// DesiredSeat is one Jevons-owned desired runtime. A later revision of the
// same seat replaces this row. Generation is the runtime identity fence.
type DesiredSeat struct {
	Seat       string
	Generation int64
	Revision   int64
	Provider   string
	Model      string
	WorkDir    string
	ConfigJSON string
}

// Grant is one Jevons-owned authorization. Scope is a name, not a credential.
type Grant struct {
	Seat     string
	GrantID  string
	Revision int64
	Scope    string
}

// ControlIntent is one Jevons-owned operation. The same operation id updates
// the row in place and must not execute twice.
type ControlIntent struct {
	OperationID string
	Seat        string
	Generation  int64
	Kind        string
	PayloadJSON string
}

// SeatStatus is the Claudia-owned current projection for one seat.
type SeatStatus struct {
	Seat       string
	SessionID  string
	Generation int64
	Revision   int64
	RuntimeID  string
	State      string
	StatusJSON string
}

// ControlOutcome is the Claudia-owned result of one operation id.
type ControlOutcome struct {
	OperationID string
	Seat        string
	Generation  int64
	Outcome     string
	Reason      string
}

// ExecOwned runs query against table for writer. The other writer is rejected
// before sqlite sees the statement.
func ExecOwned(db *sqlpipe.Database, writer, table, query string, args ...any) error {
	if db == nil {
		return fmt.Errorf("control plane: nil database")
	}
	switch writer {
	case WriterClaudia:
		if !Owns(table) {
			return fmt.Errorf("%w: %s cannot write %s", ErrCrossWrite, writer, table)
		}
	case WriterJevons:
		if !JevonsOwns(table) {
			return fmt.Errorf("%w: %s cannot write %s", ErrCrossWrite, writer, table)
		}
	default:
		return fmt.Errorf("%w: unknown writer %q", ErrCrossWrite, writer)
	}
	return db.Exec(query, args...)
}

// PublishDesiredSeat upserts one desired seat. A credential-shaped config is
// rejected and leaves the previous row in place.
func PublishDesiredSeat(db *sqlpipe.Database, seat DesiredSeat) error {
	if err := seat.validate(); err != nil {
		return err
	}
	return ExecOwned(db, WriterJevons, DesiredSeatTable, `INSERT INTO jevons_desired_seat
		(seat, generation, revision, provider, model, work_dir, config_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(seat) DO UPDATE SET
		  generation = excluded.generation,
		  revision = excluded.revision,
		  provider = excluded.provider,
		  model = excluded.model,
		  work_dir = excluded.work_dir,
		  config_json = excluded.config_json`,
		seat.Seat, seat.Generation, seat.Revision, seat.Provider, seat.Model, seat.WorkDir, seat.ConfigJSON)
}

// PublishGrant upserts one grant. Scope must not carry a credential.
func PublishGrant(db *sqlpipe.Database, grant Grant) error {
	if strings.TrimSpace(grant.Seat) == "" || strings.TrimSpace(grant.GrantID) == "" || grant.Revision <= 0 {
		return fmt.Errorf("control plane: invalid grant")
	}
	if err := rejectSecrets(grant.Scope); err != nil {
		return err
	}
	return ExecOwned(db, WriterJevons, GrantTable, `INSERT INTO jevons_grant
		(seat, grant_id, revision, scope) VALUES (?, ?, ?, ?)
		ON CONFLICT(seat, grant_id) DO UPDATE SET
		  revision = excluded.revision, scope = excluded.scope`,
		grant.Seat, grant.GrantID, grant.Revision, grant.Scope)
}

// PublishControlIntent upserts one operation. Repeating the id does not add a row.
func PublishControlIntent(db *sqlpipe.Database, intent ControlIntent) error {
	if err := (&intent).validate(); err != nil {
		return err
	}
	return ExecOwned(db, WriterJevons, IntentTable, `INSERT INTO jevons_control_intent
		(operation_id, seat, generation, kind, payload_json) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(operation_id) DO UPDATE SET
		  seat = excluded.seat, generation = excluded.generation,
		  kind = excluded.kind, payload_json = excluded.payload_json`,
		intent.OperationID, intent.Seat, intent.Generation, intent.Kind, intent.PayloadJSON)
}

// PublishSeatStatus upserts the current projection. History stays in the usage stream.
func PublishSeatStatus(db *sqlpipe.Database, status SeatStatus) error {
	if strings.TrimSpace(status.Seat) == "" || strings.TrimSpace(status.State) == "" || status.Generation < 0 || status.Revision < 0 {
		return fmt.Errorf("control plane: invalid seat status")
	}
	if status.StatusJSON == "" {
		status.StatusJSON = "{}"
	}
	if err := rejectSecrets(status.StatusJSON); err != nil {
		return err
	}
	return ExecOwned(db, WriterClaudia, StatusTable, `INSERT INTO claudia_seat_status
		(seat, session_id, generation, revision, runtime_id, state, status_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(seat) DO UPDATE SET
		  session_id = excluded.session_id, generation = excluded.generation,
		  revision = excluded.revision, runtime_id = excluded.runtime_id,
		  state = excluded.state, status_json = excluded.status_json`,
		status.Seat, status.SessionID, status.Generation, status.Revision, status.RuntimeID, status.State, status.StatusJSON)
}

// PublishControlOutcome upserts one operation result. A second publish replaces
// the JSON in place and does not add a row.
func PublishControlOutcome(db *sqlpipe.Database, outcome ControlOutcome) error {
	if strings.TrimSpace(outcome.OperationID) == "" || strings.TrimSpace(outcome.Seat) == "" || strings.TrimSpace(outcome.Outcome) == "" {
		return fmt.Errorf("control plane: invalid control outcome")
	}
	return ExecOwned(db, WriterClaudia, OutcomeTable, `INSERT INTO claudia_control_outcome
		(operation_id, seat, generation, outcome, reason) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(operation_id) DO UPDATE SET
		  seat = excluded.seat, generation = excluded.generation,
		  outcome = excluded.outcome, reason = excluded.reason`,
		outcome.OperationID, outcome.Seat, outcome.Generation, outcome.Outcome, outcome.Reason)
}

func (s DesiredSeat) validate() error {
	if strings.TrimSpace(s.Seat) == "" || s.Generation <= 0 || s.Revision <= 0 || strings.TrimSpace(s.Provider) == "" || strings.TrimSpace(s.WorkDir) == "" {
		return fmt.Errorf("control plane: invalid desired seat")
	}
	if s.ConfigJSON == "" {
		return fmt.Errorf("control plane: desired seat config is required")
	}
	return rejectSecrets(s.ConfigJSON)
}

func (in *ControlIntent) validate() error {
	if in == nil || strings.TrimSpace(in.OperationID) == "" || strings.TrimSpace(in.Seat) == "" || in.Generation <= 0 || strings.TrimSpace(in.Kind) == "" {
		return fmt.Errorf("control plane: invalid control intent")
	}
	if in.PayloadJSON == "" {
		in.PayloadJSON = "{}"
	}
	return rejectSecrets(in.PayloadJSON)
}

func rejectSecrets(body string) error {
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		if strings.TrimSpace(body) == "" {
			return nil
		}
		return fmt.Errorf("control plane: payload is not json: %w", err)
	}
	if secretKey(v) {
		return fmt.Errorf("control plane: credential payload rejected")
	}
	return nil
}

func secretKey(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			switch strings.ToLower(k) {
			case "token", "access_token", "refresh_token", "api_key", "apikey", "secret", "password", "credential", "credentials", "authorization":
				return true
			}
			if secretKey(child) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if secretKey(child) {
				return true
			}
		}
	}
	return false
}
