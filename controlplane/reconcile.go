// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

func init() {
	// The daemon calls Registry.ReconcileControlFile and OpenControlPublisher
	// and cannot import this package. Importing this package is what links
	// both calls.
	claudia.SetReconcileControlFile(ReconcileFile)
	claudia.SetPublishControlFile(PublishFile)
}

const (
	statusRunning         = "running"
	statusUngranted       = "ungranted"
	statusReattachRefused = "reattach_refused"
	statusStopped         = "stopped"

	outcomeAccepted  = "accepted"
	outcomeStale     = "stale"
	outcomeUncertain = "uncertain"
	outcomeRejected  = "rejected"
)

// ReconcileFile reconciles reg to the desired rows already stored in this
// broker's own control file, through the connection PublishFile opened. It
// never opens a second connection: the owner plane is the file's one
// writer. Desired rows reach the file from the consumer's link. With none
// there, every running seat is left alone, so a Jevons outage leaves
// Claudia running.
func ReconcileFile(ctx context.Context, path string, reg *claudia.Registry) error {
	ownedMu.Lock()
	p, at := owned, ownedPath
	ownedMu.Unlock()
	if p == nil || at != path {
		return fmt.Errorf("%w: reconcile of %s before this broker opened it (PublishFile)", ErrNotOwner, path)
	}
	return reconcile(ctx, p.withDB, reg)
}

// Reconcile reads desired state, grants, and control intents from db and
// applies them to reg. Querying the tables is the startup read. An unchanged
// generation does not mint a session. A failed reattach does not replace the
// session id. An intent whose outcome row already exists is not applied again.
func Reconcile(ctx context.Context, db *sqlpipe.Database, reg *claudia.Registry) error {
	if db == nil || reg == nil {
		return fmt.Errorf("control plane: nil reconcile")
	}
	return reconcile(ctx, func(fn func(*sqlpipe.Database) error) error { return fn(db) }, reg)
}

// dbRun runs one step against the file under its writer's lock. Reconcile
// takes the lock per step, never across a registry call: a seat launch can
// publish through the same owner plane, and holding the lock across it
// would deadlock the seat pump on its own broker.
type dbRun func(func(*sqlpipe.Database) error) error

