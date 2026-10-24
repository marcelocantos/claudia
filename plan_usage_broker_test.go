// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

func TestBrokerGrokBillingUsesCurrentStoreAndDoesNotRenew(t *testing.T) {
	ctx := t.Context()
	omp.ResetKeychainShot()
	t.Cleanup(omp.ResetKeychainShot)
	store := omp.Store{Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"records":{}}`), nil
	}}
	if err := omp.Open(ctx, store); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/grok/billing_supergrok.json")
	if err != nil {
		t.Fatal(err)
	}
	status := http.StatusOK
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Authorization"))
		if r.Header.Get("X-XAI-Token-Auth") != "xai-grok-cli" {
			t.Error("missing billing product header")
		}
		w.WriteHeader(status)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()
	args := BrokerPlanFetchArgs()
	args.Providers, args.ThrottleDir = []Provider{ProviderGrok}, ""
	args.HTTPClient, args.GrokBillingURL = srv.Client(), srv.URL
	// A valid CLI file would conceal a broker-store failure if fallback leaked.
	args.GrokAuthPath = t.TempDir() + "/auth.json"
	cliToken := "stale-cli-" + strings.Repeat("A", 210)
	if err := os.WriteFile(args.GrokAuthPath, []byte(`{"https://auth.x.ai::client":{"key":"`+cliToken+`"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"broker-before", "broker-after"} {
		if err := store.Put(ctx, omp.XAIOAuth, omp.Record{AccessToken: token, Expiry: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		got, err := QueryAllPlanUsage(ctx, args)
		if err != nil || len(got) != 1 || got[0].Status != PlanUsageAvailable {
			t.Fatalf("fetch = %+v, %v", got, err)
		}
	}
	if !reflect.DeepEqual(tokens, []string{"Bearer broker-before", "Bearer broker-after"}) {
		t.Fatalf("tokens = %v", tokens)
	}
	before, err := omp.Health(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	status = http.StatusUnauthorized
	got, err := QueryAllPlanUsage(ctx, args)
	if err != nil || len(got) != 1 || got[0].Status != PlanUsageUnavailable || !strings.Contains(got[0].Reason, "broker subscription credential") {
		t.Fatalf("401 = %+v, %v", got, err)
	}
	if len(tokens) != 3 {
		t.Fatalf("401 retried: %d calls", len(tokens))
	}
	after, err := omp.Health(ctx, store)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("billing changed health: %v -> %v, %v", before, after, err)
	}
	for _, item := range []omp.Item{{Records: map[string]omp.Record{}}, {Records: map[string]omp.Record{omp.XAIOAuth: {AccessToken: "expired", Expiry: time.Now().Add(-time.Hour)}}}} {
		if err := store.Save(ctx, item); err != nil {
			t.Fatal(err)
		}
		got, err := QueryAllPlanUsage(ctx, args)
		if err != nil || len(got) != 1 || got[0].Status != PlanUsageUnavailable || !strings.Contains(got[0].Reason, "recover the Grok subscription login in Claudia") {
			t.Fatalf("missing/expired = %+v, %v", got, err)
		}
	}
	if len(tokens) != 3 {
		t.Fatalf("missing/expired credential reached network: %v", tokens)
	}
}

func TestGrokBillingExplicitTokenPrecedesTokenSource(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer explicit" {
			t.Error("explicit token lost")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"config": map[string]any{"creditUsagePercent": 25}})
	}))
	defer srv.Close()
	_, err := fetchGrokBilling(t.Context(), &PlanUsageArgs{GrokAccessToken: "explicit", GrokBillingURL: srv.URL, GrokTokenSource: func(context.Context) (string, error) { called = true; return "source", nil }})
	if err != nil || called {
		t.Fatalf("fetch error=%v, source called=%v", err, called)
	}
}

