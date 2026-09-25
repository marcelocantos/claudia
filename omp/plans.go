// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"fmt"
)

// PlanIDs are the four subscription logins in the Keychain item.
var PlanIDs = []string{Anthropic, OpenAICodex, Cursor, XAIOAuth}

// RefreshPlans runs sidecar/auth.ts through pi-ai for every stored
// refresh token and writes the new record back. A provider with no
// refresh token is skipped — serve must not open a browser. Failure
// does not fall through to an API key.
func RefreshPlans(ctx context.Context, store Store, login Login) (refreshed, skipped []string, err error) {
	item, err := store.Load(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range PlanIDs {
		rec := item.Records[id]
		if rec.RefreshToken == "" {
			skipped = append(skipped, id)
			continue
		}
		if _, err := login.Refresh(ctx, store, id); err != nil {
			return refreshed, skipped, fmt.Errorf("omp: %s: %w", id, err)
		}
		refreshed = append(refreshed, id)
	}
	return refreshed, skipped, nil
}

// LoginPlans runs pi-ai login for every subscription id that has no
// refresh token and writes the record back. It opens a browser (or
// prints a device-code URL on stderr). Serve must not call this.
func LoginPlans(ctx context.Context, store Store, login Login) (int, error) {
	item, err := store.Load(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range PlanIDs {
		if item.Records[id].RefreshToken != "" {
			continue
		}
		if _, err := login.Refresh(ctx, store, id); err != nil {
			return n, fmt.Errorf("omp: %s: %w", id, err)
		}
		n++
	}
	return n, nil
}
