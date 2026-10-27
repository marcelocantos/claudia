// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

func TestPlanUsageDurableRetryLastSuccessAndAtomicHistory(t *testing.T) {
	asBroker(t)
	path, socket := brokerDirs(t)
	closeOwner := publishOwned(t, path, socket)
	clock := claudia.NewManualClock(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC))
	calls := 0
	status := http.StatusOK
	fetch := func(_ context.Context, provider claudia.Provider) (claudia.PlanUsage, error) {
		if provider != claudia.ProviderClaude {
			return claudia.PlanUsage{Status: claudia.PlanUsageUnavailable, Reason: "unsupported fixture"}, nil
		}
		calls++
		if status != http.StatusOK {
			return claudia.PlanUsage{Status: claudia.PlanUsageUnavailable, HTTPStatus: status, Reason: "vendor refused"}, nil
		}
		remaining, used := 75.0, 25.0
		reset := clock.Now().Add(3 * time.Hour)
		return claudia.PlanUsage{Status: claudia.PlanUsageAvailable, HTTPStatus: status, RawBody: "must never replicate", Windows: []claudia.PlanWindow{{Name: claudia.PlanWindowSession, RemainingPercent: &remaining, UsedPercent: &used, ResetsAt: &reset, LimitWindow: 5 * time.Hour}}}, nil
	}
	poll := func() {
		t.Helper()
		if _, err := claudia.PollBrokerPlanUsage(t.Context(), clock, fetch); err != nil {
			t.Fatal(err)
		}
	}
	read := func() claudia.PlanUsageObservation {
		t.Helper()
		rows, err := owned.ReadPlanUsage()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.Provider == claudia.ProviderClaude {
				return r
			}
		}
		t.Fatal("missing Claude")
		return claudia.PlanUsageObservation{}
	}
	poll()
	first := read()
	if first.Reading.RawBody != "" || first.Reading.Windows[0].Band == "" || first.Reading.Windows[0].Pressure == nil {
		t.Fatalf("normalization: %+v", first)
	}
	poll()
	if calls != 1 {
		t.Fatal("unpaced repeat", calls)
	}
	if err := closeOwner(); err != nil {
		t.Fatal(err)
	}
	closeOwner = publishOwned(t, path, socket)
	poll()
	if calls != 1 {
		t.Fatal("restart discarded pacing", calls)
	}
	clock.Advance(claudia.PlanThrottleMinInterval)
	status = http.StatusTooManyRequests
	poll()
	failed := read()
	if failed.Status != claudia.PlanUsageUnavailable || failed.Refusals != 1 || !failed.NextAttemptAt.Equal(clock.Now().Add(claudia.PlanThrottleFirstPenalty)) || !failed.SuccessAt.Equal(first.SuccessAt) || !failed.Reading.FetchedAt.Equal(first.SuccessAt) {
		t.Fatalf("failure erased success/backoff: %+v", failed)
	}
	for _, u := range claudia.ReadBrokerPlanUsage().Backends {
		if u.Provider == claudia.ProviderClaude && (u.Status == claudia.PlanUsageAvailable || len(u.Windows) > 0) {
			t.Fatal("failed provider falsely healthy", u)
		}
	}
	if err := closeOwner(); err != nil {
		t.Fatal(err)
	}
	publishOwned(t, path, socket)
	clock.Advance(time.Minute)
	poll()
	if calls != 2 {
		t.Fatal("restart bypassed429", calls)
	}
	clock.Advance(claudia.PlanThrottleFirstPenalty)
	poll()
	if read().Refusals != 2 {
		t.Fatal("refusal ladder lost across restart")
	}
	clock.Advance(2 * claudia.PlanThrottleFirstPenalty)
	status = http.StatusOK
	poll()
	recovered := read()
	if recovered.Status != claudia.PlanUsageAvailable || recovered.Refusals != 0 || !recovered.SuccessAt.Equal(clock.Now()) {
		t.Fatalf("no recovery: %+v", recovered)
	}
	ownerRead(t, func(db *sqlpipe.Database) error {
		qr, err := db.Query(`SELECT reading_json FROM claudia_plan_usage_history WHERE provider = 'claude' ORDER BY observed_at`)
		if err != nil {
			return err
		}
		if len(qr.Rows) != 2 {
			t.Fatalf("failures polluted history: %v", qr.Rows)
		}
		for _, row := range qr.Rows {
			if strings.Contains(row[0].(string), "raw_body") {
				t.Fatal("raw body persisted")
			}
		}
		return db.Exec(`CREATE TRIGGER refuse_history BEFORE INSERT ON claudia_plan_usage_history BEGIN SELECT RAISE(ABORT, 'history write failed'); END`)
	})
	clock.Advance(claudia.PlanThrottleMinInterval)
	if _, err := claudia.PollBrokerPlanUsage(t.Context(), clock, fetch); err == nil {
		t.Fatal("write failure hidden")
	}
	after := read()
	if !after.SuccessAt.Equal(recovered.SuccessAt) || after.Status != recovered.Status {
		t.Fatalf("partial completion escaped rollback: %+v", after)
	}
}

