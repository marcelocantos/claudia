// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
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