func reconcile(ctx context.Context, run dbRun, reg *claudia.Registry) error {
	if reg == nil {
		return fmt.Errorf("control plane: nil reconcile")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var (
		desired  []DesiredSeat
		grants   []Grant
		statuses map[string]SeatStatus
		intents  []ControlIntent
		outcomes map[string]ControlOutcome
	)
	if err := run(func(db *sqlpipe.Database) (err error) {
		if desired, err = DesiredSeats(db); err != nil {
			return err
		}
		if grants, err = Grants(db); err != nil {
			return err
		}
		if statuses, err = SeatStatuses(db); err != nil {
			return err
		}
		if intents, err = ControlIntents(db); err != nil {
			return err
		}
		outcomes, err = ControlOutcomes(db)
		return err
	}); err != nil {
		return err
	}
	granted := map[string]bool{}
	for _, g := range grants {
		granted[g.Seat] = true
	}
	for _, seat := range desired {
		if err := ctx.Err(); err != nil {
			return err
		}
		status, err := reconcileSeat(ctx, run, reg, seat, granted[seat.Seat], statuses[seat.Seat])
		if err != nil {
			return err
		}
		statuses[seat.Seat] = status
	}
	for _, intent := range intents {
		if _, done := outcomes[intent.OperationID]; done {
			continue
		}
		outcome, err := reconcileIntent(run, reg, intent, statuses[intent.Seat])
		if err != nil {
			return err
		}
		outcomes[intent.OperationID] = outcome
	}
	return nil
}

func reconcileSeat(ctx context.Context, run dbRun, reg *claudia.Registry, seat DesiredSeat, granted bool, prev SeatStatus) (SeatStatus, error) {
	if !granted {
		status := SeatStatus{
			Seat: seat.Seat, SessionID: prev.SessionID, Generation: seat.Generation, Revision: seat.Revision,
			RuntimeID: prev.RuntimeID, State: statusUngranted, StatusJSON: statusBody(seat),
		}
		if err := run(func(db *sqlpipe.Database) error { return PublishSeatStatus(db, status) }); err != nil {
			return SeatStatus{}, err
		}
		return status, nil
	}
	if prev.Seat != "" && prev.Generation != seat.Generation {
		if proc := reg.Get(seat.Seat); proc != nil && proc.Alive() {
			reg.Stop(seat.Seat)
		}
		if reg.Def(seat.Seat) != nil {
			if err := reg.Remove(seat.Seat); err != nil {
				return SeatStatus{}, err
			}
		}
		prev = SeatStatus{}
	}
	sessionID, err := alignDef(reg, seat)
	if err != nil {
		return SeatStatus{}, err
	}
	state := statusRunning
	runtimeID := sessionID
	proc := reg.Get(seat.Seat)
	if proc != nil && proc.Alive() {
		runtimeID = proc.SessionID()
	} else {
		before := reg.Def(seat.Seat).SessionID
		agent, launchErr := reg.LaunchContext(ctx, seat.Seat)
		after := reg.Def(seat.Seat)
		if after == nil || after.SessionID != before {
			return SeatStatus{}, fmt.Errorf("control plane: seat %s reminted session %q -> %v", seat.Seat, before, after)
		}
		if launchErr != nil {
			state = statusReattachRefused
			runtimeID = before
		} else if agent != nil {
			runtimeID = agent.SessionID()
		}
	}
	status := SeatStatus{
		Seat: seat.Seat, SessionID: sessionID, Generation: seat.Generation, Revision: seat.Revision,
		RuntimeID: runtimeID, State: state, StatusJSON: statusBody(seat),
	}
	if err := run(func(db *sqlpipe.Database) error { return PublishSeatStatus(db, status) }); err != nil {
		return SeatStatus{}, err
	}
	return status, nil
}

func reconcileIntent(run dbRun, reg *claudia.Registry, intent ControlIntent, status SeatStatus) (ControlOutcome, error) {
	outcome := ControlOutcome{OperationID: intent.OperationID, Seat: intent.Seat, Generation: intent.Generation}
	if status.Generation != intent.Generation || status.State == "" {
		outcome.Outcome = outcomeStale
		outcome.Reason = "generation"
	} else {
		switch intent.Kind {
		case "interrupt":
			outcome.Outcome = outcomeAccepted
			outcome.Reason = "recorded"
		case "stop":
			reg.Stop(intent.Seat)
			outcome.Outcome = outcomeAccepted
			outcome.Reason = "stopped"
			stopped := status
			stopped.State = statusStopped
			if err := run(func(db *sqlpipe.Database) error { return PublishSeatStatus(db, stopped) }); err != nil {
				return ControlOutcome{}, err
			}
		case "external":
			outcome.Outcome = outcomeUncertain
			outcome.Reason = "external effect"
		default:
			outcome.Outcome = outcomeRejected
			outcome.Reason = "unknown kind"
		}
	}
	if err := run(func(db *sqlpipe.Database) error { return PublishControlOutcome(db, outcome) }); err != nil {
		return ControlOutcome{}, err
	}
	return outcome, nil
}

func alignDef(reg *claudia.Registry, seat DesiredSeat) (string, error) {
	provider, err := parseProvider(seat.Provider)
	if err != nil {
		return "", err
	}
	def := reg.Def(seat.Seat)
	if def == nil {
		created, err := reg.EnsureAgent(seat.Seat, seat.WorkDir, seat.Model, false)
		if err != nil {
			return "", err
		}
		def = created
	}
	def.Provider = provider
	def.Model = seat.Model
	def.WorkDir = seat.WorkDir
	def.AutoStart = false
	if err := reg.Register(*def); err != nil {
		return "", err
	}
	return def.SessionID, nil
}

func parseProvider(s string) (claudia.Provider, error) {
	switch claudia.Provider(s) {
	case claudia.ProviderClaude, claudia.ProviderCodex, claudia.ProviderGrok, claudia.ProviderCursor:
		return claudia.Provider(s), nil
	default:
		return "", fmt.Errorf("control plane: unknown provider %q", s)
	}
}

func statusBody(seat DesiredSeat) string {
	body, err := json.Marshal(struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}{Provider: seat.Provider, Model: seat.Model})
	if err != nil {
		return "{}"
	}
	return string(body)
}

