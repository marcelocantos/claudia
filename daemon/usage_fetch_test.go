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
	// Only the scheduled producer reaches the provider. Legacy refresh reads
	// remain passive, even if another process edits the old shared JSON cache.
	waitFor(t, "initial persisted Grok reading", func() bool {
		for _, u := range f.d.usage.Read(t.Context(), false).Backends {
			if u.Provider == claudia.ProviderGrok && u.Status == claudia.PlanUsageAvailable {
				return true
			}
		}
		return false
	})
	for _, forced := range []bool{false, true, true} {
		claudia.RecordPlanAttempt(os.Getenv("CLAUDIA_PLAN_CACHE"), claudia.ProviderGrok, time.Now().Add(-time.Hour), http.StatusOK)
		snap := f.d.usage.Read(t.Context(), forced)
		if snap.Err != "" {
			t.Fatal(snap.Err)
		}
	}
	mu.Lock()
	calls := grokCalls
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("read/refresh made %d requests, want scheduled request only", calls)
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
	// Wait for the scheduled cycle to commit, rather than triggering a fetch
	// by reading or changing the obsolete shared throttle file.
	read := func(force bool, want claudia.PlanUsageStatus) claudia.PlanUsage {
		t.Helper()
		got, err := claudia.LoadPlanUsage(t.Context(), &claudia.PlanUsageCacheArgs{Refresh: force})
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range got {
			if u.Provider == claudia.ProviderClaude {
				if u.Status != want {
					t.Fatalf("usage=%+v", u)
				}
				return u
			}
		}
		t.Fatal("Claude missing")
		return claudia.PlanUsage{}
	}
	waitCycle := func(want claudia.PlanUsageStatus) {
		t.Helper()
		waitFor(t, "scheduled usage outcome", func() bool {
			snap := f.d.usage.Read(t.Context(), false)
			for _, u := range snap.Backends {
				if u.Provider == claudia.ProviderClaude {
					return u.Status == want && u.Reason != "usage poll in progress" && (want != claudia.PlanUsageAvailable || u.FetchedAt.Equal(f.clock.Now()))
				}
			}
			return false
		})
		waitFor(t, "monitor cycle complete", func() bool { f.d.usage.Read(t.Context(), false); return f.clock.Pending() > 1 })
	}
	advance := func() { f.clock.Advance(claudia.PlanThrottleMinInterval); f.d.usage.Invalidate() }
	waitCycle(claudia.PlanUsageAvailable)
	read(false, claudia.PlanUsageAvailable)
	put("claude-rotated", time.Now().Add(-time.Hour))
	read(true, claudia.PlanUsageAvailable) // force is a passive read
	advance()
	waitFor(t, "rotated request", func() bool { mu.Lock(); defer mu.Unlock(); return len(tokens) >= 2 })
	waitCycle(claudia.PlanUsageAvailable)
	put("claude-periodic", time.Now().Add(-time.Hour))
	advance()
	waitFor(t, "periodic request", func() bool { mu.Lock(); defer mu.Unlock(); return len(tokens) >= 3 })
	waitCycle(claudia.PlanUsageAvailable)
	before, err := omp.Health(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	status.Store(http.StatusUnauthorized)
	advance()
	waitCycle(claudia.PlanUsageUnavailable)
	u := read(true, claudia.PlanUsageUnavailable)
	if !strings.Contains(u.Reason, "agent login health is unchanged") {
		t.Fatalf("usage rejection reason=%q", u.Reason)
	}
	after, err := omp.Health(t.Context(), store)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("usage poisoned auth health: %v -> %v, %v", before, after, err)
	}
	put("invalid", time.Now().Add(-time.Hour))
	advance()
	waitFor(t, "invalid grant outcome", func() bool {
		for _, u := range f.d.usage.Read(t.Context(), false).Backends {
			if u.Provider == claudia.ProviderClaude {
				return strings.Contains(u.Reason, "refresh grant rejected")
			}
		}
		return false
	})
	u = read(true, claudia.PlanUsageUnavailable)
	if strings.Contains(u.Reason, "health is unchanged") {
		t.Fatalf("refresh rejection falsely claims unchanged auth: %q", u.Reason)
	}
	if err := store.Save(t.Context(), omp.Item{Records: map[string]omp.Record{}}); err != nil {
		t.Fatal(err)
	}
	advance()
	waitFor(t, "missing credential outcome", func() bool {
		for _, u := range f.d.usage.Read(t.Context(), false).Backends {
			if u.Provider == claudia.ProviderClaude {
				return strings.Contains(u.Reason, "credential source unavailable")
			}
		}
		return false
	})

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
