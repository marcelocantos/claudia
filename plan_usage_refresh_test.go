// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

func usageRefreshStore(t *testing.T) (omp.Store, *atomic.Int32) {
	t.Helper()
	omp.ResetKeychainShot()
	t.Cleanup(omp.ResetKeychainShot)
	ompRenewFailedMu.Lock()
	clear(ompRenewFailed)
	ompRenewFailedMu.Unlock()
	t.Cleanup(func() {
		ompRenewFailedMu.Lock()
		clear(ompRenewFailed)
		ompRenewFailedMu.Unlock()
	})
	oldPath, _ := omp.DefaultDataPath()
	omp.UseTestDataPath(filepath.Join(t.TempDir(), "plans.enc"))
	t.Cleanup(func() { omp.UseTestDataPath(oldPath) })
	store := planStore()
	reads := new(atomic.Int32)
	store.Run = func(context.Context, string, ...string) ([]byte, error) {
		reads.Add(1)
		return []byte(`{"data_key":"` + strings.Repeat("02", 32) + `"}`), nil
	}
	if err := omp.Open(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	previous := ompLogin
	t.Cleanup(func() { ompLogin = previous })
	t.Setenv(omp.NoNetworkEnv, "")
	return store, reads
}

func TestBrokerUsageLegacyStoreNeverRenewsOrMigrates(t *testing.T) {
	usageRefreshStore(t) // Isolate persistence and enable the real renewal path.
	omp.ResetKeychainShot()
	previousRun, previousStdin := ompKeychain, ompKeychainStdin
	t.Cleanup(func() { ompKeychain, ompKeychainStdin = previousRun, previousStdin })
	reads, writes, renewals := 0, 0, 0
	ompKeychain = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "find-generic-password" {
			reads++
			return json.Marshal(omp.Item{Records: map[string]omp.Record{
				omp.Anthropic: {AccessToken: "expired", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Hour)},
			}})
		}
		writes++
		return nil, errors.New("unexpected legacy Keychain write")
	}
	ompKeychainStdin = func(context.Context, []byte, string, ...string) ([]byte, error) {
		writes++
		return nil, errors.New("unexpected legacy Keychain write")
	}
	ompLogin = omp.Login{Script: "fixture", Run: func(context.Context, string, ...string) ([]byte, error) {
		renewals++
		return json.Marshal(omp.Record{AccessToken: "fresh", RefreshToken: "rotated", Expiry: time.Now().Add(time.Hour)})
	}}
	if err := omp.Open(t.Context(), planStore()); err != nil {
		t.Fatal(err)
	}
	if _, err := brokerUsageToken(t.Context(), omp.Anthropic); err == nil || !strings.Contains(err.Error(), "encrypted plan store not established") {
		t.Fatalf("legacy renewal refusal=%v", err)
	}
	if renewals != 0 || reads != 1 || writes != 0 {
		t.Fatalf("legacy background activity: renewals=%d Keychain reads=%d writes=%d", renewals, reads, writes)
	}
}

func TestUsageSharesProactiveRenewalFailureBackoff(t *testing.T) {
	store, _ := usageRefreshStore(t)
	if err := store.Put(t.Context(), omp.Anthropic, omp.Record{AccessToken: "expired", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	ompLogin = omp.Login{Script: "fixture", Run: func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("network unavailable")
	}}
	if _, err := RenewExpiringOMPPlans(t.Context(), OMPRenewMargin); err == nil {
		t.Fatal("proactive renewal unexpectedly succeeded")
	}
	if _, err := brokerUsageToken(t.Context(), omp.Anthropic); err == nil || !strings.Contains(err.Error(), "backoff") || calls != 1 {
		t.Fatalf("usage ignored proactive backoff: err=%v calls=%d", err, calls)
	}
	// The shared retry clock, not an independent usage cooldown, admits the
	// next attempt. Its refusal is then the existing auth rejection state.
	ompRenewFailedMu.Lock()
	ompRenewFailed[omp.Anthropic] = time.Now().Add(-ompRenewRetry)
	ompRenewFailedMu.Unlock()
	ompLogin.Run = func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return nil, errors.New("invalid_grant")
	}
	if _, err := brokerUsageToken(t.Context(), omp.Anthropic); err == nil {
		t.Fatal("invalid grant succeeded")
	}
	if _, err := RenewExpiringOMPPlans(t.Context(), OMPRenewMargin); err != nil || calls != 2 {
		t.Fatalf("proactive loop retried usage rejection: err=%v calls=%d", err, calls)
	}
}