// DesiredSeats reads the Jevons-owned desired-seat table.
func DesiredSeats(db *sqlpipe.Database) ([]DesiredSeat, error) {
	qr, err := db.Query("SELECT seat, generation, revision, provider, model, work_dir, config_json FROM " + DesiredSeatTable + " ORDER BY seat")
	if err != nil {
		return nil, err
	}
	out := make([]DesiredSeat, 0, len(qr.Rows))
	for _, row := range qr.Rows {
		seat, err := scanDesired(qr.Columns, row)
		if err != nil {
			return nil, err
		}
		out = append(out, seat)
	}
	return out, nil
}

// Grants reads the Jevons-owned grant table.
func Grants(db *sqlpipe.Database) ([]Grant, error) {
	qr, err := db.Query("SELECT seat, grant_id, revision, scope FROM " + GrantTable + " ORDER BY seat, grant_id")
	if err != nil {
		return nil, err
	}
	out := make([]Grant, 0, len(qr.Rows))
	for _, row := range qr.Rows {
		g, err := scanGrant(qr.Columns, row)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

// SeatStatuses reads the current projection keyed by seat.
func SeatStatuses(db *sqlpipe.Database) (map[string]SeatStatus, error) {
	qr, err := db.Query("SELECT seat, session_id, generation, revision, runtime_id, state, status_json FROM " + StatusTable + " ORDER BY seat")
	if err != nil {
		return nil, err
	}
	out := map[string]SeatStatus{}
	for _, row := range qr.Rows {
		status, err := scanStatus(qr.Columns, row)
		if err != nil {
			return nil, err
		}
		out[status.Seat] = status
	}
	return out, nil
}

// ControlIntents reads Jevons-owned operations.
func ControlIntents(db *sqlpipe.Database) ([]ControlIntent, error) {
	qr, err := db.Query("SELECT operation_id, seat, generation, kind, payload_json FROM " + IntentTable + " ORDER BY operation_id")
	if err != nil {
		return nil, err
	}
	out := make([]ControlIntent, 0, len(qr.Rows))
	for _, row := range qr.Rows {
		intent, err := scanIntent(qr.Columns, row)
		if err != nil {
			return nil, err
		}
		out = append(out, intent)
	}
	return out, nil
}

// ControlOutcomes reads results keyed by operation id.
func ControlOutcomes(db *sqlpipe.Database) (map[string]ControlOutcome, error) {
	qr, err := db.Query("SELECT operation_id, seat, generation, outcome, reason FROM " + OutcomeTable + " ORDER BY operation_id")
	if err != nil {
		return nil, err
	}
	out := map[string]ControlOutcome{}
	for _, row := range qr.Rows {
		outcome, err := scanOutcome(qr.Columns, row)
		if err != nil {
			return nil, err
		}
		out[outcome.OperationID] = outcome
	}
	return out, nil
}

func scanDesired(cols []string, row []sqlpipe.Value) (DesiredSeat, error) {
	seat, err := cellText(cols, row, "seat")
	if err != nil {
		return DesiredSeat{}, err
	}
	generation, err := cellInt(cols, row, "generation")
	if err != nil {
		return DesiredSeat{}, err
	}
	revision, err := cellInt(cols, row, "revision")
	if err != nil {
		return DesiredSeat{}, err
	}
	provider, err := cellText(cols, row, "provider")
	if err != nil {
		return DesiredSeat{}, err
	}
	model, err := cellText(cols, row, "model")
	if err != nil {
		return DesiredSeat{}, err
	}
	workDir, err := cellText(cols, row, "work_dir")
	if err != nil {
		return DesiredSeat{}, err
	}
	config, err := cellText(cols, row, "config_json")
	if err != nil {
		return DesiredSeat{}, err
	}
	return DesiredSeat{Seat: seat, Generation: generation, Revision: revision, Provider: provider, Model: model, WorkDir: workDir, ConfigJSON: config}, nil
}

func scanGrant(cols []string, row []sqlpipe.Value) (Grant, error) {
	seat, err := cellText(cols, row, "seat")
	if err != nil {
		return Grant{}, err
	}
	id, err := cellText(cols, row, "grant_id")
	if err != nil {
		return Grant{}, err
	}
	revision, err := cellInt(cols, row, "revision")
	if err != nil {
		return Grant{}, err
	}
	scope, err := cellText(cols, row, "scope")
	if err != nil {
		return Grant{}, err
	}
	return Grant{Seat: seat, GrantID: id, Revision: revision, Scope: scope}, nil
}

func scanStatus(cols []string, row []sqlpipe.Value) (SeatStatus, error) {
	seat, err := cellText(cols, row, "seat")
	if err != nil {
		return SeatStatus{}, err
	}
	session, err := cellText(cols, row, "session_id")
	if err != nil {
		return SeatStatus{}, err
	}
	generation, err := cellInt(cols, row, "generation")
	if err != nil {
		return SeatStatus{}, err
	}
	revision, err := cellInt(cols, row, "revision")
	if err != nil {
		return SeatStatus{}, err
	}
	runtime, err := cellText(cols, row, "runtime_id")
	if err != nil {
		return SeatStatus{}, err
	}
	state, err := cellText(cols, row, "state")
	if err != nil {
		return SeatStatus{}, err
	}
	body, err := cellText(cols, row, "status_json")
	if err != nil {
		return SeatStatus{}, err
	}
	return SeatStatus{Seat: seat, SessionID: session, Generation: generation, Revision: revision, RuntimeID: runtime, State: state, StatusJSON: body}, nil
}

func scanIntent(cols []string, row []sqlpipe.Value) (ControlIntent, error) {
	id, err := cellText(cols, row, "operation_id")
	if err != nil {
		return ControlIntent{}, err
	}
	seat, err := cellText(cols, row, "seat")
	if err != nil {
		return ControlIntent{}, err
	}
	generation, err := cellInt(cols, row, "generation")
	if err != nil {
		return ControlIntent{}, err
	}
	kind, err := cellText(cols, row, "kind")
	if err != nil {
		return ControlIntent{}, err
	}
	payload, err := cellText(cols, row, "payload_json")
	if err != nil {
		return ControlIntent{}, err
	}
	return ControlIntent{OperationID: id, Seat: seat, Generation: generation, Kind: kind, PayloadJSON: payload}, nil
}

func scanOutcome(cols []string, row []sqlpipe.Value) (ControlOutcome, error) {
	id, err := cellText(cols, row, "operation_id")
	if err != nil {
		return ControlOutcome{}, err
	}
	seat, err := cellText(cols, row, "seat")
	if err != nil {
		return ControlOutcome{}, err
	}
	generation, err := cellInt(cols, row, "generation")
	if err != nil {
		return ControlOutcome{}, err
	}
	outcome, err := cellText(cols, row, "outcome")
	if err != nil {
		return ControlOutcome{}, err
	}
	reason, err := cellText(cols, row, "reason")
	if err != nil {
		return ControlOutcome{}, err
	}
	return ControlOutcome{OperationID: id, Seat: seat, Generation: generation, Outcome: outcome, Reason: reason}, nil
}

func cellText(cols []string, row []sqlpipe.Value, name string) (string, error) {
	v, err := cell(cols, row, name)
	if err != nil {
		return "", err
	}
	return sqlText(v)
}

func cellInt(cols []string, row []sqlpipe.Value, name string) (int64, error) {
	v, err := cell(cols, row, name)
	if err != nil {
		return 0, err
	}
	return sqlInt(v)
}

func colIndex(cols []string, name string) (int, error) {
	for i, c := range cols {
		if c == name {
			return i, nil
		}
	}
	return 0, fmt.Errorf("control plane: column %s missing", name)
}

func cell(cols []string, row []sqlpipe.Value, name string) (any, error) {
	i, err := colIndex(cols, name)
	if err != nil {
		return nil, err
	}
	if i >= len(row) {
		return nil, fmt.Errorf("control plane: column %s short", name)
	}
	return row[i], nil
}

func sqlText(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case []byte:
		return string(t), nil
	default:
		return "", fmt.Errorf("control plane: text has type %T", v)
	}
}

func sqlInt(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	default:
		return 0, fmt.Errorf("control plane: integer has type %T", v)
	}
}
