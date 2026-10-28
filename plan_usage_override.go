// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0
package claudia

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// brokerUsageOverride is the topology-B in-memory allowlist and last pushed
// readings. It never writes a consumer replica. PollBrokerPlanUsage still
// commits through the broker-owned control file.
var brokerUsageOverride struct {
	mu       sync.Mutex
	allow    map[Provider]bool
	readings map[Provider]PlanUsage
}

// InstallBrokerUsageOverride installs the CLI-fixed provider allowlist for
// topology-B isolated brokers (🎯T234). Test-only; not production surface.
// Readings start empty: an admitted provider with no push is unpublished.
func InstallBrokerUsageOverride(providers []Provider) error {
	allow := make(map[Provider]bool, len(providers))
	for _, p := range providers {
		switch p {
		case ProviderClaude, ProviderCodex, ProviderGrok, ProviderCursor, ProviderBedrock:
		default:
			return fmt.Errorf("usage override: unknown provider %q", p)
		}
		if allow[p] {
			return fmt.Errorf("usage override: duplicate provider %q", p)
		}
		allow[p] = true
	}
	if len(allow) == 0 {
		return fmt.Errorf("usage override: empty provider allowlist")
	}
	brokerUsageOverride.mu.Lock()
	brokerUsageOverride.allow = allow
	brokerUsageOverride.readings = map[Provider]PlanUsage{}
	brokerUsageOverride.mu.Unlock()
	return nil
}

// ClearBrokerUsageOverride drops the allowlist and stored readings.
// Test-only; not production surface.
func ClearBrokerUsageOverride() {
	brokerUsageOverride.mu.Lock()
	brokerUsageOverride.allow = nil
	brokerUsageOverride.readings = nil
	brokerUsageOverride.mu.Unlock()
}

// brokerUsageOverrideAllows reports whether p is in the installed allowlist.
func brokerUsageOverrideAllows(p Provider) bool {
	brokerUsageOverride.mu.Lock()
	defer brokerUsageOverride.mu.Unlock()
	return brokerUsageOverride.allow[p]
}

// StoreBrokerUsageOverride records the last pushed readings for admitted
// providers. Unenrolled providers are refused and nothing is stored.
// Test-only; not production surface.
func StoreBrokerUsageOverride(readings []PlanUsage) error {
	if len(readings) == 0 {
		return fmt.Errorf("usage override: empty generation")
	}
	brokerUsageOverride.mu.Lock()
	defer brokerUsageOverride.mu.Unlock()
	if len(brokerUsageOverride.allow) == 0 {
		return fmt.Errorf("usage override: allowlist is not installed")
	}
	next := make(map[Provider]PlanUsage, len(readings))
	for _, u := range readings {
		if !brokerUsageOverride.allow[u.Provider] {
			return fmt.Errorf("usage override: provider %q is not in the override allowlist", u.Provider)
		}
		if _, dup := next[u.Provider]; dup {
			return fmt.Errorf("usage override: duplicate provider %q", u.Provider)
		}
		next[u.Provider] = u
	}
	if brokerUsageOverride.readings == nil {
		brokerUsageOverride.readings = map[Provider]PlanUsage{}
	}
	for p, u := range next {
		brokerUsageOverride.readings[p] = u
	}
	return nil
}

func lookupBrokerUsageOverride(p Provider) (PlanUsage, bool) {
	brokerUsageOverride.mu.Lock()
	defer brokerUsageOverride.mu.Unlock()
	if !brokerUsageOverride.allow[p] {
		return PlanUsage{}, false
	}
	u, ok := brokerUsageOverride.readings[p]
	return u, ok
}

// PollBrokerPlanUsageWithOverride is PollBrokerPlanUsage with a per-provider
// fetch wrapper: admitted override providers never reach the vendor, and
// every other provider uses fetch (or the real broker fetch when fetch is nil).
// Test-only topology-B seam (🎯T234); not production surface.
func PollBrokerPlanUsageWithOverride(ctx context.Context, clock Clock, fetch func(context.Context, Provider) (PlanUsage, error)) ([]PlanUsage, error) {
	return PollBrokerPlanUsage(ctx, clock, func(ctx context.Context, provider Provider) (PlanUsage, error) {
		if brokerUsageOverrideAllows(provider) {
			if u, ok := lookupBrokerUsageOverride(provider); ok {
				return u, nil
			}
			now := time.Now().UTC()
			if clock != nil {
				now = clock.Now().UTC()
			}
			return unavailablePlan(provider, now, "usage override idle"), nil
		}
		if fetch != nil {
			return fetch(ctx, provider)
		}
		return fetchBrokerProviderUsage(ctx, provider)
	})
}
