// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"fmt"
	"reflect"
)

// ValidateUsageTransition decides whether incoming replaces a projection's
// previous fact for one call. True means insert/replace; false means an exact
// duplicate or an older started fact replayed after its terminal observation.
// Errors identify invalid facts, changed identity or conflicting transitions.
// Both arguments are validated and neither is modified. A nil previous allows
// a first-seen terminal fact (for example, a spool event ahead of query replay).
// Callers must separately bind the first fact to their expected seat/session;
// this function validates the fact contract, not external routing authority.
func ValidateUsageTransition(previous, incoming *UsageFact) (bool, error) {
	if incoming == nil {
		return false, fmt.Errorf("usage transition: missing incoming fact")
	}
	if err := incoming.ValidateIdentity(incoming.Seat, incoming.SessionID); err != nil {
		return false, fmt.Errorf("usage transition incoming: %w", err)
	}
	if previous == nil {
		return true, nil
	}
	if err := previous.ValidateIdentity(previous.Seat, previous.SessionID); err != nil {
		return false, fmt.Errorf("usage transition previous: %w", err)
	}
	// The contract owns its mutable terminal measurements. Consumers must not
	// independently strip fields or infer identity from request IDs alone.
	identity := func(f UsageFact) UsageFact {
		f.Seq, f.State, f.FinishedAt = 0, "", ""
		f.Counters = UsageCounters{}
		f.ContextTokens, f.ProtocolTokenDeltaTotal, f.APIEquivalentCostUSD = nil, nil, nil
		f.CostSource, f.ProviderRequestID, f.ProviderResponseID = "", "", ""
		return f
	}
	if !reflect.DeepEqual(identity(*previous), identity(*incoming)) {
		return false, fmt.Errorf("usage transition: immutable identity changed")
	}
	if incoming.Seq == previous.Seq {
		if !reflect.DeepEqual(*previous, *incoming) {
			return false, fmt.Errorf("usage transition: same sequence has conflicting facts")
		}
		return false, nil
	}
	if incoming.Seq < previous.Seq {
		if incoming.State == "started" && previous.State != "started" {
			return false, nil
		}
		return false, fmt.Errorf("usage transition: invalid older replay")
	}
	if previous.State != "started" || incoming.State == "started" {
		return false, fmt.Errorf("usage transition: expected started to terminal")
	}
	return true, nil
}
