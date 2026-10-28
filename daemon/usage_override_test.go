// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/controlplane"
	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

func TestT234OverrideRefusesFixtureUsageMode(t *testing.T) {
	t.Cleanup(claudia.ClearBrokerUsageOverride)
	dir := t.TempDir()
	_, err := New(Options{
		SocketPath:             dir + "/b.sock",
		StateDir:               dir,
		DisableResume:          true,
		DisableIntel:           true,
		DisableMCPHost:         true,
		Logger:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		FixtureUsage:           true,
		UsageOverrideProviders: []claudia.Provider{claudia.ProviderGrok},
	})
	if err == nil {
		t.Fatal("fixture+override accepted")
	}
}

func TestT234MixedSeatUsageReplica(t *testing.T) {
	t.Cleanup(claudia.ClearBrokerUsageOverride)
	f := newFixture(t)
	var mu sync.Mutex
	var fetched []claudia.Provider
	aheadReset := time.Now().UTC().Add(7 * 24 * time.Hour / 2)
	healthyReset := time.Now().UTC().Add(7 * 24 * time.Hour * 9 / 10)
	opts := f.options(nil)
	opts.UsageFetch = nil
	opts.DisableResume = true
	opts.DisableMCPHost = true
	opts.UsageOverrideProviders = []claudia.Provider{claudia.ProviderGrok}
	opts.UsageProviderFetch = func(_ context.Context, p claudia.Provider) (claudia.PlanUsage, error) {
		mu.Lock()
		fetched = append(fetched, p)
		mu.Unlock()
		if p == claudia.ProviderClaude {
			return overrideWeekly(p, 90, 10, healthyReset), nil
		}
		return claudia.PlanUsage{Provider: p, Status: claudia.PlanUsageUnavailable, Reason: "deterministic"}, nil
	}
	sidecar := false
	opts.StartSidecar = func(context.Context) error {
		sidecar = true
		return nil
	}
	f.bootWith(t, opts)
	if !sidecar {
		t.Fatal("sidecar start skipped")
	}

	replica, err := controlplane.OpenReplica(t.TempDir() + "/consumer.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replica.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = replica.Link(ctx, claudia.ControlSocketPath(f.sock)) }()

	rawSeat(t, f.sock, "seat-t234")
	claudia.RecordObservedReceipt(&claudia.RequestReceipt{
		RequestID: "t234-send", SessionID: "sid-seat-t234", Seat: "seat-t234",
		State: claudia.ReceiptAccepted, AttemptID: "attempt", TurnIDs: []string{},
		AcceptedAt: "2026-10-28T01:00:00.000000000Z", UpdatedAt: "2026-10-28T01:00:00.000000000Z",
	})
	waitFor(t, "seat receipt in replica", func() bool {
		var n int64
		_ = replica.Read(func(db *sqlpipe.Database) error {
			qr, err := db.Query(`SELECT COUNT(*) FROM claudia_request_receipt WHERE request_id = 't234-send'`)
			if err == nil && len(qr.Rows) > 0 {
				n, _ = qr.Rows[0][0].(int64)
			}
			return err
		})
		return n == 1
	})

	grok := overrideWeekly(claudia.ProviderGrok, 30, 70, aheadReset)
	if err := claudia.StoreBrokerUsageOverride([]claudia.PlanUsage{grok}); err != nil {
		t.Fatal(err)
	}
	if err := claudia.CommitBrokerUsageFixture([]claudia.PlanUsage{grok}); err != nil {
		t.Fatal(err)
	}
	waitOverrideReplicaBand(t, replica, claudia.ProviderGrok, claudia.PlanBandAhead, 30)

	waitFor(t, "deterministic claude fetch committed", func() bool {
		u := readOverrideReplica(t, replica)[claudia.ProviderClaude]
		return u.Status == claudia.PlanUsageAvailable && len(u.Windows) > 0 && u.Windows[0].RemainingPercent != nil && *u.Windows[0].RemainingPercent == 90
	})

	hot := overrideWeekly(claudia.ProviderGrok, 0, 100, healthyReset)
	if err := claudia.StoreBrokerUsageOverride([]claudia.PlanUsage{hot}); err != nil {
		t.Fatal(err)
	}
	if err := claudia.CommitBrokerUsageFixture([]claudia.PlanUsage{hot}); err != nil {
		t.Fatal(err)
	}
	waitOverrideReplicaBand(t, replica, claudia.ProviderGrok, claudia.PlanBandExhausted, 0)
	claude := readOverrideReplica(t, replica)[claudia.ProviderClaude]
	if claude.Status != claudia.PlanUsageAvailable || claude.Windows[0].RemainingPercent == nil || *claude.Windows[0].RemainingPercent != 90 {
		t.Fatalf("non-overridden claude mutated: %+v", claude)
	}
	mu.Lock()
	got := append([]claudia.Provider(nil), fetched...)
	mu.Unlock()
	for _, p := range got {
		if p == claudia.ProviderGrok {
			t.Fatalf("overridden grok reached real fetch: %v", got)
		}
	}
}

func overrideWeekly(provider claudia.Provider, remaining, used float64, reset time.Time) claudia.PlanUsage {
	return claudia.PlanUsage{Provider: provider, Status: claudia.PlanUsageAvailable, Windows: []claudia.PlanWindow{{
		Name: claudia.PlanWindowWeekly, RemainingPercent: &remaining, UsedPercent: &used, ResetsAt: &reset, LimitWindow: 7 * 24 * time.Hour,
	}}}
}

func readOverrideReplica(t *testing.T, r *controlplane.Replica) map[claudia.Provider]claudia.PlanUsage {
	t.Helper()
	result := map[claudia.Provider]claudia.PlanUsage{}
	err := r.Read(func(db *sqlpipe.Database) error {
		q, err := db.Query("SELECT provider, status, success_at, reading_json FROM claudia_plan_usage")
		if err != nil {
			return err
		}
		for _, row := range q.Rows {
			provider, status, success, body := fmt.Sprint(row[0]), fmt.Sprint(row[1]), fmt.Sprint(row[2]), fmt.Sprint(row[3])
			if status != string(claudia.PlanUsageAvailable) {
				continue
			}
			var usage claudia.PlanUsage
			if err := json.Unmarshal([]byte(body), &usage); err != nil {
				return err
			}
			if success == "" || usage.Provider != claudia.Provider(provider) {
				return fmt.Errorf("invalid published row for %s", provider)
			}
			result[usage.Provider] = usage
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func waitOverrideReplicaBand(t *testing.T, r *controlplane.Replica, provider claudia.Provider, band claudia.PlanBand, remaining float64) {
	t.Helper()
	updates, err := r.SubscribeQuery(t.Context(), "SELECT provider, status, success_at, reading_json FROM claudia_plan_usage")
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case _, ok := <-updates:
			if !ok {
				t.Fatal("replica subscription closed")
			}
			if u, found := readOverrideReplica(t, r)[provider]; found && len(u.Windows) > 0 && u.Windows[0].RemainingPercent != nil && *u.Windows[0].RemainingPercent == remaining {
				if u.Windows[0].Band != band || claudia.ClassifyPlan(u, u.FetchedAt, nil).Weekly != band {
					t.Fatalf("%s band=%s want %s", provider, u.Windows[0].Band, band)
				}
				return
			}
		case <-t.Context().Done():
			t.Fatalf("replica did not deliver %s %s/%v: %v", provider, band, remaining, t.Context().Err())
		}
	}
}
