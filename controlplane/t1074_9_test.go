// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

func TestT10749RejectsCredentialsAndCrossWrites(t *testing.T) {
	db := openFileDB(t)
	seat := DesiredSeat{
		Seat: "seat-a", Generation: 1, Revision: 1, Provider: "grok", Model: "m",
		WorkDir: t.TempDir(), ConfigJSON: `{}`,
	}
	if err := PublishDesiredSeat(db, seat); err != nil {
		t.Fatal(err)
	}
	seat.ConfigJSON = `{"token":"x"}`
	if err := PublishDesiredSeat(db, seat); err == nil {
		t.Fatal("token config was published")
	}
	seat.Revision = 2
	seat.ConfigJSON = `{"outer":{"Password":"p"}}`
	if err := PublishDesiredSeat(db, seat); err == nil {
		t.Fatal("nested password was published")
	}
	got, err := DesiredSeats(db)
	if err != nil || len(got) != 1 || got[0].Revision != 1 || got[0].ConfigJSON != `{}` {
		t.Fatalf("previous desired row = %+v %v", got, err)
	}
	if err := PublishGrant(db, Grant{Seat: "seat-a", GrantID: "g", Revision: 1, Scope: `{"api_key":"x"}`}); err == nil {
		t.Fatal("api_key grant was published")
	}
	if err := PublishGrant(db, Grant{Seat: "seat-a", GrantID: "g", Revision: 1, Scope: `{"name":"read"}`}); err != nil {
		t.Fatal(err)
	}
	if err := PublishControlIntent(db, ControlIntent{OperationID: "op", Seat: "seat-a", Generation: 1, Kind: "stop", PayloadJSON: `{"credentials":"x"}`}); err == nil {
		t.Fatal("credential intent was published")
	}
	if err := ExecOwned(db, WriterClaudia, DesiredSeatTable, `INSERT INTO jevons_desired_seat (seat, generation, revision, provider, model, work_dir, config_json) VALUES ('x', 1, 1, 'grok', 'm', '/tmp', '{}')`); !errors.Is(err, ErrCrossWrite) {
		t.Fatalf("claudia write of desired seat = %v", err)
	}
	if err := ExecOwned(db, WriterJevons, StatusTable, `INSERT INTO claudia_seat_status (seat, session_id, generation, revision, runtime_id, state, status_json) VALUES ('x', 's', 1, 1, 'r', 'running', '{}')`); !errors.Is(err, ErrCrossWrite) {
		t.Fatalf("jevons write of status = %v", err)
	}
}