func TestBrokerClaudeUsageCurrentCredentialAndUsageOnlyFailures(t *testing.T) {
	ctx := t.Context()
	t.Setenv(claudeOAuthTokenEnv, "unrelated-environment-token")
	omp.ResetKeychainShot()
	t.Cleanup(omp.ResetKeychainShot)
	keychainReads := 0
	store := omp.Store{Run: func(context.Context, string, ...string) ([]byte, error) {
		keychainReads++
		return []byte(`{"records":{}}`), nil
	}}
	if err := omp.Open(ctx, store); err != nil {
		t.Fatal(err)
	}
	status := http.StatusOK
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Authorization"))
		if r.Header.Get("anthropic-beta") != claudeOAuthBetaHeader {
			t.Error("missing Claude OAuth beta header")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":25}}`))
	}))
	defer srv.Close()
	args := BrokerPlanFetchArgs()
	args.Providers, args.ThrottleDir = []Provider{ProviderClaude}, ""
	args.HTTPClient, args.ClaudeUsageURL = srv.Client(), srv.URL
	for _, token := range []string{"claude-before", "claude-after"} {
		if err := store.Put(ctx, omp.Anthropic, omp.Record{AccessToken: token, Expiry: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		got, err := QueryAllPlanUsage(ctx, args)
		if err != nil || len(got) != 1 || got[0].Status != PlanUsageAvailable {
			t.Fatalf("fetch = %+v, %v", got, err)
		}
	}
	if !reflect.DeepEqual(tokens, []string{"Bearer claude-before", "Bearer claude-after"}) {
		t.Fatalf("tokens = %v", tokens)
	}
	before, err := omp.Health(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		status = code
		got, err := QueryAllPlanUsage(ctx, args)
		if err != nil || len(got) != 1 || got[0].Status != PlanUsageUnavailable || !strings.Contains(got[0].Reason, "agent login health is unchanged") {
			t.Fatalf("rejected = %+v, %v", got, err)
		}
	}
	after, err := omp.Health(ctx, store)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("usage changed health: %v -> %v, %v", before, after, err)
	}
	for _, item := range []omp.Item{
		{Records: map[string]omp.Record{}},
		{Records: map[string]omp.Record{omp.Anthropic: {AccessToken: "expired", RefreshToken: "still-refreshable", Expiry: time.Now().Add(-time.Hour)}}},
	} {
		if err := store.Save(ctx, item); err != nil {
			t.Fatal(err)
		}
		got, err := QueryAllPlanUsage(ctx, args)
		if err != nil || len(got) != 1 || got[0].Status != PlanUsageUnavailable || !strings.Contains(got[0].Reason, "credential source unavailable") || strings.Contains(got[0].Reason, "probe removed") {
			t.Fatalf("missing/expired = %+v, %v", got, err)
		}
		if rec, ok := item.Records[omp.Anthropic]; ok && rec.RefreshToken != "" {
			health, err := omp.Health(ctx, store)
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range health {
				if h.Provider == omp.Anthropic && h.State != omp.HealthOK {
					t.Fatalf("refresh-capable login health changed: %+v", h)
				}
			}
		}
	}
	if len(tokens) != 4 || keychainReads != 1 {
		t.Fatalf("unexpected retry, fallback or Keychain probe: HTTP=%d Keychain=%d", len(tokens), keychainReads)
	}
}

func TestClaudeUsageCredentialPrecedence(t *testing.T) {
	t.Setenv(claudeOAuthTokenEnv, "environment")
	for _, tc := range []struct {
		name, explicit, source string
		err                    error
		wantToken              string
		wantSource             bool
	}{
		{name: "explicit", explicit: "explicit", source: "source", wantToken: "explicit"},
		{name: "source", source: "source", wantToken: "source", wantSource: true},
		{name: "empty-source", source: "  ", wantSource: true},
		{name: "failed-source", err: errors.New("not open"), wantSource: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called, requests := false, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if tc.wantToken == "" || r.Header.Get("Authorization") != "Bearer "+tc.wantToken {
					t.Error("incorrect credential precedence")
				}
				_, _ = w.Write([]byte(`{"five_hour":{"utilization":25}}`))
			}))
			defer srv.Close()
			got, err := QueryPlanUsage(t.Context(), &PlanUsageArgs{Provider: ProviderClaude, ClaudeAccessToken: tc.explicit,
				ClaudeUsageURL: srv.URL, HTTPClient: srv.Client(), ClaudeTokenSource: func(context.Context) (string, error) {
					called = true
					return tc.source, tc.err
				}})
			if err != nil || called != tc.wantSource {
				t.Fatalf("fetch err=%v source=%v", err, called)
			}
			if tc.wantToken == "" {
				if requests != 0 || got.Status != PlanUsageUnavailable {
					t.Fatalf("failed source fell back: requests=%d usage=%+v", requests, got)
				}
			} else if requests != 1 || got.Status != PlanUsageAvailable {
				t.Fatalf("fetch requests=%d usage=%+v", requests, got)
			}
		})
	}
}
