// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// ModelPredicates is what a caller needs. Available-tokens is not a field
// — Resolve always applies it (🎯T61.3).
type ModelPredicates struct {
	// Mode is CapabilityTask or CapabilitySession. Empty means either.
	Mode Capability
	// Purpose selects which quality series to use (coding, general, …).
	// Empty keeps the catalog-shelf path. Set a purpose to pick from
	// intel: generation and effort become outputs (🎯T71). A purpose
	// with no catalog-overlapping observations yields to general.
	Purpose ModelPurpose
	// Quality is the band: frontier / standard / economy.
	// Empty means standard. On the catalog path this is a generation
	// shelf. With Purpose set it is a floor on that purpose's scores.
	Quality ModelQuality
	// Model pins a catalog generation. Empty means Resolve chooses.
	Model string
	// Effort pins a think setting. Empty means Resolve chooses.
	Effort ModelEffort
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
	// Intel is the purpose-quality series (tests / explicit dir).
	Intel *ModelIntelArgs
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
	Purpose  ModelPurpose
	Effort   ModelEffort
	Access   ModelAccess
	Band     PlanBand
	CostUSD  float64
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

	if pred.Purpose != "" || pred.Model != "" || pred.Effort != "" || pred.Intel != nil {
		return resolveFromIntel(pred, byProv, now)
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
		if row.Quality != wantQ {
			continue
		}
		u, has := byProv[row.Provider]
		if has && !HasAvailableTokens(u, now, pred.Thresholds) {
			continue
		}
		band := PlanBandUnpublished
		var pressure float64
		if has {
			band = ClassifyPlan(u, now, pred.Thresholds).Weekly
			pressure = weeklyPressure(u, now, pred.Thresholds)
		}
		candidates = append(candidates, catalogCand{row: row, band: band, pressure: pressure})
	}
	if len(candidates) == 0 {
		return ModelPick{}, fmt.Errorf("resolve: no catalog model matches predicates")
	}

	best := candidates[0]
	for _, c := range candidates[1:] {
		if catalogBetter(c, best, pred.PreferProvider, wantQ) {
			best = c
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
	row      CatalogModel
	band     PlanBand
	pressure float64
}

func catalogBetter(c, best catalogCand, prefer Provider, wantQ ModelQuality) bool {
	if better, ok := slackDecides(c.pressure, best.pressure); ok {
		return better
	}
	return resolveScore(c.row, prefer, wantQ) > resolveScore(best.row, prefer, wantQ)
}

func slackDecides(c, best float64) (cBetter bool, decided bool) {
	const slackEps = 0.05
	if c < best-slackEps {
		return true, true
	}
	if c > best+slackEps {
		return false, true
	}
	return false, false
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

type intelCand struct {
	row      CatalogModel
	effort   ModelEffort
	value    float64
	cost     float64
	hasScore bool
	band     PlanBand
	pressure float64
}

func resolveFromIntel(pred ModelPredicates, byProv map[Provider]PlanUsage, now time.Time) (ModelPick, error) {
	obs, err := LatestModelIntel(pred.Intel)
	if err != nil {
		return ModelPick{}, err
	}
	requested := pred.Purpose
	if requested == "" {
		requested = ModelPurposeGeneral
	}
	purpose := requested
	var fallbackFrom ModelPurpose
	if requested != ModelPurposeGeneral && !purposeHasCatalogSeries(obs, requested) && purposeHasCatalogSeries(obs, ModelPurposeGeneral) {
		purpose = ModelPurposeGeneral
		fallbackFrom = requested
	}
	wantQ := pred.Quality
	if wantQ == "" {
		wantQ = ModelQualityStandard
	}
	pinRow, hasPin := CatalogModel{}, false
	if pred.Model != "" {
		pinRow, hasPin = MatchCatalogGeneration(pred.Model)
		if !hasPin {
			return ModelPick{}, fmt.Errorf("resolve: pinned model %q is not in the catalog", pred.Model)
		}
	}

	var pool []intelCand
	seen := map[string]bool{}
	for _, o := range obs {
		if o.Purpose != purpose {
			continue
		}
		row, ok := MatchCatalogGeneration(o.Generation)
		if !ok {
			continue
		}
		id := string(row.Provider) + "/" + row.Model + "/" + string(o.Effort)
		if seen[id] {
			continue
		}
		seen[id] = true
		pool = append(pool, intelCand{row: row, effort: o.Effort, value: o.Value, cost: o.CostUSD, hasScore: true})
	}
	if hasPin {
		found := false
		for _, c := range pool {
			if c.row.Model == pinRow.Model && (pred.Effort == "" || c.effort == pred.Effort) {
				found = true
				break
			}
		}
		if !found {
			pool = append(pool, intelCand{row: pinRow, effort: pred.Effort})
		}
	}

	exclude := map[Provider]bool{}
	for _, p := range pred.ExcludeProviders {
		exclude[p] = true
	}
	var scores []float64
	for _, o := range obs {
		if o.Purpose != purpose {
			continue
		}
		if _, ok := MatchCatalogGeneration(o.Generation); ok {
			scores = append(scores, o.Value)
		}
	}
	var eligible []intelCand
	for _, c := range pool {
		if exclude[c.row.Provider] {
			continue
		}
		if pred.Mode == CapabilityTask && !c.row.Task {
			continue
		}
		if pred.Mode == CapabilitySession && !c.row.Session {
			continue
		}
		if pred.PreferPlan && c.row.Access != ModelAccessPlan {
			continue
		}
		if hasPin && c.row.Model != pinRow.Model {
			continue
		}
		if pred.Effort != "" && c.effort != pred.Effort {
			continue
		}
		u, has := byProv[c.row.Provider]
		if has && !HasAvailableTokens(u, now, pred.Thresholds) {
			continue
		}
		c.band = PlanBandUnpublished
		if has {
			c.band = ClassifyPlan(u, now, pred.Thresholds).Weekly
			c.pressure = weeklyPressure(u, now, pred.Thresholds)
		}
		eligible = append(eligible, c)
	}
	if len(eligible) == 0 {
		return ModelPick{}, fmt.Errorf("resolve: no catalog model matches predicates")
	}

	pinnedOnly := pred.Model != "" && pred.Effort != "" && pred.Purpose == "" && pred.Quality == ""
	var floor float64
	var haveFloor bool
	if !pinnedOnly {
		floor, haveFloor = qualityScoreFloor(scores, wantQ)
	}
	var kept []intelCand
	for _, c := range eligible {
		if haveFloor && c.hasScore && c.value < floor {
			continue
		}
		if haveFloor && !c.hasScore && pred.Purpose != "" {
			continue
		}
		kept = append(kept, c)
	}
	if pred.Model != "" && pred.Effort != "" && pred.Purpose != "" && len(kept) == 0 {
		return ModelPick{}, fmt.Errorf("resolve: pinned %s/%s misses %s %s floor", pinRow.Model, pred.Effort, purpose, wantQ)
	}
	if len(kept) == 0 {
		return ModelPick{}, fmt.Errorf("resolve: no catalog model meets %s quality=%s", purpose, wantQ)
	}

	best := kept[0]
	for _, c := range kept[1:] {
		if intelBetter(c, best, pred.PreferProvider) {
			best = c
		}
	}
	reason := formatIntelReason(purpose, wantQ, best.effort, best.cost, best.band)
	if fallbackFrom != "" {
		reason += " purpose_fallback_from=" + string(fallbackFrom)
	}
	if pred.PreferProvider != "" && best.row.Provider == pred.PreferProvider {
		reason += " prefer_provider"
	}
	if pred.Model != "" {
		reason += " pin_model"
	}
	if pred.Effort != "" {
		reason += " pin_effort"
	}
	return ModelPick{
		Provider: best.row.Provider,
		Model:    best.row.Model,
		Quality:  wantQ,
		Purpose:  purpose,
		Effort:   best.effort,
		Access:   best.row.Access,
		Band:     best.band,
		CostUSD:  best.cost,
		Reason:   reason,
	}, nil
}

func purposeHasCatalogSeries(obs []ModelObservation, purpose ModelPurpose) bool {
	for _, o := range obs {
		if o.Purpose != purpose {
			continue
		}
		if _, ok := MatchCatalogGeneration(o.Generation); ok {
			return true
		}
	}
	return false
}

func intelBetter(c, best intelCand, prefer Provider) bool {
	// Lower pressure (blue/purple slack) wins before research cost.
	if better, ok := slackDecides(c.pressure, best.pressure); ok {
		return better
	}
	cc, bc := costOrInf(c.cost, c.hasScore), costOrInf(best.cost, best.hasScore)
	if cc != bc {
		return cc < bc
	}
	if prefer != "" {
		if c.row.Provider == prefer && best.row.Provider != prefer {
			return true
		}
		if best.row.Provider == prefer && c.row.Provider != prefer {
			return false
		}
	}
	if c.hasScore != best.hasScore {
		return c.hasScore
	}
	return c.value > best.value
}

func costOrInf(cost float64, hasScore bool) float64 {
	if cost > 0 {
		return cost
	}
	if hasScore {
		return 1e9
	}
	return 1e12
}

func qualityScoreFloor(scores []float64, q ModelQuality) (float64, bool) {
	if len(scores) == 0 {
		return 0, false
	}
	cp := append([]float64(nil), scores...)
	sort.Float64s(cp)
	switch q {
	case ModelQualityEconomy:
		return cp[0], true
	case ModelQualityFrontier:
		return percentileSorted(cp, 0.75), true
	default:
		return percentileSorted(cp, 0.40), true
	}
}

func percentileSorted(sorted []float64, p float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := p * float64(len(sorted)-1)
	lo := int(idx)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[lo]
	}
	frac := idx - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

func weeklyPressure(u PlanUsage, now time.Time, th *PlanThresholds) float64 {
	thresholds := DefaultPlanThresholds()
	if th != nil {
		thresholds = th.withDefaults()
	}
	w, ok := primaryAllowanceWindow(u)
	if !ok {
		return 0
	}
	used := usedPercent(w)
	rtp, hasTime := remainingTimePercent(w, now)
	if !hasTime || used == nil {
		return 0
	}
	return Pressure(*used, 100-rtp, thresholds)
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
