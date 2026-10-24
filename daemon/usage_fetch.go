// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"

	"github.com/marcelocantos/claudia"
)

func brokerUsageFetchers(injected func(context.Context) ([]claudia.PlanUsage, error)) (fetch, force func(context.Context) ([]claudia.PlanUsage, error)) {
	if injected != nil {
		return injected, injected
	}
	query := func(ctx context.Context, forced bool) ([]claudia.PlanUsage, error) {
		args := claudia.BrokerPlanFetchArgs()
		args.Forced = forced
		return claudia.QueryAllPlanUsage(ctx, args)
	}
	return func(ctx context.Context) ([]claudia.PlanUsage, error) { return query(ctx, false) },
		func(ctx context.Context) ([]claudia.PlanUsage, error) { return query(ctx, true) }
}
