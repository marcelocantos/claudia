// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
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
	// TieBreak is an ordered provider preference on the catalog path
	// (🎯T172, widened by 🎯T203). The first listed provider whose row is
	// in the leader's dest band and within catalogTieBreakReach of its
	// pressure wins. Unlike PreferProvider it cannot cross a band, beat a
	// decided pressure gap, or reach an ineligible row. Without it, a
	// near-equal choice goes to the better-ranked row and an exact tie
	// is drawn at random — never an error.
	TieBreak []Provider
	// Rand draws the winner of an exact catalog ranking tie: it returns a
	// value in [0, n). Nil uses math/rand/v2, which is seeded from the
	// runtime. Tests pin it; it is not on the broker wire.
	Rand func(n int) int
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
	// OwnerOverride is the owner's verdict on a provider's band (🎯T948 /
	// 🎯T987 / 🎯T1013.1), keyed by Provider. A dest-band override (ok /
	// under / locked) pins that provider as the pick when it is otherwise
	// a viable catalog row (mode/quality/access match), whatever its own
	// usage reading says. A keep-off override (any non-dest band, e.g.
	// exhausted) excludes the provider structurally — the same exclusion
	// a hot/exhausted reading would produce, not a caller-side filter. A
	// caller used to pre-filter ExcludeProviders or inject an explicit
	// pick instead of asking Resolve for this; that seam let a bare start
	// bypass the override and land on an exhausted provider (2026-10-02).
	OwnerOverride map[Provider]OwnerOverride
}