func TestUsageRenewalCancellationStopsNonreadingSeatFanout(t *testing.T) {
	store, _ := usageRefreshStore(t)
	if err := store.Put(t.Context(), omp.Anthropic, omp.Record{AccessToken: "old", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ctrl, _, _, peer, _ := receiptPipe(t)
	ctrl.mu.Lock()
	ctrl.provider, ctrl.token = omp.Anthropic, "old"
	ctrl.mu.Unlock()
	ompSeats.Store(ctrl, struct{}{})
	t.Cleanup(func() { ompSeats.Delete(ctrl) })
	ompLogin = omp.Login{Script: "fixture", Run: func(context.Context, string, ...string) ([]byte, error) {
		return json.Marshal(omp.Record{AccessToken: strings.Repeat("x", 16<<20), RefreshToken: "next", Expiry: time.Now().Add(time.Hour)})
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := brokerUsageToken(ctx, omp.Anthropic); result <- err }()
	// Observe a real write start, then leave the peer unread. Cancelling the
	// renewal must stop both the token send and its fallback load write.
	var first [1]byte
	if _, err := io.ReadFull(peer, first[:]); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled fanout=%v", err)
	}
	if !ompRefreshLock(omp.Anthropic).TryLock() {
		t.Fatal("cancelled renewal kept auth lock")
	}
	ompRefreshLock(omp.Anthropic).Unlock()
	if ctrl.currentToken() != "old" {
		t.Fatal("failed seat write claimed to update the seat token")
	}
}

func TestNoninteractiveAuthErrorsNeverExposeCredentialOutput(t *testing.T) {
	store, _ := usageRefreshStore(t)
	if err := store.Put(t.Context(), omp.Anthropic, omp.Record{AccessToken: "expired", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	login := omp.Login{Script: "fixture", NoLogin: true, Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"access_token":"ok","refresh_token":"ok","expiry":"secret-test-token"}`), nil
	}}
	if _, err := login.Refresh(t.Context(), store, omp.Anthropic); err == nil || strings.Contains(err.Error(), "secret-test-token") {
		t.Fatalf("credential JSON leak: %v", err)
	}
	script := filepath.Join(t.TempDir(), "auth.ts")
	if err := os.WriteFile(script, []byte(`console.error("invalid_grant secret-test-token"); process.exit(1);`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDIA_OMP_AUTH", script)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = w
	_, callErr := execBunLogin(t.Context(), "bun", script, "refresh", omp.Anthropic, "--no-login", `{}`)
	os.Stderr = previous
	_ = w.Close()
	raw, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || len(raw) != 0 || callErr == nil || !strings.Contains(callErr.Error(), "invalid_grant") || strings.Contains(callErr.Error(), "secret-test-token") {
		t.Fatalf("helper stderr=%q err=%v read=%v", raw, callErr, err)
	}
}

func TestBrokerUsageRefreshCoalescesAndPersistsBothPlans(t *testing.T) {
	store, reads := usageRefreshStore(t)
	for _, plan := range []string{omp.Anthropic, omp.XAIOAuth} {
		t.Run(plan, func(t *testing.T) {
			if err := store.Put(t.Context(), plan, omp.Record{AccessToken: "expired", RefreshToken: "refresh-" + plan, Expiry: time.Now().Add(-time.Hour)}); err != nil {
				t.Fatal(err)
			}
			entered, finish := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			ompLogin = omp.Login{Script: "fixture", ForceLogin: true, Run: func(ctx context.Context, _ string, args ...string) ([]byte, error) {
				calls.Add(1)
				if len(args) != 5 || args[1] != "refresh" || args[2] != plan || args[3] != "--no-login" {
					t.Error("usage did not force noninteractive refresh")
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > planUsageRefreshTimeout {
					t.Error("refresh has no production deadline")
				}
				close(entered)
				select {
				case <-finish:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return json.Marshal(omp.Record{AccessToken: "fresh-" + plan, RefreshToken: "next-" + plan, Expiry: time.Now().Add(time.Hour)})
			}}
			result := make(chan error, 1)
			go func() {
				token, err := brokerUsageToken(t.Context(), plan)
				if err == nil && token != "fresh-"+plan {
					err = errors.New("wrong renewed token")
				}
				result <- err
			}()
			select {
			case <-entered:
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
			// This is the same lock held by agent rejection/recovery, not an
			// independent usage lock that could spend the refresh token twice.
			if ompRefreshLock(plan).TryLock() {
				ompRefreshLock(plan).Unlock()
				t.Fatal("renewal did not hold the auth owner's lock")
			}
			if _, err := brokerUsageToken(t.Context(), plan); err == nil || !strings.Contains(err.Error(), "in progress") {
				t.Fatalf("concurrent renewal = %v", err)
			}
			close(finish)
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			if token, err := brokerUsageToken(t.Context(), plan); err != nil || token != "fresh-"+plan || calls.Load() != 1 {
				t.Fatalf("reuse token=%q err=%v calls=%d", token, err, calls.Load())
			}
		})
	}
	if reads.Load() != 1 {
		t.Fatal("renewal read the Keychain again")
	}
	// Reopen only in the fixture to prove Login.Refresh persisted both rotated
	// records before returning. Production usage never invokes Open/RetryOpen.
	omp.ResetKeychainShot()
	if err := omp.Open(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	for _, plan := range []string{omp.Anthropic, omp.XAIOAuth} {
		if token, err := store.AccessToken(t.Context(), plan); err != nil || token != "fresh-"+plan {
			t.Fatalf("persisted %s: %q %v", plan, token, err)
		}
	}
}

func TestBrokerGrokUsageRenewsOnlyExpiredAccess(t *testing.T) {
	store, reads := usageRefreshStore(t)
	if err := store.Put(t.Context(), omp.XAIOAuth, omp.Record{AccessToken: "old", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	ompLogin = omp.Login{Script: "fixture", Run: func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return json.Marshal(omp.Record{AccessToken: "grok-fresh", RefreshToken: "next", Expiry: time.Now().Add(time.Hour)})
	}}
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer grok-fresh" {
			t.Error("billing did not use renewed access")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":25}}`))
	}))
	defer srv.Close()
	args := BrokerPlanFetchArgs()
	args.Providers, args.ThrottleDir = []Provider{ProviderGrok}, ""
	args.HTTPClient, args.GrokBillingURL = srv.Client(), srv.URL
	for _, code := range []int{http.StatusOK, http.StatusUnauthorized} {
		status = code
		got, err := QueryAllPlanUsage(t.Context(), args)
		if err != nil || len(got) != 1 {
			t.Fatalf("usage=%+v err=%v", got, err)
		}
		if code == http.StatusUnauthorized && got[0].Status != PlanUsageUnavailable {
			t.Fatalf("rejection lost: %+v", got)
		}
	}
	if calls != 1 || reads.Load() != 1 {
		t.Fatalf("usage retried auth/Keychain: renewals=%d reads=%d", calls, reads.Load())
	}
}

