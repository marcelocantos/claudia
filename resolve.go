// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"time"
)

// ModelPredicates is what a caller needs. Available-tokens is not a field
// — Resolve always applies it (🎯T61.3).
type ModelPredicates struct {
	// Mode is CapabilityTask or CapabilitySession. Empty means either.
	Mode Capability
	// Quality is frontier / standard / economy. Empty means standard.
	Quality ModelQuality
	// PreferPlan prefers subscription-harness rows over direct APIs.
	PreferPlan bool
	// PreferProvider wins among token-eligible rows when set.
	PreferProvider Provider
	// ExcludeProviders drops those backends (ladder walk).
	ExcludeProviders []Provider
	// Usage overrides the cached snapshot (hermetic tests).
	Usage []PlanUsage
	// Cache is passed to LoadPlanUsage when Usage is nil.
	Cache *PlanUsageCacheArgs
	// Now overrides the clock.
	Now time.Time
	// Thresholds overrides DefaultPlanThresholds.
	Thresholds *PlanThresholds
}

// ModelPick is one Resolve result. Resolve does not spawn.
type ModelPick struct {
	Provider Provider
	Model    string
	Quality  ModelQuality
	Access   ModelAccess
	Band     PlanBand
	Reason   string
}

// Resolve chooses a catalog model matching predicates. It never Start,
// SetModel, or Migrate. Known-exhausted / weekly-hot / session-low
// candidates are skipped automatically.
func Resolve(ctx context.Context, pred ModelPredicates) (ModelPick, error) {
	now := pred.Now
	if now.IsZero() {
		now = time.Now()
	}
	usage, err := resolveUsage(ctx, pred)
	if err != nil {
		return ModelPick{}, err
	}
	byProv := map[Provider]PlanUsage{}
	for _, u := range usage {
		byProv[u.Provider] = u
	}

	wantQ := pred.Quality
	if wantQ == "" {
		wantQ = ModelQualityStandard
	}
	exclude := map[Provider]bool{}
	for _, p := range pred.ExcludeProviders {
		exclude[p] = true
	}

	var candidates []catalogCand
	for _, row := range ModelCatalog() {
		if exclude[row.Provider] {
			continue
		}
		if pred.Mode == CapabilityTask && !row.Task {
			continue
		}
		if pred.Mode == CapabilitySession && !row.Session {
			continue
		}
		if pred.PreferPlan && row.Access != ModelAccessPlan {
			continue
		}
		u, has := byProv[row.Provider]
		if has && !HasAvailableTokens(u, now, pred.Thresholds) {
			continue
		}
		band := PlanBandUnpublished
		if has {
			band = ClassifyPlan(u, now, pred.Thresholds).Weekly
		}
		candidates = append(candidates, catalogCand{row: row, band: band})
	}
	if len(candidates) == 0 {
		return ModelPick{}, fmt.Errorf("resolve: no catalog model matches predicates")
	}

	best := candidates[0]
	bestScore := resolveScore(best.row, pred.PreferProvider, wantQ)
	for _, c := range candidates[1:] {
		if s := resolveScore(c.row, pred.PreferProvider, wantQ); s > bestScore {
			best, bestScore = c, s
		}
	}
	reason := fmt.Sprintf("quality=%s access=%s band=%s", best.row.Quality, best.row.Access, best.band)
	if pred.PreferProvider != "" && best.row.Provider == pred.PreferProvider {
		reason += " prefer_provider"
	}
	return ModelPick{
		Provider: best.row.Provider,
		Model:    best.row.Model,
		Quality:  best.row.Quality,
		Access:   best.row.Access,
		Band:     best.band,
		Reason:   reason,
	}, nil
}

type catalogCand struct {
	row  CatalogModel
	band PlanBand
}

func resolveUsage(ctx context.Context, pred ModelPredicates) ([]PlanUsage, error) {
	if pred.Usage != nil {
		return pred.Usage, nil
	}
	args := pred.Cache
	if args == nil {
		args = &PlanUsageCacheArgs{}
	}
	if args.Now.IsZero() && !pred.Now.IsZero() {
		cp := *args
		cp.Now = pred.Now
		args = &cp
	}
	return LoadPlanUsage(ctx, args)
}

func resolveScore(row CatalogModel, prefer Provider, want ModelQuality) int {
	score := 0
	if prefer != "" && row.Provider == prefer {
		score += 1000
	}
	switch {
	case row.Quality == want:
		score += 100
	case want == ModelQualityStandard && row.Quality == ModelQualityEconomy:
		score += 40
	case want == ModelQualityStandard && row.Quality == ModelQualityFrontier:
		score += 20
	case want == ModelQualityFrontier && row.Quality == ModelQualityStandard:
		score += 40
	case want == ModelQualityEconomy && row.Quality == ModelQualityStandard:
		score += 40
	}
	if row.Access == ModelAccessPlan {
		score += 5
	}
	return score
}
