// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
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
	// New starts the production monitor; Read joins its initial fetch.
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

func TestBrokerClaudeUsageMonitorRotationRejectionAndPacing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDIA_PLAN_CACHE", t.TempDir())
	// Broker usage must not silently take credentials from the environment.
	t.Setenv("CLAUDIA_CLAUDE_OAUTH_TOKEN", "wrong-environment-account")
	omp.ResetKeychainShot()
	t.Cleanup(omp.ResetKeychainShot)
	oldPath, _ := omp.DefaultDataPath()
	dataPath := filepath.Join(t.TempDir(), "plans.enc")
	omp.UseTestDataPath(dataPath)
	t.Cleanup(func() { omp.UseTestDataPath(oldPath) })
	// Run the production Go auth runner with a self-contained, no-network
	// process fixture. The separate auth.ts test pins the real helper policy.
	root := t.TempDir()
	script := filepath.Join(root, "auth.ts")
	marker := filepath.Join(root, "attempts")
	if err := os.WriteFile(script, []byte(`import { appendFileSync } from "node:fs";
const [verb, provider, policy] = process.argv.slice(2);
if (verb !== "refresh" || policy !== "--no-login") throw new Error("interactive auth forbidden");
const record = JSON.parse(await new Response(Bun.stdin).text());
appendFileSync(process.env.USAGE_TEST_ATTEMPTS!, provider + "\n");
if (record.access_token === "invalid") { console.error("invalid_grant"); process.exit(1); }
process.stdout.write(JSON.stringify({access_token: record.access_token + "-renewed", refresh_token: "next-refresh", expiry: new Date(Date.now() + 3600000).toISOString()}));
`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDIA_OMP_AUTH", script)
	t.Setenv("USAGE_TEST_ATTEMPTS", marker)
	t.Setenv(omp.NoNetworkEnv, "")
	var keychainReads atomic.Int32
	store := omp.Store{DataPath: dataPath, Run: func(context.Context, string, ...string) ([]byte, error) {
		keychainReads.Add(1)
		return []byte(`{"data_key":"` + strings.Repeat("01", 32) + `"}`), nil
	}}
	if err := omp.Open(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	put := func(token string, expiry time.Time) {
		t.Helper()
		if err := store.Put(t.Context(), omp.Anthropic, omp.Record{AccessToken: token, RefreshToken: "fake-refresh", Expiry: expiry}); err != nil {
			t.Fatal(err)
		}
	}
	put("claude-initial", time.Now().Add(-time.Hour))
	var status atomic.Int32
	status.Store(http.StatusOK)
	var mu sync.Mutex
	var tokens []string
	firstRequest := make(chan struct{}, 1)
	oldClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: usageRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.anthropic.com/api/oauth/usage" {
			t.Errorf("unexpected usage endpoint %s", r.URL)
		}
		mu.Lock()
		tokens = append(tokens, r.Header.Get("Authorization"))
		mu.Unlock()
		select {
		case firstRequest <- struct{}{}:
		default:
		}
		return &http.Response{StatusCode: int(status.Load()), Body: io.NopCloser(strings.NewReader(`{"five_hour":{"utilization":25}}`)), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = oldClient })
	f := newFixture(t)
	opts := f.options(nil)
	opts.UsageFetch, opts.DisableResume = nil, true
	f.bootWith(t, opts)
	// Observe the periodic monitor's initial request before making any read.
	select {
	case <-firstRequest:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	read := func(force bool, want claudia.PlanUsageStatus) claudia.PlanUsage {
		t.Helper()
		// This reaches daemon.Handle(TypeUsage), including its forced producer.
		got, err := claudia.LoadPlanUsage(t.Context(), &claudia.PlanUsageCacheArgs{Refresh: force})
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range got {
			if u.Provider == claudia.ProviderClaude {
				if u.Status != want {
					t.Fatalf("force=%v usage=%+v", force, u)
				}
				return u
			}
		}
		t.Fatal("Claude missing from broker usage response")
		return claudia.PlanUsage{}
	}
	read(false, claudia.PlanUsageAvailable)
	put("claude-rotated", time.Now().Add(-time.Hour))
	// Inside the forced floor: even the forced request must not hit HTTP.
	read(true, claudia.PlanUsageAvailable)
	betweenFloors := (claudia.PlanThrottleMinInterval + claudia.PlanThrottleForcedInterval) / 2
	allowForced := func() {
		claudia.RecordPlanAttempt(os.Getenv("CLAUDIA_PLAN_CACHE"), claudia.ProviderClaude, time.Now().Add(-betweenFloors), http.StatusOK)
	}
	allowForced()
	read(true, claudia.PlanUsageAvailable)
	put("claude-periodic", time.Now().Add(-time.Hour))
	claudia.RecordPlanAttempt(os.Getenv("CLAUDIA_PLAN_CACHE"), claudia.ProviderClaude, time.Now().Add(-2*claudia.PlanThrottleMinInterval), http.StatusOK)
	// Drain the forced request's event, then require the Run loop to fetch
	// with its ordinary producer before the next consumer read.
	select {
	case <-firstRequest:
	default:
	}
	f.d.usage.Invalidate()
	select {
	case <-firstRequest:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	read(false, claudia.PlanUsageAvailable)
	before, err := omp.Health(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	status.Store(http.StatusUnauthorized)
	allowForced()
	u := read(true, claudia.PlanUsageUnavailable)
	if !strings.Contains(u.Reason, "agent login health is unchanged") {
		t.Fatalf("usage rejection reason=%q", u.Reason)
	}
	after, err := omp.Health(t.Context(), store)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("usage poisoned auth health: %v -> %v, %v", before, after, err)
	}
	put("invalid", time.Now().Add(-time.Hour))
	allowForced()
	u = read(true, claudia.PlanUsageUnavailable)
	if !strings.Contains(u.Reason, "refresh grant rejected") {
		t.Fatalf("invalid grant hidden: %q", u.Reason)
	}
	if strings.Contains(u.Reason, "health is unchanged") {
		t.Fatalf("refresh rejection falsely claims unchanged auth: %q", u.Reason)
	}
	// Even if a caller independently clears the usage floor, the failed
	// credential's refresh attempt remains coalesced for that floor.
	allowForced()
	read(true, claudia.PlanUsageUnavailable)
	if err := store.Save(t.Context(), omp.Item{Records: map[string]omp.Record{}}); err != nil {
		t.Fatal(err)
	}
	allowForced()
	u = read(true, claudia.PlanUsageUnavailable)
	if !strings.Contains(u.Reason, "credential source unavailable") || strings.Contains(u.Reason, "probe removed") {
		t.Fatalf("missing credential hidden: %q", u.Reason)
	}
	mu.Lock()
	gotTokens := append([]string(nil), tokens...)
	mu.Unlock()
	if !reflect.DeepEqual(gotTokens, []string{"Bearer claude-initial-renewed", "Bearer claude-rotated-renewed", "Bearer claude-periodic-renewed", "Bearer claude-periodic-renewed"}) || keychainReads.Load() != 1 {
		t.Fatalf("rotation/pacing/recovery boundary: tokens=%v Keychain=%d", gotTokens, keychainReads.Load())
	}
	attempts, err := os.ReadFile(marker)
	if err != nil || string(attempts) != strings.Repeat(omp.Anthropic+"\n", 4) {
		t.Fatalf("renewal attempts=%q err=%v", attempts, err)
	}
}
