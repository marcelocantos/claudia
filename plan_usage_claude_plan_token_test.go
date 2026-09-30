// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// The Claude bar reads usage with the broker's own Anthropic plan login when
// no token is given: on 2026-10-01 Claude Code's keychain item stopped
// carrying claudeAiOauth and the bar went blank while the fleet worked.
func TestClaudeUsageUsesTheBrokersPlanLogin(t *testing.T) {
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	ompKeychain = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"records":{"anthropic":{"refresh_token":"r","access_token":"plan-token","expiry":"` + exp + `"}}}`), nil
	}
	omp.ResetKeychainShot()
	t.Cleanup(func() { ompKeychain = nil; omp.ResetKeychainShot() })
	if err := OpenOMPPlans(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Setenv(claudeOAuthTokenEnv, "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer plan-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":30,"resets_at":"2026-10-01T05:00:00Z"},"seven_day":{"utilization":40,"resets_at":"2026-10-05T00:00:00Z"}}`))
	}))
	defer srv.Close()

	pu, err := QueryPlanUsage(context.Background(), &PlanUsageArgs{
		Provider:       ProviderClaude,
		HTTPClient:     srv.Client(),
		ClaudeUsageURL: srv.URL,
		Now:            time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pu.Status != PlanUsageAvailable || len(pu.Windows) != 2 {
		t.Fatalf("status=%q reason=%q windows=%d", pu.Status, pu.Reason, len(pu.Windows))
	}
}
