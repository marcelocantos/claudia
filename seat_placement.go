// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SeatPlacementAction is Claudia's decision for one existing session.
type SeatPlacementAction string

const (
	SeatStay    SeatPlacementAction = "stay"
	SeatMigrate SeatPlacementAction = "migrate"
	SeatPark    SeatPlacementAction = "park"
	SeatDefer   SeatPlacementAction = "defer"
)

// SeatPlacementArgs supplies plan readings and host constraints for one seat.
// AllowedProviders nil permits the catalog's plan providers; a non-nil empty
// slice permits none. The preferred provider is a preference, never a ban on
// other eligible providers. Hosts express a prohibition in ExcludeProviders.
type SeatPlacementArgs struct {
	CurrentProvider  Provider
	Usage            []PlanUsage
	AllowedProviders []Provider
	ExcludeProviders []Provider
	PreferProvider   Provider
	Quality          ModelQuality
	Now              time.Time
	Thresholds       *PlanThresholds
	MaxUsageAge      time.Duration
}

// SeatPlacement is a per-agent provider decision, before any process moves.
// A host may defer a migration for an in-flight turn or another constraint;
// it must report that constraint rather than claiming Claudia chose to stay.
type SeatPlacement struct {
	Action SeatPlacementAction
	From   Provider
	Pick   ModelPick
	Reason string
	Author string
}

// ResolveSeatPlacementForSeat is [ResolveSeatPlacement] with the seat's
// AllowedProviders/ExcludeProviders/PreferProvider read directly from
// Claudia's own seat-policy store (🎯T1013.2) rather than handed in by the
// caller. args carries everything else (current provider, plan usage,
// quality, clock); any of its AllowedProviders/ExcludeProviders/
// PreferProvider fields that are already non-empty are left alone — a
// caller with a one-off override still wins — and the stored policy fills
// in only what args left unset. A nil store or empty name reads as no
// stored policy (every provider eligible, no preference), identical to
// calling ResolveSeatPlacement directly.
func ResolveSeatPlacementForSeat(ctx context.Context, name string, store *SeatPolicyStore, args *SeatPlacementArgs) (SeatPlacement, error) {
	if args == nil {
		return SeatPlacement{}, fmt.Errorf("seat placement: args is required")
	}
	merged := *args
	if store != nil && strings.TrimSpace(name) != "" {
		policy := store.Get(name)
		if merged.PreferProvider == "" {
			merged.PreferProvider = policy.PreferProvider
		}
		if merged.AllowedProviders == nil {
			if providers, restricted := policy.Allowed(); restricted {
				merged.AllowedProviders = providers
			}
		}
		if merged.ExcludeProviders == nil && len(policy.ExcludeProviders) > 0 {
			merged.ExcludeProviders = policy.ExcludeProviders
		}
	}
	return ResolveSeatPlacement(ctx, &merged)
}

// ResolveSeatPlacement applies the same classification and model ranking as
// Resolve to an existing seat. A missing or stale source reading cannot
// trigger a move; a confirmed hot source with no eligible destination parks.
// A ranking ambiguity defers rather than stopping a usable seat.
func ResolveSeatPlacement(ctx context.Context, args *SeatPlacementArgs) (SeatPlacement, error) {
	if args == nil || strings.TrimSpace(string(args.CurrentProvider)) == "" {
		return SeatPlacement{}, fmt.Errorf("seat placement: current provider is required")
	}
	now := args.Now
	if now.IsZero() {
		now = time.Now()
	}
	from := PlanProvider(args.CurrentProvider)
	decision := SeatPlacement{Action: SeatStay, From: from, Author: DecisionAuthor}
	usage := make([]PlanUsage, 0, len(args.Usage))
	var source PlanUsage
	var haveSource bool
	for _, raw := range args.Usage {
		u := raw
		u.Provider = PlanProvider(u.Provider)
		usage = append(usage, u)
		if u.Provider == from {
			if haveSource {
				return SeatPlacement{}, fmt.Errorf("seat placement: duplicate reading for %s", from)
			}
			source, haveSource = u, true
		}
	}
	if !haveSource || source.Status != PlanUsageAvailable {
		decision.Reason = "source plan reading unavailable"
		return decision, nil
	}
	if stalePlanReading(source, now, args.MaxUsageAge) {
		decision.Reason = "source plan reading stale"
		return decision, nil
	}
	if !ShouldVacate(source, now, args.Thresholds) {
		decision.Reason = "source provider has usable allowance"
		return decision, nil
	}
	pressure := "weekly hot or exhausted"
	if ClassifyPlan(source, now, args.Thresholds).Session == PlanSessionExhausted {
		pressure = "session exhausted"
	}

	allowed := map[Provider]bool{}
	if args.AllowedProviders != nil {
		for _, p := range args.AllowedProviders {
			allowed[PlanProvider(p)] = true
		}
	}
	excluded := map[Provider]bool{from: true}
	for _, p := range args.ExcludeProviders {
		excluded[PlanProvider(p)] = true
	}
	var eligible int
	seen := map[Provider]bool{}
	uncertainDest := false
	for _, u := range usage {
		p := u.Provider
		seen[p] = true
		if args.AllowedProviders != nil && !allowed[p] {
			excluded[p] = true
			continue
		}
		if excluded[p] {
			continue
		}
		if stalePlanReading(u, now, args.MaxUsageAge) {
			excluded[p] = true
			uncertainDest = true
			continue
		}
		if u.Status != PlanUsageAvailable {
			uncertainDest = true
			continue
		}
		if HasAvailableTokens(u, now, args.Thresholds) &&
			IsDestBand(ClassifyPlan(u, now, args.Thresholds).Weekly) {
			eligible++
		}
	}
	for _, row := range ModelCatalog() {
		p := row.Provider
		if row.Access != ModelAccessPlan || !row.Session || p == from || excluded[p] || seen[p] ||
			(args.AllowedProviders != nil && !allowed[p]) {
			continue
		}
		uncertainDest = true
	}
	if eligible == 0 {
		if uncertainDest {
			decision.Action = SeatDefer
			decision.Reason = pressure + "; destination plan readings incomplete or stale"
			return decision, nil
		}
		decision.Action = SeatPark
		decision.Reason = pressure + "; no eligible destination"
		return decision, nil
	}
	var exclusions []Provider
	for _, row := range ModelCatalog() {
		if excluded[row.Provider] {
			exclusions = append(exclusions, row.Provider)
		}
	}
	quality := args.Quality
	if quality == "" {
		quality = ModelQualityStandard
	}
	pick, err := Resolve(ctx, ModelPredicates{
		Mode: CapabilitySession, Quality: quality, PreferPlan: true,
		Background: true, RequireUsage: true, Usage: usage, Now: now,
		Thresholds: args.Thresholds, PreferProvider: PlanProvider(args.PreferProvider),
		ExcludeProviders: exclusions,
	})
	if err != nil {
		decision.Action = SeatDefer
		decision.Reason = pressure + "; destination unresolved: " + err.Error()
		return decision, nil
	}
	decision.Action = SeatMigrate
	decision.Pick = pick
	decision.Reason = pressure + "; " + pick.Reason
	return decision, nil
}

func stalePlanReading(u PlanUsage, now time.Time, maxAge time.Duration) bool {
	if maxAge <= 0 {
		return false
	}
	return u.FetchedAt.IsZero() || now.Sub(u.FetchedAt) > maxAge
}
