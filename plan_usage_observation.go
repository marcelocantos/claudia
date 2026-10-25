// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

// PlanUsageObservation separates the latest polling outcome from the last
// successful measurement. Only the broker's control-file store implements it.
type PlanUsageObservation struct {
	Provider                              Provider
	Status                                PlanUsageStatus
	Reason                                string
	AttemptedAt, SuccessAt, NextAttemptAt time.Time
	Refusals                              int
	Reading                               *PlanUsage
}

// PlanUsageStore serializes reservations and atomically commits outcome,
// successful reading, history and retry timing in the broker's existing file.
type PlanUsageStore interface {
	ReadPlanUsage() ([]PlanUsageObservation, error)
	ReservePlanUsage(Provider, time.Time, time.Time) (PlanUsageObservation, bool, error)
	CompletePlanUsage(PlanUsageObservation) error
}

var controlPlanStore struct {
	sync.Mutex
	store PlanUsageStore
}

// SetControlPlanUsageStore binds the broker-owned control publisher. Consumers
// read their replica instead; a nil store disables polling, never falls back.
func SetControlPlanUsageStore(store PlanUsageStore) {
	controlPlanStore.Lock()
	defer controlPlanStore.Unlock()
	controlPlanStore.store = store
}

func currentPlanUsageStore() (PlanUsageStore, error) {
	controlPlanStore.Lock()
	defer controlPlanStore.Unlock()
	if controlPlanStore.store == nil {
		return nil, fmt.Errorf("control plane: plan usage store not linked")
	}
	return controlPlanStore.store, nil
}

// ReadBrokerPlanUsage reads canonical outcomes without performing a request.
// Failed outcomes deliberately have no windows, even when the DB retains a
// last-good reading for display: routing must not treat stale success as health.
func ReadBrokerPlanUsage() PlanUsageSnapshot {
	store, err := currentPlanUsageStore()
	if err != nil {
		return PlanUsageSnapshot{Err: err.Error()}
	}
	rows, err := store.ReadPlanUsage()
	if err != nil {
		return PlanUsageSnapshot{Err: err.Error()}
	}
	snapshot := PlanUsageSnapshot{}
	for _, row := range rows {
		u := PlanUsage{Provider: row.Provider, Status: row.Status, Reason: row.Reason, FetchedAt: row.SuccessAt}
		if row.Status == PlanUsageAvailable && row.Reading != nil {
			u = *row.Reading
		}
		snapshot.Backends = append(snapshot.Backends, u)
		if row.SuccessAt.After(snapshot.FetchedAt) {
			snapshot.FetchedAt = row.SuccessAt
		}
	}
	return snapshot
}

const BrokerPlanRetryInterval = time.Minute

// PollBrokerPlanUsage is called only by the daemon's scheduled monitor.
// Reservations survive crashes. The broker never consults the shared JSON
// readings or throttle; no force/reload path bypasses this schedule.
func PollBrokerPlanUsage(ctx context.Context, clock Clock, fetch func(context.Context, Provider) (PlanUsage, error)) ([]PlanUsage, error) {
	store, err := currentPlanUsageStore()
	if err != nil {
		return nil, err
	}
	if clock == nil {
		clock = SystemClock{}
	}
	if fetch == nil {
		fetch = fetchBrokerProviderUsage
	}
	for _, provider := range []Provider{ProviderClaude, ProviderCodex, ProviderGrok, ProviderCursor, ProviderBedrock} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		now := clock.Now().UTC()
		previous, allowed, err := store.ReservePlanUsage(provider, now, now.Add(PlanThrottleMinInterval))
		if err != nil {
			return nil, err
		}
		if !allowed {
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, planUsageRefreshTimeout)
		reading, fetchErr := fetch(requestCtx, provider)
		cancel()
		if fetchErr != nil {
			reading = unavailablePlan(provider, now, fetchErr.Error())
		}
		reading.Provider, reading.FetchedAt, reading.RawBody = provider, now, ""
		next := now.Add(PlanThrottleMinInterval)
		refusals := previous.Refusals
		switch {
		case reading.HTTPStatus == 429 || reading.HTTPStatus == 529:
			if refusals < 5 {
				refusals++
			}
			penalty := PlanThrottleFirstPenalty << (refusals - 1)
			if penalty > PlanThrottleMaxPenalty {
				penalty = PlanThrottleMaxPenalty
			}
			next = now.Add(penalty)
		case reading.Status == PlanUsageAvailable:
			refusals = 0
		default:
			next = now.Add(BrokerPlanRetryInterval)
			if reading.HTTPStatus != 0 {
				refusals = 0
			}
		}
		observation := previous
		observation.Provider, observation.Status, observation.Reason = provider, reading.Status, reading.Reason
		observation.AttemptedAt, observation.NextAttemptAt, observation.Refusals = now, next, refusals
		if reading.Status == PlanUsageAvailable {
			for i := range reading.Windows {
				w := &reading.Windows[i]
				w.Band = ClassifyWindow(*w, now, DefaultPlanThresholds())
				if used := usedPercent(*w); used != nil {
					if remaining, ok := remainingTimePercent(*w, now); ok {
						pressure := Pressure(*used, 100-remaining, DefaultPlanThresholds())
						if !math.IsNaN(pressure) && !math.IsInf(pressure, 0) {
							w.Pressure = &pressure
						}
					}
				}
			}
			observation.Reading, observation.SuccessAt = &reading, now
		}
		if err := store.CompletePlanUsage(observation); err != nil {
			return nil, err
		}
	}
	snap := ReadBrokerPlanUsage()
	if snap.Err != "" {
		return nil, fmt.Errorf("%s", snap.Err)
	}
	return snap.Backends, nil
}

func fetchBrokerProviderUsage(ctx context.Context, provider Provider) (PlanUsage, error) {
	args := BrokerPlanFetchArgs()
	args.Providers = []Provider{provider}
	readings, err := QueryAllPlanUsage(ctx, args)
	if err != nil {
		return PlanUsage{}, err
	}
	if len(readings) != 1 {
		return PlanUsage{}, fmt.Errorf("%s usage: missing provider outcome", provider)
	}
	return readings[0], nil
}
