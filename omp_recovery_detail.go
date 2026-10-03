// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"

	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/omp"
)

// OMPRecoveryResult contains no credential or untrusted helper output.
type OMPRecoveryResult = omp.RecoveryResult

// RecoverOMPPlanDetailed is broker-side, strictly noninteractive recovery.
// It shares the same plan lock as owner recovery and T169 token recovery.
func RecoverOMPPlanDetailed(ctx context.Context, provider string) OMPRecoveryResult {
	if !IsOMPPlan(provider) {
		return OMPRecoveryResult{Provider: provider, Outcome: omp.RecoveryFailure, Classification: "unsupported_provider"}
	}
	mu := ompRefreshLock(provider)
	mu.Lock()
	result := omp.RecoverPlanNoLogin(ctx, planStore(), ompRecoveryLogin(false), provider)
	mu.Unlock()
	if result.Outcome != omp.RecoveryFailure {
		reloadOMPSeats(ctx, provider)
	}
	return result
}

// RecoverOMPAuthDetailed asks only the distinct no-login operation. An older
// daemon's unsupported-operation error is returned; never fall back to an
// acknowledgement that cannot distinguish refresh from a healthy no-op.
func RecoverOMPAuthDetailed(ctx context.Context, provider string) (OMPRecoveryResult, error) {
	if !IsOMPPlan(provider) {
		return OMPRecoveryResult{}, fmt.Errorf("omp: unsupported subscription provider")
	}
	client, err := dialBroker()
	if err != nil {
		return OMPRecoveryResult{}, err
	}
	defer client.Close()
	resp, err := client.call(ctx, &broker.Request{Type: broker.TypeAuthRecoverDetail, AuthDetail: &broker.NamedRequest{Name: provider}})
	if err != nil {
		return OMPRecoveryResult{}, err
	}
	if resp.Type != broker.TypeAuthRecoveryDetail || resp.AuthDetail == nil || resp.AuthDetail.Provider != provider {
		return OMPRecoveryResult{}, fmt.Errorf("omp: invalid detailed recovery response")
	}
	r := resp.AuthDetail
	if err := r.Validate(); err != nil {
		return OMPRecoveryResult{}, fmt.Errorf("omp: invalid detailed recovery outcome")
	}
	return OMPRecoveryResult{Provider: r.Provider, Outcome: r.Outcome, Classification: r.Classification}, nil
}
