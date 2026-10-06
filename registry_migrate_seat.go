// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SeatMigration is the uniform outcome of [Registry.Migrate], whichever
// mechanical path performed the move.
type SeatMigration struct {
	// Live is true when a live handle moved in place via [Agent.Migrate];
	// false when the seat had no live process and moved via
	// [Registry.MigrateStopped].
	Live bool
	// Source is the seat's definition before the move.
	Source AgentDef
	// Destination is the seat's definition after the move.
	Destination AgentDef
	// Agent is the (possibly new) live handle for the seat, when one
	// exists after the move.
	Agent *Agent
	// Transfer is the disposable context-transfer result. Empty when the
	// move used an already-prepared ContextBrief or retained turns
	// in-process (the live Agent.Migrate path distills its own seed and
	// does not report a separate transfer result here).
	Transfer MigrationTransferResult
}

// migrateInterruptSettle is how long a forced migrate waits after
// interrupting a seat's in-flight turn before retrying.
var migrateInterruptSettle = 3 * time.Second

// migrateLive is the injectable seam behind the live branch of
// [Registry.Migrate], standing in for (*Agent).Migrate in tests that have
// no real provider backend to migrate onto.
var migrateLive = func(proc *Agent, args *MigrateArgs) error {
	return proc.Migrate(args)
}

// Migrate is the single decide-and-execute entry point for provider
// migration (🎯T1013.5). A caller no longer orchestrates separate
// prepare/thin-brief/seed/launch steps: this one call decides whether the
// seat is live or stopped, and performs the full mechanical sequence
// (context transfer, destination launch, seed hand-off) end to end via
// [Agent.Migrate] or [Registry.MigrateStopped], whichever applies.
//
// args.ContextBrief and retainedTranscript are forwarded exactly as a
// caller supplies them: an already-prepared brief skips the disposable
// transfer agent; a retained host transcript (durable history for a seat
// whose process-local turn log may be empty, e.g. after a daemon restart)
// is summarized on the destination provider before the move. A seat with
// neither, and no in-process turns to distill, refuses unless args.Force.
//
// force (args.Force) also authorizes interrupting a live in-flight turn:
// without it, a seat mid-turn refuses the move so a response in progress
// is not cut off from under its caller.
func (r *Registry) Migrate(ctx context.Context, name string, args MigrateArgs, retainedTranscript string) (SeatMigration, error) {
	if r == nil {
		return SeatMigration{}, fmt.Errorf("migrate: no registry")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return SeatMigration{}, fmt.Errorf("migrate: name is required")
	}
	target := SubscriptionSeatProvider(args.Provider)
	if target == "" {
		return SeatMigration{}, fmt.Errorf("migrate %q: destination provider is required", name)
	}
	args.Provider = target

	source := r.Def(name)
	if source == nil {
		return SeatMigration{}, fmt.Errorf("migrate %q: no such seat", name)
	}
	src := *source

	if PlanProvider(src.Provider) == PlanProvider(target) && src.MigrationSeed == "" {
		return SeatMigration{}, fmt.Errorf("migrate %q: already on %s", name, target)
	}

	// A seat with a persisted-but-undelivered destination (a prior
	// MigrateStopped attempt whose launch or hand-off failed) always
	// retries through the stopped path, whether or not a live handle
	// happens to exist for the OLD provider right now — the pending
	// destination, not the live source, is what must be retried.
	if src.MigrationSeed == "" {
		if proc := r.Get(name); proc != nil && proc.Alive() {
			return r.migrateLiveSeat(name, src, proc, args)
		}
	}

	moved, err := r.MigrateStopped(ctx, name, args, retainedTranscript)
	result := SeatMigration{
		Live: false, Source: moved.Source, Destination: moved.Destination,
		Agent: moved.Agent, Transfer: moved.Transfer,
	}
	if err != nil {
		return result, err
	}
	return result, nil
}

// migrateLiveSeat is the live branch of [Registry.Migrate]: it owns the
// turn-in-flight refusal and force-interrupt-then-retry rule that used to
// live at the jevons host layer (remapViaClaudia), so the host no longer
// decides whether to interrupt a turn before asking Claudia to move it.
func (r *Registry) migrateLiveSeat(name string, src AgentDef, proc *Agent, args MigrateArgs) (SeatMigration, error) {
	err := migrateLive(proc, &args)
	if err != nil && args.Force && isTurnInFlight(err) {
		if ierr := proc.Interrupt(); ierr != nil {
			return SeatMigration{}, fmt.Errorf("migrate %q: forced interrupt before migrate: %w", name, ierr)
		}
		time.Sleep(migrateInterruptSettle)
		err = migrateLive(proc, &args)
	}
	if err != nil {
		return SeatMigration{}, fmt.Errorf("migrate %q: %w", name, err)
	}
	dest := r.Def(name)
	if dest == nil {
		return SeatMigration{}, fmt.Errorf("migrate %q: registry row vanished after Agent.Migrate", name)
	}
	return SeatMigration{Live: true, Source: src, Destination: *dest, Agent: proc}, nil
}

func isTurnInFlight(err error) bool {
	return err != nil && strings.Contains(err.Error(), "turn in flight")
}
