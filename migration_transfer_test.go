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

func TestAgentMigrateSummarizesRetainedHistoryBeforeMoving(t *testing.T) {
	agent, _ := startMigrateFixture(t, ProviderClaude, "summary-source")
	agent.PublishEvent(Event{Type: "user", Text: "finish the violet migration"})
	agent.PublishEvent(Event{Type: "progress", ProgressType: ProgressToolUse, ToolTitle: "Bash"})
	var got MigrationTransferArgs
	agent.migrationSummarizer = func(_ context.Context, args MigrationTransferArgs) (MigrationTransferResult, error) {
		got = args
		return MigrationTransferResult{Brief: "violet handover"}, nil
	}
	var delivered string
	agent.ops.migrate = func(_ *Agent, args *MigrateArgs) error {
		delivered = args.ContextBrief
		return nil
	}
	if err := agent.Migrate(&MigrateArgs{Provider: ProviderGrok, Model: "grok-4"}); err != nil {
		t.Fatal(err)
	}
	if got.Destination != ProviderGrok || !strings.Contains(got.Transcript, "violet migration") ||
		!strings.Contains(got.Transcript, "inert tool names: Bash") || delivered != "violet handover" {
		t.Fatalf("transfer args=%+v, successor brief=%q", got, delivered)
	}
}

func TestAgentMigrateSummarizesHostHistoryAfterAdoption(t *testing.T) {
	agent, _ := startMigrateFixture(t, ProviderGrok, "adopted-source")
	// The adopted handle has no process-local turns. Jevons retained these
	// before the daemon restarted and supplies them as inert input.
	const predecessor = "user: Complete T691 and remember VIOLET67\nassistant: The live seat is still on Grok\n"
	var summarized MigrationTransferArgs
	agent.migrationSummarizer = func(_ context.Context, args MigrationTransferArgs) (MigrationTransferResult, error) {
		summarized = args
		return MigrationTransferResult{Brief: "Continue T691; VIOLET67 is retained"}, nil
	}
	var successor MigrateArgs
	agent.ops.migrate = func(_ *Agent, args *MigrateArgs) error {
		successor = *args
		return nil
	}
	if err := agent.Migrate(&MigrateArgs{
		Provider: ProviderCodex, Model: "gpt-6-sol", RetainedTranscript: predecessor,
	}); err != nil {
		t.Fatal(err)
	}
	if summarized.Transcript != strings.TrimSpace(predecessor) || summarized.Destination != ProviderCodex {
		t.Fatalf("disposable transfer input = %+v", summarized)
	}
	if successor.ContextBrief != "Continue T691; VIOLET67 is retained" || successor.RetainedTranscript != "" {
		t.Fatalf("work successor received raw history or lost brief: %+v", successor)
	}
}

func TestAgentMigrateRefusesFailedTransferBeforeMoving(t *testing.T) {
	agent, _ := startMigrateFixture(t, ProviderClaude, "failed-summary-source")
	agent.PublishEvent(Event{Type: "user", Text: "finish the violet migration"})
	agent.migrationSummarizer = func(context.Context, MigrationTransferArgs) (MigrationTransferResult, error) {
		return MigrationTransferResult{}, errors.New("summary provider unavailable")
	}
	moved := false
	agent.ops.migrate = func(*Agent, *MigrateArgs) error { moved = true; return nil }
	err := agent.Migrate(&MigrateArgs{Provider: ProviderGrok, Model: "grok-4"})
	if err == nil || !strings.Contains(err.Error(), "summary provider unavailable") || moved {
		t.Fatalf("transfer failure err=%v moved=%v", err, moved)
	}
}
