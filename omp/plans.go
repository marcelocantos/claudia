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

// RefreshPlans renews every expired subscription login through pi-ai,
// then writes the Keychain item once. A login that is still valid is
// left as it is. A provider with no refresh token is skipped — serve
// must not open a browser. Failure does not fall through to an API key,
// and it does not write a partial item.
func RefreshPlans(ctx context.Context, store Store, login Login) (refreshed, skipped []string, err error) {
	item, err := store.Load(ctx)
	if err != nil {
		return nil, nil, err
	}
	if item.Records == nil {
		item.Records = map[string]Record{}
	}
	for _, id := range PlanIDs {
		rec := item.Records[id]
		if !usableRefresh(rec) {
			skipped = append(skipped, id)
			continue
		}
		if accessLive(store, rec) {
			continue
		}
		next, err := login.fetch(ctx, id, rec)
		if err != nil {
			return refreshed, skipped, fmt.Errorf("omp: %s: %w", id, err)
		}
		item.Records[id] = next
		refreshed = append(refreshed, id)
	}
	if len(refreshed) == 0 {
		return refreshed, skipped, nil
	}
	if err := store.Save(ctx, item); err != nil {
		return refreshed, skipped, err
	}
	return refreshed, skipped, nil
}

func accessLive(store Store, rec Record) bool {
	return rec.AccessToken != "" && !rec.Expiry.IsZero() && store.now().Before(rec.Expiry)
}

// LoginPlans runs pi-ai login for every subscription id that has no
// refresh token, then writes the Keychain item once. It opens a browser
// (or prints a device-code URL on stderr). Serve must not call this.
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
	if item.Records == nil {
		item.Records = map[string]Record{}
	}
	n := 0
	for _, id := range want {
		if len(only) == 0 && usableRefresh(item.Records[id]) {
			continue
		}
		fmt.Fprintf(os.Stderr, "omp: login %s through pi-ai (browser or device code)\n", id)
		next, err := login.fetch(ctx, id, item.Records[id])
		if err != nil {
			return n, fmt.Errorf("omp: %s: %w", id, err)
		}
		item.Records[id] = next
		n++
	}
	if n == 0 {
		return 0, nil
	}
	if err := store.Save(ctx, item); err != nil {
		return n, err
	}
	return n, nil
}
