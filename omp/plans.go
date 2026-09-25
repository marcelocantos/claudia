// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"fmt"
	"os"
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
		if !usableRefresh(rec) {
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
// only, if set, limits the run to those provider ids.
func LoginPlans(ctx context.Context, store Store, login Login, only ...string) (int, error) {
	want := PlanIDs
	if len(only) > 0 {
		want = only
		for _, id := range want {
			if !known(id) {
				return 0, fmt.Errorf("omp: %q is not a subscription provider", id)
			}
		}
	}
	item, err := store.Load(ctx)
	if err != nil {
		return 0, err
	}
	if len(only) > 0 {
		login.ForceLogin = true
	}
	n := 0
	for _, id := range want {
		if len(only) == 0 && usableRefresh(item.Records[id]) {
			continue
		}
		fmt.Fprintf(os.Stderr, "omp: login %s through pi-ai (browser or device code)\n", id)
		if _, err := login.Refresh(ctx, store, id); err != nil {
			return n, fmt.Errorf("omp: %s: %w", id, err)
		}
		n++
	}
	return n, nil
}
