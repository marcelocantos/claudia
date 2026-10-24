// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/omp"
)

type usageRoundTrip func(*http.Request) (*http.Response, error)

func (f usageRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBrokerDefaultUsageMonitorUsesStoreForPeriodicAndForcedFetch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDIA_PLAN_CACHE", t.TempDir())
	t.Setenv("CLAUDIA_CLAUDE_OAUTH_TOKEN", "")
	omp.ResetKeychainShot()
	t.Cleanup(omp.ResetKeychainShot)
	store := omp.Store{Run: func(context.Context, string, ...string) ([]byte, error) { return []byte(`{"records":{}}`), nil }}
	if err := omp.Open(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), omp.XAIOAuth, omp.Record{AccessToken: "broker-current", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../testdata/grok/billing_supergrok.json")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var grokCalls int
	oldClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: usageRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "cli-chat-proxy.grok.com" {
			t.Errorf("unexpected outbound usage host %s", r.URL.Host)
		}
		if r.Header.Get("Authorization") != "Bearer broker-current" {
			t.Error("monitor did not use broker token")
		}
		mu.Lock()
		grokCalls++
		mu.Unlock()
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(raw))), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = oldClient })
	f := newFixture(t)
	opts := f.options(nil)
	opts.UsageFetch = nil
	opts.DisableResume = true
	f.bootWith(t, opts)
	// Exercise the production monitor, rather than only the helper that creates
	// its fetch closures. No Run loop is needed: Read uses the same producers.
	for _, forced := range []bool{false, true} {
		if forced {
			// Between the forced and ordinary floors: this read reaches the
			// vendor only if New retained the forced producer's pacing flag.
			elapsed := (claudia.PlanThrottleMinInterval + claudia.PlanThrottleForcedInterval) / 2
			claudia.RecordPlanAttempt(os.Getenv("CLAUDIA_PLAN_CACHE"), claudia.ProviderGrok, time.Now().Add(-elapsed), http.StatusOK)
		}
		snap := f.d.usage.Read(t.Context(), forced)
		found := false
		for _, u := range snap.Backends {
			if u.Provider == claudia.ProviderGrok {
				found = true
				if u.Status != claudia.PlanUsageAvailable {
					t.Fatalf("forced=%v: %+v", forced, u)
				}
			}
		}
		if !found {
			t.Fatalf("forced=%v: Grok missing from %+v", forced, snap)
		}
	}
	mu.Lock()
	calls := grokCalls
	mu.Unlock()
	if calls != 2 {
		t.Fatalf("normal/forced calls=%d, want 2 (forced fetch must retain its shorter request floor)", calls)
	}
}

func TestBrokerUsageFetcherPreservesInjectedProducer(t *testing.T) {
	count := 0
	fetch, force := brokerUsageFetchers(func(context.Context) ([]claudia.PlanUsage, error) {
		count++
		return []claudia.PlanUsage{{Provider: claudia.ProviderGrok, Reason: "injected"}}, nil
	})
	for _, fn := range []func(context.Context) ([]claudia.PlanUsage, error){fetch, force} {
		got, err := fn(t.Context())
		if err != nil || len(got) != 1 || got[0].Reason != "injected" {
			t.Fatalf("injection lost: %v,%v", got, err)
		}
	}
	if count != 2 {
		t.Fatalf("injected calls=%d", count)
	}
}