// OwnerOverride is the owner's band verdict on one provider for
// OwnerOverride predicates. Band ok/under/locked is a dest-band pin;
// any other band (e.g. exhausted) is a keep-off exclusion. Reason is
// carried through to the pick's Reason / the rejection trace so a
// caller can show the owner's own words, not a re-derived summary.
type OwnerOverride struct {
	Band   PlanBand
	Reason string
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

// Resolve chooses a catalog model matching predicates. It never Start or
// SwitchModel. Known-exhausted / weekly-hot / session-low
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
	// 🎯T1013.1: an owner override is applied here, not pre-filtered by a
	// caller. A keep-off band (anything not a dest band) excludes the
	// provider the same way a hot/exhausted reading would; a dest-band
	// override (ok/under/locked) is not an exclusion — it is a pin,
	// applied after candidates are built below.
	ownerPin := map[Provider]OwnerOverride{}
	for p, ov := range pred.OwnerOverride {
		np := PlanProvider(p)
		if np == "" {
			continue
		}
		if IsDestBand(ov.Band) {
			ownerPin[np] = ov
			continue
		}
		exclude[np] = true
	}

	// rejected records, per catalog row, why it did not become a
	// candidate. Previously this loop silently continued past every
	// rejection and, on total failure, returned the bare error
	// "resolve: no catalog model matches predicates" with no trace of
	// which rows were tried or why each was dropped — the exact gap
	// that made a real ge-po resolve failure undiagnosable after the
	// fact. This trace is logged (slog) unconditionally and folded
	// into the returned error so a caller/log reader does not need a
	// separate debug build to see it.
	var candidates []catalogCand
	var rejected []string
	for _, row := range ModelCatalog() {
		id := string(row.Provider) + "/" + row.Model
		if exclude[row.Provider] {
			rejected = append(rejected, id+": excluded provider")
			continue
		}
		if pred.Mode == CapabilityTask && !row.Task {
			rejected = append(rejected, id+": mode=task not supported")
			continue
		}
		if pred.Mode == CapabilitySession && !row.Session {
			rejected = append(rejected, id+": mode=session not supported")
			continue
		}
		if pred.PreferPlan && row.Access != ModelAccessPlan {
			rejected = append(rejected, id+fmt.Sprintf(": require_plan but access=%s", row.Access))
			continue
		}
		if row.Quality != wantQ {
			rejected = append(rejected, id+fmt.Sprintf(": quality=%s wants %s", row.Quality, wantQ))
			continue
		}
		u, has := byProv[row.Provider]
		pin, pinned := ownerPin[row.Provider]
		// 🎯T1013.1: a dest-band override pins this provider's candidacy
		// whatever its own usage says — the owner has already judged it
		// is where seats belong, the same standing the readings would
		// give a naturally-ok plan.
		if !pinned {
			if skipForUsage(pred, u, has, now) {
				rejected = append(rejected, id+": usage/band ineligible (exhausted, hot, or no usage data under RequireUsage)")
				continue
			}
			// 🎯T86: the plan can have headroom while this model has none.
			if has && !ModelHasAvailableTokens(u, row.Model, now, pred.Thresholds) {
				rejected = append(rejected, id+": no available tokens on this model")
				continue
			}
		}
		band := PlanBandUnpublished
		var pressure float64
		if has {
			pressure = weeklyPressure(u, now, pred.Thresholds)
		}
		if pinned {
			band = pin.Band
		} else if has {
			band = ClassifyPlan(u, now, pred.Thresholds).Weekly
		}
		weeklyLeft, sessionLeft := unknownHeadroom, unknownHeadroom
		if has {
			weeklyLeft, sessionLeft = rowHeadroom(u, row.Model)
		}
		candidates = append(candidates, catalogCand{
			row: row, band: band, pressure: pressure,
			weeklyLeft: weeklyLeft, sessionLeft: sessionLeft,
			pinned: pinned, pinReason: pin.Reason,
		})
	}
	if len(rejected) > 0 {
		slog.Debug("resolve: catalog rejections", "rejected", rejected, "kept", len(candidates))
	}
	best, err := pickCatalog(candidates, pred.PreferProvider, pred.TieBreak, pred.Rand)
	if err != nil {
		if len(rejected) > 0 {
			return ModelPick{}, fmt.Errorf("%w (tried %d catalog rows: %s)", err, len(rejected), strings.Join(rejected, "; "))
		}
		return ModelPick{}, err
	}
	reason := fmt.Sprintf("quality=%s access=%s band=%s", best.row.Quality, best.row.Access, best.band)
	if pred.PreferProvider != "" && best.row.Provider == pred.PreferProvider {
		reason += " prefer_provider"
	}
	if best.tieBreak {
		reason += " tie_break"
	}
	if best.drawnOf > 1 {
		reason += fmt.Sprintf(" drawn_of=%d", best.drawnOf)
	}
	if best.pinned {
		reason += " owner_override"
		if r := strings.TrimSpace(best.pinReason); r != "" {
			reason += ": " + r
		}
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
	// weeklyLeft and sessionLeft are percent remaining (0–100) on this
	// row's weekly allowance — the tighter of the plan window and a
	// model-scoped window (🎯T86) — and on the session window.
	// unknownHeadroom when the provider published no number.
	weeklyLeft  float64
	sessionLeft float64
	// pinned is 🎯T1013.1: an owner dest-band override on this provider.
	// A pinned candidate wins pickCatalog outright, before the normal
	// dest-band ranking is even consulted.
	pinned    bool
	pinReason string
	// tieBreak and drawnOf record how pickCatalog decided, for the
	// pick's Reason: TieBreak moved the pick off the ranked leader, or
	// the pick was drawn at random from drawnOf exactly tied rows.
	tieBreak bool
	drawnOf  int
}

// unknownHeadroom ranks an unpublished headroom below any published
// number, including 0%.
const unknownHeadroom = -1.0

// catalogTieBreakReach is how far (in pressure, ln units) behind the
// ranked leader a TieBreak-named row may sit and still be chosen. It is
// the old 0.05 slack epsilon (🎯T172): a gap that small is noise in a
// reading the vendor rounds to whole percent, so a caller's preference
// may settle it. A wider gap is a real pacing difference and stands.
const catalogTieBreakReach = 0.05

// ErrTokenTied reported catalog candidates left undecided by band and
// pressure. Since 🎯T203 Resolve never returns it: a near tie is ranked
// by continuous pressure and headroom, and an exact tie is drawn at
// random. The type stays so callers that match it still compile.
type ErrTokenTied struct {
	IDs []string
}

func (e *ErrTokenTied) Error() string {
	return fmt.Sprintf("resolve: token-tied models %s; set TieBreak", strings.Join(e.IDs, " "))
}

// catalogRank orders two catalog candidates (🎯T203). Keys, in order:
//
//  1. Dest band rank: locked, then under, then ok (🎯T693). Spend
//     surplus that will otherwise expire before work that can wait.
//  2. Weekly pressure, lower first — the Spend Pressure Map's
//     ln(current pace / required pace), continuous. No dead band: two
//     readings 0.01 apart are ranked, not tied.
//  3. Weekly headroom (percent left on this row), more first. Mostly
//     decides when a model-scoped window is tighter than the plan's.
//  4. Session headroom, more first.
//
// It returns <0 when a ranks above b, >0 when below, 0 for an exact tie
// on every key. Unpublished rows have pressure 0 and unknown headroom,
// so a pool of them ties exactly — which is what the random draw is for.
func catalogRank(a, b catalogCand) int {
	ar, aDest := destBandRank(a.band)
	br, bDest := destBandRank(b.band)
	if !aDest {
		ar = 3
	}
	if !bDest {
		br = 3
	}
	switch {
	case ar != br:
		return ar - br
	case a.pressure < b.pressure:
		return -1
	case a.pressure > b.pressure:
		return 1
	case a.weeklyLeft > b.weeklyLeft:
		return -1
	case a.weeklyLeft < b.weeklyLeft:
		return 1
	case a.sessionLeft > b.sessionLeft:
		return -1
	case a.sessionLeft < b.sessionLeft:
		return 1
	}
	return 0
}

func catalogID(c catalogCand) string {
	return string(c.row.Provider) + "/" + c.row.Model
}

func pickCatalog(cands []catalogCand, prefer Provider, tieBreak []Provider, draw func(int) int) (catalogCand, error) {
	if len(cands) == 0 {
		return catalogCand{}, fmt.Errorf("resolve: no catalog model matches predicates")
	}
	// 🎯T1013.1: an owner dest-band override pins its provider as the
	// pick outright, ahead of the normal dest-band ranking below — the
	// owner has already judged it, Resolve does not re-adjudicate it
	// against providers it did not override. A pinned candidate's band
	// is already a dest band by construction (Resolve only pins dest-band
	// overrides), so it would qualify for the dests pool below anyway;
	// restricting the pool to pinned candidates when any exist is what
	// keeps a naturally-better-banded, un-overridden provider from
	// outranking the owner's explicit pick.
	var pinned []catalogCand
	for _, c := range cands {
		if c.pinned {
			pinned = append(pinned, c)
		}
	}
	// Dest bands first (🎯T693 / jevons T693): locked, then under, then
	// ok. hot and ahead are never destinations. If nothing dest-eligible
	// is published, PreferProvider among unpublished catalog rows — not
	// among hot/ahead.
	var dests []catalogCand
	if len(pinned) > 0 {
		dests = pinned
	} else {
		for _, c := range cands {
			if _, ok := destBandRank(c.band); ok {
				dests = append(dests, c)
			}
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
	// 🎯T203: rank, never refuse. Catalog id is only the sort's last key
	// so the ranked slice does not depend on the order candidates arrived
	// in; it never decides between rows that tie on every signal.
	pool = append([]catalogCand(nil), pool...)
	sort.SliceStable(pool, func(i, j int) bool {
		if r := catalogRank(pool[i], pool[j]); r != 0 {
			return r < 0
		}
		return catalogID(pool[i]) < catalogID(pool[j])
	})
	leader := pool[0]
	tied := 1
	for tied < len(pool) && catalogRank(pool[tied], leader) == 0 {
		tied++
	}
	_, leaderDest := destBandRank(leader.band)
	for _, p := range tieBreak {
		for _, c := range pool {
			if c.row.Provider != p {
				continue
			}
			_, cDest := destBandRank(c.band)
			sameBand := c.band == leader.band || (!cDest && !leaderDest)
			if sameBand && c.pressure <= leader.pressure+catalogTieBreakReach {
				c.tieBreak = c.row != leader.row || tied > 1
				return c, nil
			}
		}
	}
	if tied == 1 {
		return leader, nil
	}
	// The owner's rule: rows equal on every signal split the mints
	// between them at random rather than by a fixed order.
	if draw == nil {
		draw = rand.IntN
	}
	i := draw(tied)
	if i < 0 || i >= tied {
		return catalogCand{}, fmt.Errorf("resolve: tie draw %d outside [0, %d)", i, tied)
	}
	ids := make([]string, tied)
	for k := range tied {
		ids[k] = catalogID(pool[k])
	}
	slog.Info("resolve: exact catalog tie drawn at random", "tied", ids, "drawn", ids[i])
	pick := pool[i]
	pick.drawnOf = tied
	return pick, nil
}

// rowHeadroom is this row's percent left on its weekly allowance (the
// tighter of the plan window and a 🎯T86 model-scoped window) and on the
// session window. Either is unknownHeadroom when not published.
func rowHeadroom(u PlanUsage, model string) (weekly, session float64) {
	weekly, session = unknownHeadroom, unknownHeadroom
	if u.Status != PlanUsageAvailable {
		return weekly, session
	}
	left := func(w PlanWindow) (float64, bool) {
		if w.RemainingPercent != nil {
			return *w.RemainingPercent, true
		}
		if w.UsedPercent != nil {
			return 100 - *w.UsedPercent, true
		}
		return 0, false
	}
	if w, ok := primaryAllowanceWindow(u); ok {
		if v, ok := left(w); ok {
			weekly = v
		}
	}
	if w := ModelWindowFor(u, model); w != nil {
		if v, ok := left(*w); ok && (weekly == unknownHeadroom || v < weekly) {
			weekly = v
		}
	}
	if w, ok := windowNamed(u, PlanWindowSession); ok {
		if v, ok := left(w); ok {
			session = v
		}
	}
	return weekly, session
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
	// 🎯T1013.1: an owner override is applied here, not pre-filtered by a
	// caller. A keep-off band (anything not a dest band) excludes the
	// provider the same way a hot/exhausted reading would; a dest-band
	// override (ok/under/locked) is not an exclusion — it is a pin,
	// applied after candidates are built below.
	ownerPin := map[Provider]OwnerOverride{}
	for p, ov := range pred.OwnerOverride {
		np := PlanProvider(p)
		if np == "" {
			continue
		}
		if IsDestBand(ov.Band) {
			ownerPin[np] = ov
			continue
		}
		exclude[np] = true
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