func TestT10749OwnershipAndSubscription(t *testing.T) {
	if ApproveClaudia(nil) || ApproveClaudia(map[string]bool{}) || ApproveClaudia(map[string]bool{DesiredSeatTable: true}) {
		t.Fatal("non-claudia claim was approved")
	}
	if !ApproveClaudia(ClaudiaTables()) {
		t.Fatal("claudia tables were rejected")
	}
	dir := t.TempDir()
	clientDB := openNamedDB(t, filepath.Join(dir, "client.db"))
	serverDB := openNamedDB(t, filepath.Join(dir, "server.db"))
	client, err := sqlpipe.NewPeer(clientDB, sqlpipe.PeerConfig{OwnedTables: map[string]bool{DesiredSeatTable: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := sqlpipe.NewPeer(serverDB, sqlpipe.PeerConfig{ApproveOwnership: ApproveClaudia})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	err = sqlpipe.SyncPeerHandshake(client, server)
	if client.State() == sqlpipe.PeerLive || server.State() == sqlpipe.PeerLive {
		t.Fatalf("jevons table claim reached live: err=%v client=%d server=%d", err, client.State(), server.State())
	}

	liveClientDB := openNamedDB(t, filepath.Join(dir, "live-client.db"))
	liveServerDB := openNamedDB(t, filepath.Join(dir, "live-server.db"))
	liveClient, err := sqlpipe.NewPeer(liveClientDB, sqlpipe.PeerConfig{OwnedTables: ClaudiaTables()})
	if err != nil {
		t.Fatal(err)
	}
	defer liveClient.Close()
	liveServer, err := sqlpipe.NewPeer(liveServerDB, sqlpipe.PeerConfig{ApproveOwnership: ApproveClaudia})
	if err != nil {
		t.Fatal(err)
	}
	defer liveServer.Close()
	if err := sqlpipe.SyncPeerHandshake(liveClient, liveServer); err != nil {
		t.Fatal(err)
	}
	if liveClient.State() != sqlpipe.PeerLive || liveServer.State() != sqlpipe.PeerLive {
		t.Fatalf("shared pair not live: client=%d server=%d", liveClient.State(), liveServer.State())
	}
	if _, err := liveClient.Subscribe("SELECT seat FROM " + DesiredSeatTable + " ORDER BY seat"); err != nil {
		t.Fatal(err)
	}
	if err := PublishDesiredSeat(liveServerDB, DesiredSeat{
		Seat: "seat-a", Generation: 1, Revision: 1, Provider: "grok", Model: "m",
		WorkDir: t.TempDir(), ConfigJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	msgs, err := liveServer.Flush()
	if err != nil {
		t.Fatal(err)
	}
	var notes int
	for _, om := range msgs {
		hr, err := liveClient.HandleMessage(om.Msg)
		if err != nil {
			t.Fatal(err)
		}
		for _, sub := range hr.Subscriptions {
			notes++
			if len(sub.Rows) != 1 {
				t.Fatalf("subscription rows = %d", len(sub.Rows))
			}
		}
	}
	if notes != 1 {
		t.Fatalf("subscription updates = %d", notes)
	}
}

func TestT10749ReconcileSeatAndIntent(t *testing.T) {
	t.Run("no grant does not launch or stop", func(t *testing.T) {
		reg, lc := testRegistry(t)
		launchNamed(t, reg, "seat-a")
		db := openFileDB(t)
		publishSeat(t, db, "seat-a", 1, 1, false)
		if err := Reconcile(context.Background(), db, reg); err != nil {
			t.Fatal(err)
		}
		if lc.starts() != 1 || !alive(reg, "seat-a") {
			t.Fatalf("ungranted reconcile starts=%d alive=%v", lc.starts(), alive(reg, "seat-a"))
		}
		status := mustStatus(t, db, "seat-a")
		if status.State != statusUngranted {
			t.Fatalf("status = %+v", status)
		}
	})

	t.Run("same generation does not start again", func(t *testing.T) {
		reg, lc := testRegistry(t)
		db := openFileDB(t)
		publishSeat(t, db, "seat-a", 1, 1, true)
		if err := Reconcile(context.Background(), db, reg); err != nil {
			t.Fatal(err)
		}
		session := reg.Def("seat-a").SessionID
		seat := desired("seat-a", 1, 2)
		seat.Model = "next"
		if err := PublishDesiredSeat(db, seat); err != nil {
			t.Fatal(err)
		}
		if err := Reconcile(context.Background(), db, reg); err != nil {
			t.Fatal(err)
		}
		if lc.starts() != 1 || reg.Def("seat-a").SessionID != session || reg.Def("seat-a").Model != "next" {
			t.Fatalf("starts=%d session=%s model=%s", lc.starts(), reg.Def("seat-a").SessionID, reg.Def("seat-a").Model)
		}
		if mustStatus(t, db, "seat-a").Revision != 2 || mustStatus(t, db, "seat-a").RuntimeID != session {
			t.Fatalf("status = %+v", mustStatus(t, db, "seat-a"))
		}
	})

	t.Run("generation change mints one new session", func(t *testing.T) {
		reg, lc := testRegistry(t)
		db := openFileDB(t)
		publishSeat(t, db, "seat-a", 1, 1, true)
		if err := Reconcile(context.Background(), db, reg); err != nil {
			t.Fatal(err)
		}
		first := reg.Def("seat-a").SessionID
		if err := PublishDesiredSeat(db, desired("seat-a", 2, 2)); err != nil {
			t.Fatal(err)
		}
		if err := Reconcile(context.Background(), db, reg); err != nil {
			t.Fatal(err)
		}
		second := reg.Def("seat-a").SessionID
		if lc.starts() != 2 || second == "" || second == first {
			t.Fatalf("starts=%d first=%s second=%s", lc.starts(), first, second)
		}
		status := mustStatus(t, db, "seat-a")
		if status.Generation != 2 || status.SessionID != second || status.RuntimeID != second || status.State != statusRunning {
			t.Fatalf("status = %+v", status)
		}
	})

	t.Run("stale intent does not stop", func(t *testing.T) {
		reg, lc := testRegistry(t)
		db := openFileDB(t)
		publishSeat(t, db, "seat-a", 1, 1, true)
		if err := PublishControlIntent(db, ControlIntent{OperationID: "op-stale", Seat: "seat-a", Generation: 2, Kind: "stop"}); err != nil {
			t.Fatal(err)
		}
		if err := Reconcile(context.Background(), db, reg); err != nil {
			t.Fatal(err)
		}
		if !alive(reg, "seat-a") || lc.starts() != 1 {
			t.Fatal("stale stop killed the seat")
		}
		outcome := mustOutcome(t, db, "op-stale")
		if outcome.Outcome != outcomeStale || outcome.Reason != "generation" {
			t.Fatalf("outcome = %+v", outcome)
		}
	})

	t.Run("duplicate outcome is not applied", func(t *testing.T) {
		reg, _ := testRegistry(t)
		db := openFileDB(t)
		publishSeat(t, db, "seat-a", 1, 1, true)
		if err := Reconcile(context.Background(), db, reg); err != nil {
			t.Fatal(err)
		}
		if err := PublishControlOutcome(db, ControlOutcome{OperationID: "op-dup", Seat: "seat-a", Generation: 1, Outcome: outcomeAccepted, Reason: "already"}); err != nil {
			t.Fatal(err)
		}
		if err := PublishControlIntent(db, ControlIntent{OperationID: "op-dup", Seat: "seat-a", Generation: 1, Kind: "stop"}); err != nil {
			t.Fatal(err)
		}
		if err := Reconcile(context.Background(), db, reg); err != nil {
			t.Fatal(err)
		}
		if !alive(reg, "seat-a") {
			t.Fatal("duplicate stop was applied")
		}
		if mustOutcome(t, db, "op-dup").Reason != "already" {
			t.Fatalf("outcome replaced: %+v", mustOutcome(t, db, "op-dup"))
		}
	})

	t.Run("interrupt records without stopping", func(t *testing.T) {
		reg, lc := testRegistry(t)
		db := openFileDB(t)
		publishSeat(t, db, "seat-a", 1, 1, true)
		if err := PublishControlIntent(db, ControlIntent{OperationID: "op-int", Seat: "seat-a", Generation: 1, Kind: "interrupt"}); err != nil {
			t.Fatal(err)
		}
		if err := Reconcile(context.Background(), db, reg); err != nil {
			t.Fatal(err)
		}
		if !alive(reg, "seat-a") || lc.starts() != 1 {
			t.Fatal("interrupt stopped or relaunched")
		}
		outcome := mustOutcome(t, db, "op-int")
		if outcome.Outcome != outcomeAccepted || outcome.Reason != "recorded" {
			t.Fatalf("outcome = %+v", outcome)
		}
	})

	t.Run("stop is accepted once", func(t *testing.T) {
		reg, _ := testRegistry(t)
		db := openFileDB(t)
		publishSeat(t, db, "seat-a", 1, 1, true)
		if err := PublishControlIntent(db, ControlIntent{OperationID: "op-stop", Seat: "seat-a", Generation: 1, Kind: "stop"}); err != nil {
			t.Fatal(err)
		}
		if err := Reconcile(context.Background(), db, reg); err != nil {
			t.Fatal(err)
		}
		if alive(reg, "seat-a") {
			t.Fatal("stop left the seat running")
		}
		if mustOutcome(t, db, "op-stop").Reason != "stopped" || mustStatus(t, db, "seat-a").State != statusStopped {
			t.Fatalf("stop status/outcome = %+v %+v", mustStatus(t, db, "seat-a"), mustOutcome(t, db, "op-stop"))
		}
		if err := Reconcile(context.Background(), db, reg); err != nil {
			t.Fatal(err)
		}
		if mustOutcome(t, db, "op-stop").Reason != "stopped" {
			t.Fatal("second pass rewrote the stop")
		}
	})

	t.Run("external effect stays uncertain", func(t *testing.T) {
		reg, lc := testRegistry(t)
		db := openFileDB(t)
		publishSeat(t, db, "seat-a", 1, 1, true)
		if err := PublishControlIntent(db, ControlIntent{OperationID: "op-ext", Seat: "seat-a", Generation: 1, Kind: "external"}); err != nil {
			t.Fatal(err)
		}
		if err := Reconcile(context.Background(), db, reg); err != nil {
			t.Fatal(err)
		}
		if err := Reconcile(context.Background(), db, reg); err != nil {
			t.Fatal(err)
		}
		outcome := mustOutcome(t, db, "op-ext")
		if outcome.Outcome != outcomeUncertain || outcome.Reason != "external effect" || !alive(reg, "seat-a") || lc.starts() != 1 {
			t.Fatalf("outcome=%+v alive=%v starts=%d", outcome, alive(reg, "seat-a"), lc.starts())
		}
	})

	t.Run("unknown kind is rejected", func(t *testing.T) {
		reg, _ := testRegistry(t)
		db := openFileDB(t)
		publishSeat(t, db, "seat-a", 1, 1, true)
		if err := PublishControlIntent(db, ControlIntent{OperationID: "op-bad", Seat: "seat-a", Generation: 1, Kind: "nope"}); err != nil {
			t.Fatal(err)
		}
		if err := Reconcile(context.Background(), db, reg); err != nil {
			t.Fatal(err)
		}
		outcome := mustOutcome(t, db, "op-bad")
		if outcome.Outcome != outcomeRejected || outcome.Reason != "unknown kind" || !alive(reg, "seat-a") {
			t.Fatalf("outcome=%+v alive=%v", outcome, alive(reg, "seat-a"))
		}
	})

	t.Run("empty desired leaves an unrelated seat", func(t *testing.T) {
		reg, lc := testRegistry(t)
		launchNamed(t, reg, "other")
		db := openFileDB(t)
		if err := Reconcile(context.Background(), db, reg); err != nil {
			t.Fatal(err)
		}
		if !alive(reg, "other") || lc.starts() != 1 {
			t.Fatal("empty desired touched the unrelated seat")
		}
	})

	t.Run("a freshly created broker file leaves a running seat", func(t *testing.T) {
		reg, _ := testRegistry(t)
		launchNamed(t, reg, "other")
		asBroker(t)
		dbPath, sock := brokerDirs(t)
		publishOwned(t, dbPath, sock)
		if err := ReconcileFile(context.Background(), dbPath, reg); err != nil {
			t.Fatal(err)
		}
		if !alive(reg, "other") {
			t.Fatal("an empty control file stopped the seat")
		}
	})

	t.Run("reattach refusal does not remint", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "agents.json")
		reg1, err := claudia.NewRegistry(path)
		if err != nil {
			t.Fatal(err)
		}
		const sid = "sess-keep"
		if err := reg1.Register(claudia.AgentDef{
			Name: "seat-a", WorkDir: t.TempDir(), SessionID: sid, Provider: claudia.ProviderGrok, Model: "m",
		}); err != nil {
			t.Fatal(err)
		}
		reg2, err := claudia.NewRegistry(path)
		if err != nil {
			t.Fatal(err)
		}
		lc := &launchCount{refuseResume: true}
		reg2.SetLaunchers(lc.launchers())
		t.Cleanup(func() { reg2.StopAll() })
		db := openFileDB(t)
		publishSeat(t, db, "seat-a", 1, 1, true)
		if err := Reconcile(context.Background(), db, reg2); err != nil {
			t.Fatal(err)
		}
		if reg2.Def("seat-a").SessionID != sid {
			t.Fatalf("session = %s", reg2.Def("seat-a").SessionID)
		}
		if !lc.sawResume() {
			t.Fatal("bounce launch did not require resume")
		}
		status := mustStatus(t, db, "seat-a")
		if status.State != statusReattachRefused || status.SessionID != sid || status.RuntimeID != sid {
			t.Fatalf("status = %+v", status)
		}
	})

	t.Run("resume acceptance keeps the session", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "agents.json")
		reg1, err := claudia.NewRegistry(path)
		if err != nil {
			t.Fatal(err)
		}
		const sid = "sess-resume"
		if err := reg1.Register(claudia.AgentDef{
			Name: "seat-a", WorkDir: t.TempDir(), SessionID: sid, Provider: claudia.ProviderGrok, Model: "m",
		}); err != nil {
			t.Fatal(err)
		}
		reg2, err := claudia.NewRegistry(path)
		if err != nil {
			t.Fatal(err)
		}
		lc := &launchCount{}
		reg2.SetLaunchers(lc.launchers())
		t.Cleanup(func() { reg2.StopAll() })
		db := openFileDB(t)
		publishSeat(t, db, "seat-a", 1, 1, true)
		if err := Reconcile(context.Background(), db, reg2); err != nil {
			t.Fatal(err)
		}
		if reg2.Def("seat-a").SessionID != sid || !lc.sawResume() || !alive(reg2, "seat-a") {
			t.Fatalf("session=%s resume=%v alive=%v", reg2.Def("seat-a").SessionID, lc.sawResume(), alive(reg2, "seat-a"))
		}
		if mustStatus(t, db, "seat-a").State != statusRunning || mustStatus(t, db, "seat-a").RuntimeID != sid {
			t.Fatalf("status = %+v", mustStatus(t, db, "seat-a"))
		}
	})

	t.Run("changed session is an error", func(t *testing.T) {
		reg, lc := testRegistry(t)
		lc.remint = true
		db := openFileDB(t)
		publishSeat(t, db, "seat-a", 1, 1, true)
		err := Reconcile(context.Background(), db, reg)
		if err == nil || !strings.Contains(err.Error(), "reminted") {
			t.Fatalf("err = %v", err)
		}
	})
}

type launchCount struct {
	mu           sync.Mutex
	n            int
	resume       bool
	refuseResume bool
	remint       bool
}

func (l *launchCount) launchers() *claudia.RegistryLaunchers {
	return &claudia.RegistryLaunchers{Start: func(ctx context.Context, cfg claudia.Config) (*claudia.Agent, error) {
		l.mu.Lock()
		l.n++
		if cfg.RequireResume {
			l.resume = true
		}
		refuse := l.refuseResume && cfg.RequireResume
		remint := l.remint
		l.mu.Unlock()
		if refuse {
			return nil, fmt.Errorf("session %s: existing conversation required — refusing to mint a replacement session", cfg.SessionID)
		}
		if remint {
			cfg.SessionID = cfg.SessionID + "-reminted"
		}
		// RequireResume stays set. The stub accepts it for a grok seat and
		// returns that same session id.
		return claudia.StartStub(ctx, cfg, nil)
	}}
}

func (l *launchCount) starts() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

func (l *launchCount) sawResume() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.resume
}

