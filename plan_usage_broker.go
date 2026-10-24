// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"

	"github.com/marcelocantos/claudia/omp"
)

// BrokerPlanFetchArgs uses the broker's already-open in-memory subscription
// store for Grok billing. It never opens the Keychain, renews a login, or reads
// a CLI credential. Ordinary library callers keep DefaultPlanFetchArgs.
// Resolve on each fetch, rather than capturing a token that can be rotated.
func BrokerPlanFetchArgs() *AllPlanUsageArgs {
	args := DefaultPlanFetchArgs()
	args.GrokTokenSource = func(ctx context.Context) (string, error) {
		return planStore().AccessToken(ctx, omp.XAIOAuth)
	}
	return args
}
