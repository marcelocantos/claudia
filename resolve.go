// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"sort"
	"strings"
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
	// Skill is a client alias for Purpose (`skill=analysis`). Purpose
	// wins when both are set.
	Skill ModelPurpose
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
	// Background avoids providers spending ahead of pace (orange or red).
	// Unpublished usage remains eligible unless RequireUsage is also set.
	// The zero value preserves interactive selection behavior.
	Background bool
	// PreferProvider wins among token-eligible rows when set. Eligible
	// means HasAvailableTokens, not "tied on slack": a preferred Claude
	// with headroom beats a greener Grok. Hot/exhausted preferred dests
	// still yield.
	PreferProvider Provider
	// ExcludeProviders drops those backends (ladder walk).
	ExcludeProviders []Provider
	// RequireUsage drops catalog rows with no snapshot or an unpublished
	// band so a host that must refuse rather than land on an unknown dest
	// can fail closed. Off by default: unpublished is not a veto.
	RequireUsage bool
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

// DecisionAuthor is the name Resolve stamps on a pick. A host that
// re-adjudicates is a different decision and must not reuse this.
const DecisionAuthor = "claudia"

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
	// Author is always "claudia". Resolve authors its own pick; a host
	// that re-adjudicates is a different decision and must not reuse this.
	Author string
}