func TestPlanUsageSubscriptionInitialLiveReconnectLocalAndClose(t *testing.T) {
	asBroker(t)
	path, socket := brokerDirs(t)
	publishOwned(t, path, socket)
	clock := claudia.NewManualClock(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC))
	failure := false
	fetch := func(_ context.Context, p claudia.Provider) (claudia.PlanUsage, error) {
		if failure {
			return claudia.PlanUsage{}, errors.New("temporary transport failure")
		}
		left := 42.0
		return claudia.PlanUsage{Status: claudia.PlanUsageAvailable, Windows: []claudia.PlanWindow{{Name: claudia.PlanWindowWeekly, RemainingPercent: &left}}}, nil
	}
	poll := func() {
		t.Helper()
		if _, err := claudia.PollBrokerPlanUsage(t.Context(), clock, fetch); err != nil {
			t.Fatal(err)
		}
	}
	poll() // All data predates handshake: the first sqlpipe result must see it.
	replica, err := OpenReplica(filepath.Join(t.TempDir(), JevonsDBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if err := replica.Write(func(db *sqlpipe.Database) error {
		return db.Exec(`CREATE TABLE jv_plan_overrides (id INTEGER PRIMARY KEY, label TEXT NOT NULL); INSERT INTO jv_plan_overrides VALUES (1, 'first')`)
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	updates, err := replica.SubscribeQuery(ctx, `SELECT {status, success_at, label: (SELECT label FROM jv_plan_overrides WHERE id = 1)} FROM claudia_plan_usage WHERE provider = 'claude'`)
	if err != nil {
		t.Fatal(err)
	}
	first := <-updates
	if len(first.Rows) != 0 {
		t.Fatal("cache should start empty")
	}
	var linkCancel context.CancelFunc
	var linkDone chan error
	start := func() {
		var ctx context.Context
		ctx, linkCancel = context.WithCancel(t.Context())
		linkDone = make(chan error, 1)
		go func() { linkDone <- replica.Link(ctx, socket) }()
	}
	stop := func() {
		linkCancel()
		if err := <-linkDone; err != nil {
			t.Fatal(err)
		}
	}
	next := func(wantStatus, wantLabel string) {
		t.Helper()
		for {
			select {
			case result, ok := <-updates:
				if !ok {
					t.Fatal("subscription closed early")
				}
				if len(result.Rows) == 0 {
					continue
				}
				var row struct{ Status, SuccessAt, Label string }
				var body []byte
				switch value := result.Rows[0][0].(type) {
				case string:
					body = []byte(value)
				case []byte:
					body = value
				default:
					t.Fatalf("JSON cell type %T", value)
				}
				if err := json.Unmarshal(body, &row); err != nil {
					t.Fatal(err)
				}
				if row.Status == wantStatus && row.Label == wantLabel {
					return
				}
			case err := <-linkDone:
				t.Fatalf("link ended: %v", err)
			}
		}
	}
	start()
	next("available", "first")
	failure = true
	clock.Advance(claudia.PlanThrottleMinInterval)
	poll()
	next("unavailable", "first")
	if err := replica.Write(func(db *sqlpipe.Database) error {
		return db.Exec(`UPDATE jv_plan_overrides SET label = 'local' WHERE id = 1`)
	}); err != nil {
		t.Fatal(err)
	}
	next("unavailable", "local")
	stop()
	failure = false
	clock.Advance(claudia.BrokerPlanRetryInterval)
	poll()
	// Cached state remains readable during disconnection; reconnect catches up.
	if err := replica.Read(func(db *sqlpipe.Database) error {
		qr, err := db.Query(`SELECT status FROM claudia_plan_usage WHERE provider = 'claude'`)
		if err == nil && qr.Rows[0][0] != "unavailable" {
			t.Fatal("cache changed offline")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := replica.Write(func(db *sqlpipe.Database) error {
		return db.Exec(`UPDATE jv_plan_overrides SET label = 'offline' WHERE id = 1`)
	}); err != nil {
		t.Fatal(err)
	}
	next("unavailable", "offline")
	start()
	next("available", "offline")
	cancel()
	// go test -timeout is the clock for a hang (🎯T97).
	if _, ok := <-updates; ok {
		t.Fatal("cancel retained subscription")
	}
	live, err := replica.SubscribeQuery(t.Context(), `SELECT count(*) FROM claudia_plan_usage`)
	if err != nil {
		t.Fatal(err)
	}
	<-live
	if err := replica.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-linkDone; err != nil {
		t.Fatal(err)
	}
	if _, ok := <-live; ok {
		t.Fatal("Close retained subscription")
	}
}

func TestPlanUsageScheduledFailureRecoversWithoutConsumerRead(t *testing.T) {
	asBroker(t)
	path, socket := brokerDirs(t)
	publishOwned(t, path, socket)
	clock := claudia.NewManualClock(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC))
	cycles := make(chan []claudia.PlanUsage, 2)
	attempts := 0
	monitor := claudia.NewPlanUsageMonitor(&claudia.PlanUsageMonitorArgs{
		Clock: clock, TTL: claudia.BrokerPlanRetryInterval, ReadSnapshot: claudia.ReadBrokerPlanUsage,
		Fetch: func(ctx context.Context) ([]claudia.PlanUsage, error) {
			return claudia.PollBrokerPlanUsage(ctx, clock, func(_ context.Context, p claudia.Provider) (claudia.PlanUsage, error) {
				if p != claudia.ProviderClaude {
					return claudia.PlanUsage{Status: claudia.PlanUsageUnavailable, Reason: "fixture"}, nil
				}
				attempts++
				if attempts == 1 {
					return claudia.PlanUsage{}, errors.New("temporary network failure")
				}
				return claudia.PlanUsage{Status: claudia.PlanUsageAvailable}, nil
			})
		},
		OnUpdate: func(readings []claudia.PlanUsage, err error) {
			if err != nil {
				t.Error(err)
			}
			cycles <- readings
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); monitor.Run(ctx) }()
	defer func() { cancel(); <-done }()
	for cycle := 0; cycle < 2; cycle++ {
		{
			readings := <-cycles
			want := claudia.PlanUsageUnavailable
			if cycle == 1 {
				want = claudia.PlanUsageAvailable
			}
			found := false
			for _, reading := range readings {
				if reading.Provider == claudia.ProviderClaude {
					found = true
					if reading.Status != want {
						t.Fatalf("cycle %d: %+v", cycle, reading)
					}
				}
			}
			if !found {
				t.Fatal("missing provider")
			}
		}
		if cycle == 0 {
			eventually(t, "scheduled timer", func() bool { return clock.Pending() > 0 })
			clock.Advance(claudia.BrokerPlanRetryInterval)
		}
	}
	if attempts != 2 {
		t.Fatal("unexpected attempt count", attempts)
	}
}

// Unrelated local intent writes must not re-run time-dependent projections.
func TestReplicaQueryNativeLocalInvalidation(t *testing.T) {
	replica, err := OpenReplica(filepath.Join(t.TempDir(), JevonsDBFile))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	write := func(sql string) {
		t.Helper()
		if err := replica.Write(func(db *sqlpipe.Database) error { return db.Exec(sql) }); err != nil {
			t.Fatal(err)
		}
	}
	write(`CREATE TABLE jv_local(id INTEGER PRIMARY KEY, value TEXT); INSERT INTO jv_local VALUES(1,'first')`)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results, err := replica.SubscribeQuery(ctx, `SELECT {value, nonce: hex(randomblob(16))} FROM jv_local`)
	if err != nil {
		t.Fatal(err)
	}
	<-results
	if err := replica.Link(ctx, filepath.Join(t.TempDir(), "missing.sock")); err == nil {
		t.Fatal("missing socket unexpectedly connected")
	}
	for _, sql := range []string{
		`CREATE TABLE jv_other(id INTEGER PRIMARY KEY); INSERT INTO jv_other VALUES(1)`,
		`UPDATE jv_local SET value='first'`,
		`BEGIN; UPDATE jv_local SET value='failed'; ROLLBACK`,
		`SAVEPOINT attempt; UPDATE jv_local SET value='failed'; ROLLBACK TO attempt; RELEASE attempt`,
	} {
		write(sql)
		select {
		case result := <-results:
			t.Fatalf("unrelated/rolled-back write emitted: %+v", result)
		default:
		}
	}
	write(`UPDATE jv_local SET value='committed'`)
	select {
	case <-results:
	default:
		t.Fatal("offline committed change did not emit")
	}
}