func testRegistry(t *testing.T) (*claudia.Registry, *launchCount) {
	t.Helper()
	reg, err := claudia.NewRegistry(filepath.Join(t.TempDir(), "agents.json"))
	if err != nil {
		t.Fatal(err)
	}
	lc := &launchCount{}
	reg.SetLaunchers(lc.launchers())
	t.Cleanup(func() { reg.StopAll() })
	return reg, lc
}

func launchNamed(t *testing.T, reg *claudia.Registry, name string) {
	t.Helper()
	def, err := reg.EnsureAgent(name, t.TempDir(), "m", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(claudia.AgentDef{
		Name: def.Name, WorkDir: def.WorkDir, SessionID: def.SessionID, Provider: claudia.ProviderGrok, Model: "m",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Launch(name); err != nil {
		t.Fatal(err)
	}
}

func desired(seat string, generation, revision int64) DesiredSeat {
	return DesiredSeat{
		Seat: seat, Generation: generation, Revision: revision, Provider: "grok", Model: "m",
		WorkDir: "/tmp/jevons-seat", ConfigJSON: `{}`,
	}
}

func publishSeat(t *testing.T, db *sqlpipe.Database, seat string, generation, revision int64, grant bool) {
	t.Helper()
	row := desired(seat, generation, revision)
	row.WorkDir = t.TempDir()
	if err := PublishDesiredSeat(db, row); err != nil {
		t.Fatal(err)
	}
	if grant {
		if err := PublishGrant(db, Grant{Seat: seat, GrantID: "g", Revision: revision, Scope: `{"name":"read"}`}); err != nil {
			t.Fatal(err)
		}
	}
}

func mustStatus(t *testing.T, db *sqlpipe.Database, seat string) SeatStatus {
	t.Helper()
	statuses, err := SeatStatuses(db)
	if err != nil {
		t.Fatal(err)
	}
	status, ok := statuses[seat]
	if !ok {
		t.Fatalf("no status for %s", seat)
	}
	return status
}

func mustOutcome(t *testing.T, db *sqlpipe.Database, id string) ControlOutcome {
	t.Helper()
	outcomes, err := ControlOutcomes(db)
	if err != nil {
		t.Fatal(err)
	}
	outcome, ok := outcomes[id]
	if !ok {
		t.Fatalf("no outcome for %s", id)
	}
	return outcome
}

func alive(reg *claudia.Registry, name string) bool {
	proc := reg.Get(name)
	return proc != nil && proc.Alive()
}

func openNamedDB(t *testing.T, path string) *sqlpipe.Database {
	t.Helper()
	db, err := sqlpipe.OpenDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}