// Resolve chooses a catalog model matching predicates. It never Start,
// SetModel, or Migrate. Known-exhausted / weekly-hot / session-low
// candidates are skipped automatically.
func Resolve(ctx context.Context, pred ModelPredicates) (ModelPick, error) {
	pred = normalizePredicates(pred)
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
		if skipForUsage(pred, u, has, now) {
			continue
		}
		// 🎯T86: the plan can have headroom while this model has none.
		if has && !ModelHasAvailableTokens(u, row.Model, now, pred.Thresholds) {
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
	best, err := pickCatalog(candidates, pred.PreferProvider)
	if err != nil {
		return ModelPick{}, err
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
		Author:   DecisionAuthor,
	}, nil
}

type catalogCand struct {
	row      CatalogModel
	band     PlanBand
	pressure float64
}

func pickCatalog(cands []catalogCand, prefer Provider) (catalogCand, error) {
	if len(cands) == 0 {
		return catalogCand{}, fmt.Errorf("resolve: no catalog model matches predicates")
	}
	// Dest bands first (🎯T693 / jevons T693): locked, then under, then
	// ok. hot and ahead are never destinations. If nothing dest-eligible
	// is published, PreferProvider among unpublished catalog rows — not
	// among hot/ahead.
	var dests []catalogCand
	for _, c := range cands {
		if _, ok := destBandRank(c.band); ok {
			dests = append(dests, c)
		}
	}
	pool := dests
	if len(pool) == 0 {
		var unpublished []catalogCand
		for _, c := range cands {
			if !publishedPlanBand(c.band) {
				unpublished = append(unpublished, c)
			}
		}
		if len(unpublished) == 0 {
			return catalogCand{}, fmt.Errorf("resolve: no catalog model matches predicates")
		}
		pool = unpublished
	}
	if prefer != "" {
		var pref []catalogCand
		for _, c := range pool {
			if c.row.Provider == prefer {
				pref = append(pref, c)
			}
		}
		if len(pref) > 0 {
			pool = pref
		}
	}
	best := pool[0]
	for _, c := range pool[1:] {
		if better, ok := destBetter(c.band, c.pressure, best.band, best.pressure); ok && better {
			best = c
		}
	}
	var slack []catalogCand
	for _, c := range pool {
		if _, decided := destBetter(c.band, c.pressure, best.band, best.pressure); !decided {
			slack = append(slack, c)
		}
	}
	if len(slack) == 1 {
		return slack[0], nil
	}
	ids := make([]string, len(slack))
	for i, c := range slack {
		ids[i] = string(c.row.Provider) + "/" + c.row.Model
	}
	sort.Strings(ids)
	return catalogCand{}, fmt.Errorf("resolve: token-tied models %s; set PreferProvider", strings.Join(ids, " "))
}

// IsDestBand reports a published band that may receive new work.
// locked / under / ok yes; ahead / hot / exhausted / unpublished no.
func IsDestBand(b PlanBand) bool {
	_, ok := destBandRank(b)
	return ok
}

// destBandRank is destination order: locked (surplus locked in), then
// under (paid allowance at risk of expiring), then ok. hot and ahead
// are never destinations.
func destBandRank(b PlanBand) (int, bool) {
	switch b {
	case PlanBandLocked:
		return 0, true
	case PlanBandUnder:
		return 1, true
	case PlanBandOK:
		return 2, true
	default:
		return 0, false
	}
}

func destBetter(cBand PlanBand, cPress float64, bestBand PlanBand, bestPress float64) (cBetter bool, decided bool) {
	cr, cOK := destBandRank(cBand)
	br, bOK := destBandRank(bestBand)
	if cOK != bOK {
		return cOK, true
	}
	if !cOK {
		return false, false
	}
	if cr != br {
		return cr < br, true
	}
	return slackDecides(cPress, bestPress)
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

func slackDecidesPublished(cPress float64, cBand PlanBand, bestPress float64, bestBand PlanBand) (cBetter bool, decided bool) {
	cPub := publishedPlanBand(cBand)
	bPub := publishedPlanBand(bestBand)
	if cPub != bPub {
		return cPub, true
	}
	if !cPub {
		return false, false
	}
	return destBetter(cBand, cPress, bestBand, bestPress)
}

func publishedPlanBand(b PlanBand) bool {
	return b != "" && b != PlanBandUnpublished
}

func normalizePredicates(pred ModelPredicates) ModelPredicates {
	if pred.Purpose == "" && pred.Skill != "" {
		pred.Purpose = pred.Skill
	}
	return pred
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
	if requested != ModelPurposeGeneral && !purposeHasCatalogSeries(obs, requested) {
		// No series for the named skill (AA often omits math/HLE).
		// Interpret as general — even when general is empty too, so
		// the catalog shelf is scored as general, not a phantom analysis.
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
	// AA does not score every spawnable row (Cursor composer is the
	// usual gap). Shelf-matching catalog rows stay in the set so
	// token slack can pick them. Catalog declaration order is not a rank.
	pool = appendCatalogShelf(pool, wantQ)

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
		if skipForUsage(pred, u, has, now) {
			continue
		}
		// 🎯T86: same rule on the intel path — a spent model is
		// ineligible even when its provider's plan is fine.
		if has && !ModelHasAvailableTokens(u, c.row.Model, now, pred.Thresholds) {
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
		if !c.hasScore && c.row.Quality != wantQ {
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
	if pred.PreferProvider != "" {
		var pref []intelCand
		for _, c := range kept {
			if c.row.Provider == pred.PreferProvider {
				pref = append(pref, c)
			}
		}
		if len(pref) > 0 {
			kept = pref
		}
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
		Author:   DecisionAuthor,
	}, nil
}

func skipForUsage(pred ModelPredicates, u PlanUsage, has bool, now time.Time) bool {
	if pred.Background && has {
		switch ClassifyPlan(u, now, pred.Thresholds).Weekly {
		case PlanBandAhead, PlanBandHot, PlanBandExhausted:
			return true
		}
	}
	if !pred.RequireUsage {
		return false
	}
	if !has {
		return true
	}
	if u.Status != PlanUsageAvailable {
		return true
	}
	return !publishedPlanBand(ClassifyPlan(u, now, pred.Thresholds).Weekly)
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

func appendCatalogShelf(pool []intelCand, wantQ ModelQuality) []intelCand {
	seen := map[string]bool{}
	for _, c := range pool {
		seen[string(c.row.Provider)+"/"+c.row.Model] = true
	}
	for _, row := range ModelCatalog() {
		if row.Quality != wantQ {
			continue
		}
		id := string(row.Provider) + "/" + row.Model
		if seen[id] {
			continue
		}
		seen[id] = true
		pool = append(pool, intelCand{row: row, effort: ModelEffortUnspecified})
	}
	return pool
}

func intelBetter(c, best intelCand, prefer Provider) bool {
	// Dest band first (locked/under/ok), then lower pressure, then cost.
	// A published dest band beats unpublished: pressure 0 is "unknown", not blue.
	if better, ok := slackDecidesPublished(c.pressure, c.band, best.pressure, best.band); ok {
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
