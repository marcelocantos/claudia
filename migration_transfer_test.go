// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestMigrationSummaryNeverFallsBackWithoutBroker(t *testing.T) {
	t.Setenv("CLAUDIA_NO_BROKER", "1")
	cfg := migrationTransferConfig(ProviderClaude, "claude-sonnet-5", t.TempDir())
	_, err := StartContext(context.Background(), cfg)
	if !errors.Is(err, errNoBroker) {
		t.Fatalf("summary seat should refuse direct startup without broker: %v", err)
	}
}

func TestMigrationSummaryUsesStandardModelOnDestination(t *testing.T) {
	for _, tc := range []struct {
		provider Provider
		model    string
	}{
		{ProviderClaude, "claude-sonnet-5"},
		{ProviderGrok, "grok-4.5"},
		{ProviderCodex, "gpt-6-sol"},
		{ProviderCursor, "composer-2.5"},
	} {
		got, err := migrationSummaryModel(context.Background(), tc.provider)
		if err != nil || got != tc.model {
			t.Errorf("%s summary model = %q, err=%v; want %q", tc.provider, got, err, tc.model)
		}
	}
}

func TestMigrationTransferBoundsHistoryAndHasNoTools(t *testing.T) {
	if got := tailRunes("begin:"+strings.Repeat("a", 100)+":end", 8); got != "aaaa:end" {
		t.Fatalf("tail = %q", got)
	}
	for _, provider := range []Provider{ProviderClaude, ProviderGrok, ProviderCodex, ProviderCursor} {
		cfg := migrationTransferConfig(provider, "summary-model", t.TempDir())
		if PlanProvider(cfg.Provider) != provider || cfg.Model != "summary-model" || !cfg.SummaryOnly {
			t.Fatalf("%s config = %+v", provider, cfg)
		}
		if cfg.Name == "" || cfg.WorkDir == "" {
			t.Fatalf("%s disposable seat identity = %+v", provider, cfg)
		}
	}
}
