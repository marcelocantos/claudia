// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0
package claudia

import (
	"fmt"
	"math"
	"time"
)

// CommitBrokerUsageFixture is called only by the isolated test control socket.
// It writes through the broker-owned control-file publisher, never a consumer replica.
// A batch is one transaction so a consumer cannot see a partially updated phase.
func CommitBrokerUsageFixture(readings []PlanUsage) error {
	if len(readings) == 0 {
		return fmt.Errorf("usage fixture: empty generation")
	}
	store, err := currentPlanUsageStore()
	if err != nil {
		return err
	}
	batch, ok := store.(interface {
		CommitFixturePlanUsage([]PlanUsageObservation) error
	})
	if !ok {
		return fmt.Errorf("usage fixture: publisher does not support atomic generations")
	}
	now := time.Now().UTC()
	rows := make([]PlanUsageObservation, 0, len(readings))
	seen := map[Provider]bool{}
	for _, u := range readings {
		switch u.Provider {
		case ProviderClaude, ProviderCodex, ProviderGrok, ProviderCursor, ProviderBedrock:
		default:
			return fmt.Errorf("usage fixture: unknown provider %q", u.Provider)
		}
		if seen[u.Provider] {
			return fmt.Errorf("usage fixture: duplicate provider %q", u.Provider)
		}
		seen[u.Provider] = true
		if u.Status != PlanUsageAvailable && u.Status != PlanUsageUnavailable {
			return fmt.Errorf("usage fixture: invalid status %q", u.Status)
		}
		if u.Status == PlanUsageAvailable && len(u.Windows) == 0 {
			return fmt.Errorf("usage fixture: available requires windows")
		}
		if u.Status == PlanUsageUnavailable && len(u.Windows) != 0 {
			return fmt.Errorf("usage fixture: unavailable cannot publish windows")
		}
		if u.HTTPStatus == 429 && u.Status != PlanUsageUnavailable {
			return fmt.Errorf("usage fixture: 429 is unpublished")
		}
		for i := range u.Windows {
			w := &u.Windows[i]
			if w.Name == "" || (w.UsedPercent == nil && w.RemainingPercent == nil) {
				return fmt.Errorf("usage fixture: window lacks name or percent")
			}
			for _, pct := range []*float64{w.UsedPercent, w.RemainingPercent} {
				if pct != nil && (math.IsNaN(*pct) || math.IsInf(*pct, 0) || *pct < 0 || *pct > 100) {
					return fmt.Errorf("usage fixture: invalid percentage")
				}
			}
			w.Band = ClassifyWindow(*w, now, DefaultPlanThresholds())
		}
		u.FetchedAt, u.RawBody = now, ""
		row := PlanUsageObservation{Provider: u.Provider, Status: u.Status, Reason: u.Reason, AttemptedAt: now, NextAttemptAt: now.Add(PlanThrottleMinInterval)}
		if u.Status == PlanUsageAvailable {
			row.Reading = &u
			row.SuccessAt = now
		}
		rows = append(rows, row)
	}
	return batch.CommitFixturePlanUsage(rows)
}