func TestBrokerUsageRefreshFailureCancellationAndUnavailableStore(t *testing.T) {
	store, reads := usageRefreshStore(t)
	put := func(token, refresh string) {
		t.Helper()
		if err := store.Put(t.Context(), omp.Anthropic, omp.Record{AccessToken: token, RefreshToken: refresh, Expiry: time.Now().Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	ompLogin = omp.Login{Script: "fixture", Run: func(context.Context, string, ...string) ([]byte, error) {
		calls.Add(1)
		return nil, errors.New("invalid_grant secret-test-token")
	}}
	put("invalid-grant-test", "old-refresh")
	for range 3 {
		_, err := brokerUsageToken(t.Context(), omp.Anthropic)
		if err == nil || !strings.Contains(err.Error(), "sign-in required") || strings.Contains(err.Error(), "secret-test-token") {
			t.Fatalf("failure=%v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("failed renewal storm")
	}
	put("nonrefreshable", "")
	if _, err := brokerUsageToken(t.Context(), omp.Anthropic); err == nil || !strings.Contains(err.Error(), "without a refresh") {
		t.Fatalf("nonrefreshable=%v", err)
	}
	if err := store.Put(t.Context(), omp.XAIOAuth, omp.Record{AccessToken: "cancel-test", RefreshToken: "new-refresh", Expiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	ompLogin.Run = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { _, err := brokerUsageToken(ctx, omp.XAIOAuth); result <- err }()
	select {
	case <-entered:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	cancel()
	if err := <-result; err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancel=%v", err)
	}
	omp.ResetKeychainShot()
	if _, err := brokerUsageToken(t.Context(), omp.Anthropic); err == nil || !strings.Contains(err.Error(), "store unavailable") {
		t.Fatalf("unopened store=%v", err)
	}
	if reads.Load() != 1 {
		t.Fatal("usage retried opening the Keychain")
	}
}
