// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// RefreshModelIntel fetches published scores and appends them (🎯T71).
// A missing AA key records a failed run and returns an error; it does
// not invent observations. Context is reserved for the HTTP path.
func RefreshModelIntel(ctx context.Context, args *ModelIntelArgs) (ModelIntelRun, error) {
	dir, err := intelDir(args)
	if err != nil {
		return ModelIntelRun{}, err
	}
	now := intelNow(args)
	run := ModelIntelRun{StartedAt: now, Source: modelIntelSourceAA}
	raw, err := fetchAAModels(ctx, args)
	if err != nil {
		run.FinishedAt = intelNow(args)
		run.Error = err.Error()
		_ = appendIntelRun(dir, run)
		return run, err
	}
	obs, err := observationsFromAA(raw, now)
	if err != nil {
		run.FinishedAt = intelNow(args)
		run.Error = err.Error()
		_ = appendIntelRun(dir, run)
		return run, err
	}
	if err := appendObservations(dir, obs); err != nil {
		run.FinishedAt = intelNow(args)
		run.Error = err.Error()
		_ = appendIntelRun(dir, run)
		return run, err
	}
	run.OK = true
	run.Count = len(obs)
	run.FinishedAt = intelNow(args)
	if err := appendIntelRun(dir, run); err != nil {
		return run, err
	}
	return run, nil
}

// LatestModelIntel is the newest observation per
// (generation, effort, purpose, source).
func LatestModelIntel(args *ModelIntelArgs) ([]ModelObservation, error) {
	obs, err := loadIntelObservations(args)
	if err != nil {
		return nil, err
	}
	type key struct {
		g, e, p, s string
	}
	best := map[key]ModelObservation{}
	for _, o := range obs {
		k := key{o.Generation, string(o.Effort), string(o.Purpose), o.Source}
		if prev, ok := best[k]; !ok || o.ObservedAt.After(prev.ObservedAt) {
			best[k] = o
		}
	}
	out := make([]ModelObservation, 0, len(best))
	for _, o := range best {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Purpose != out[j].Purpose {
			return out[i].Purpose < out[j].Purpose
		}
		if out[i].Generation != out[j].Generation {
			return out[i].Generation < out[j].Generation
		}
		return out[i].Effort < out[j].Effort
	})
	return out, nil
}

// ModelIntelHistory is every observation for one identity, oldest first.
func ModelIntelHistory(args *ModelIntelArgs, generation string, effort ModelEffort, purpose ModelPurpose) ([]ModelObservation, error) {
	obs, err := loadIntelObservations(args)
	if err != nil {
		return nil, err
	}
	var out []ModelObservation
	for _, o := range obs {
		if o.Generation == generation && o.Effort == effort && (purpose == "" || o.Purpose == purpose) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ObservedAt.Before(out[j].ObservedAt) })
	return out, nil
}

// ModelIntelDrift is one identity's move between the last two readings.
type ModelIntelDrift struct {
	Generation  string       `json:"generation"`
	Effort      ModelEffort  `json:"effort,omitempty"`
	Purpose     ModelPurpose `json:"purpose"`
	Source      string       `json:"source"`
	From        float64      `json:"from"`
	To          float64      `json:"to"`
	Delta       float64      `json:"delta"`
	BoardEvent  bool         `json:"board_event,omitempty"`
	Significant bool         `json:"significant"`
	FromRev     string       `json:"from_rev,omitempty"`
	ToRev       string       `json:"to_rev,omitempty"`
}

// DriftModelIntel compares the last two observations per identity.
func DriftModelIntel(args *ModelIntelArgs) ([]ModelIntelDrift, error) {
	obs, err := loadIntelObservations(args)
	if err != nil {
		return nil, err
	}
	type key struct {
		g, e, p, s string
	}
	by := map[key][]ModelObservation{}
	for _, o := range obs {
		k := key{o.Generation, string(o.Effort), string(o.Purpose), o.Source}
		by[k] = append(by[k], o)
	}
	var out []ModelIntelDrift
	for _, series := range by {
		sort.Slice(series, func(i, j int) bool { return series[i].ObservedAt.Before(series[j].ObservedAt) })
		if len(series) < 2 {
			continue
		}
		a, b := series[len(series)-2], series[len(series)-1]
		d := ModelIntelDrift{
			Generation: a.Generation,
			Effort:     a.Effort,
			Purpose:    a.Purpose,
			Source:     a.Source,
			From:       a.Value,
			To:         b.Value,
			Delta:      b.Value - a.Value,
			FromRev:    a.SourceRev,
			ToRev:      b.SourceRev,
			BoardEvent: a.SourceRev != "" && b.SourceRev != "" && a.SourceRev != b.SourceRev,
		}
		if !d.BoardEvent && absFloat(d.Delta) >= modelIntelDriftIndexPoints {
			d.Significant = true
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		ai, aj := absFloat(out[i].Delta), absFloat(out[j].Delta)
		if ai != aj {
			return ai > aj
		}
		return out[i].Generation < out[j].Generation
	})
	return out, nil
}

func loadIntelObservations(args *ModelIntelArgs) ([]ModelObservation, error) {
	if args != nil && args.Latest != nil {
		return args.Latest, nil
	}
	dir, err := intelDir(args)
	if err != nil {
		return nil, err
	}
	return readObservations(dir)
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func formatIntelReason(purpose ModelPurpose, quality ModelQuality, effort ModelEffort, cost float64, band PlanBand) string {
	var b strings.Builder
	fmt.Fprintf(&b, "purpose=%s quality=%s effort=%s band=%s", purpose, quality, effortOrUnspecified(effort), band)
	if cost > 0 {
		fmt.Fprintf(&b, " cost=%.4f", cost)
	}
	return b.String()
}

func effortOrUnspecified(e ModelEffort) string {
	if e == ModelEffortUnspecified {
		return "unspecified"
	}
	return string(e)
}
