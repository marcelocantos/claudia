// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// StoppedMigration is the durable outcome of migrating a seat whose host
// has no live handle. The registry owns both the destination and its seed.
type StoppedMigration struct {
	Source      AgentDef
	Destination AgentDef
	Agent       *Agent
	Transfer    MigrationTransferResult
}

// MigrateStopped runs the same two-step transfer as Agent.Migrate for a seat
// that may no longer have a live handle. An adopt-only probe first recovers a
// broker-held predecessor if one exists; otherwise the registry persists a
// fresh destination with its handover before launching it. A launch failure
// leaves that intent recoverable by repeating this call with the same target.
// Transcript is inert normalized history supplied by the host.
func (r *Registry) MigrateStopped(ctx context.Context, name string, args MigrateArgs, transcript string) (StoppedMigration, error) {
	if r == nil || strings.TrimSpace(name) == "" {
		return StoppedMigration{}, fmt.Errorf("migrate stopped: registry and name are required")
	}
	target := SubscriptionSeatProvider(args.Provider)
	if target == "" {
		return StoppedMigration{}, fmt.Errorf("migrate stopped %q: destination provider is required", name)
	}
	if strings.TrimSpace(args.ContextBrief) != "" {
		return StoppedMigration{}, fmt.Errorf("migrate stopped %q: pass inert transcript, not a host-authored handover", name)
	}
	ctx, op, finish, err := r.beginLifecycle(ctx, name, false)
	if err != nil {
		return StoppedMigration{}, err
	}
	defer finish()
	r.mu.Lock()
	def := r.agents[name]
	if def == nil {
		r.mu.Unlock()
		return StoppedMigration{}, fmt.Errorf("migrate stopped: no agent %q", name)
	}
	source := cloneAgentDef(*def)
	if live := r.procs[name]; live != nil && live.Alive() && source.MigrationSeed == "" {
		r.mu.Unlock()
		return StoppedMigration{}, fmt.Errorf("migrate stopped %q: live handle exists; use Agent.Migrate", name)
	}
	summarize := r.migrationSummarizer
	r.mu.Unlock()
	if PlanProvider(source.Provider) == PlanProvider(target) {
		if source.MigrationSeed == "" {
			return StoppedMigration{}, fmt.Errorf("migrate stopped %q: already on %s", name, target)
		}
		proc, err := r.startHeld(ctx, op, name, !source.MigrationPendingStart, true)
		if err != nil {
			return StoppedMigration{Source: source, Destination: source}, fmt.Errorf("migrate stopped %q: retry pending destination: %w", name, err)
		}
		if source.MigrationFrom != "" {
			source.Provider = source.MigrationFrom
			source.SessionID = source.MigrationFromSession
		}
		return StoppedMigration{Source: source, Destination: *r.Def(name), Agent: proc}, nil
	}
	// Check broker state before paying for transfer. A prior migration may
	// have committed in the daemon while this consumer kept a stale row.
	proc, adoptErr := r.startHeld(ctx, op, name, true, false)
	if adoptErr == nil && PlanProvider(proc.Provider()) == PlanProvider(target) {
		return StoppedMigration{Source: source, Destination: *r.Def(name), Agent: proc}, nil
	}
	if adoptErr != nil && !errors.Is(adoptErr, ErrNoSessionWindow) &&
		!strings.Contains(adoptErr.Error(), ErrNoSessionWindow.Error()) {
		return StoppedMigration{}, fmt.Errorf("migrate stopped %q: predecessor adoption refused: %w", name, adoptErr)
	}
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		if !args.Force {
			return StoppedMigration{}, fmt.Errorf("migrate stopped %q: no predecessor context to transfer", name)
		}
		transcript = "system: forced cold start; no predecessor turns were retained"
	}
	if summarize == nil {
		summarize = SummarizeForMigration
	}
	transfer, err := summarize(ctx, MigrationTransferArgs{
		Destination: target, Goal: source.Goal, Transcript: transcript,
	})
	if err != nil {
		return StoppedMigration{}, fmt.Errorf("migrate stopped %q: context transfer: %w", name, err)
	}
	if strings.TrimSpace(transfer.Brief) == "" {
		return StoppedMigration{}, fmt.Errorf("migrate stopped %q: context transfer returned an empty brief", name)
	}
	destinationModel := strings.TrimSpace(args.Model)
	if destinationModel == "" && useOMP(Config{Provider: target}) {
		// The sidecar requires a concrete model id. The transfer seat already
		// selected one on this provider; a fixture or alternate summarizer may
		// omit it, in which case resolve the same standard tier explicitly.
		destinationModel = strings.TrimSpace(transfer.Model)
		if destinationModel == "" {
			destinationModel, err = migrationSummaryModel(ctx, PlanProvider(target))
			if err != nil {
				return StoppedMigration{}, fmt.Errorf("migrate stopped %q: destination model: %w", name, err)
			}
		}
	}
	prepared := args
	prepared.Provider = target
	prepared.Model = destinationModel
	prepared.ContextBrief = clipRunes(strings.TrimSpace(transfer.Brief), maxBriefRunes)
	if adoptErr == nil {
		if err := proc.Migrate(&prepared); err != nil {
			return StoppedMigration{}, fmt.Errorf("migrate stopped %q: adopted predecessor migration: %w", name, err)
		}
		return StoppedMigration{Source: source, Destination: *r.Def(name), Agent: proc, Transfer: transfer}, nil
	}

	r.mu.Lock()
	def = r.agents[name]
	if def == nil || PlanProvider(def.Provider) != PlanProvider(source.Provider) || def.SessionID != source.SessionID || op.stops > 0 {
		r.mu.Unlock()
		return StoppedMigration{}, fmt.Errorf("migrate stopped %q: source changed during context transfer", name)
	}
	next := cloneAgentDef(*def)
	next.Provider = target
	next.Model = destinationModel
	next.SessionID = uuid.NewString()
	next.Materialized = false
	next.ConnectURL, next.ConnectPID = "", 0
	next.MigrationSeed = migrateSeedHeader + "\n\n" + prepared.ContextBrief
	next.MigrationFrom = source.Provider
	next.MigrationFromSession = source.SessionID
	next.MigrationPendingStart = true
	priorFresh, hadFresh := r.freshSession[name]
	r.agents[name] = &next
	r.freshSession[name] = next.SessionID
	delete(r.resumeDenied, name)
	if err := r.save(); err != nil {
		r.agents[name] = def
		if hadFresh {
			r.freshSession[name] = priorFresh
		} else {
			delete(r.freshSession, name)
		}
		r.mu.Unlock()
		return StoppedMigration{}, fmt.Errorf("migrate stopped %q: persist destination and handover: %w", name, err)
	}
	r.mu.Unlock()
	result := StoppedMigration{Source: source, Destination: next, Transfer: transfer}
	proc, err = r.startHeld(ctx, op, name, false, false)
	if err != nil {
		return result, fmt.Errorf("migrate stopped %q: destination persisted but launch failed: %w", name, err)
	}
	result.Agent = proc
	result.Destination = *r.Def(name)
	return result, nil
}
