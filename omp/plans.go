// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// PlanIDs are the four subscription logins in the Keychain item.
var PlanIDs = []string{Anthropic, OpenAICodex, Cursor, XAIOAuth}

// RefreshPlans renews every expired subscription login through pi-ai.
// Each success is saved immediately so a later failure cannot discard a
// rotated refresh token. A login that is still valid is left as it is.
// A provider with no refresh token is skipped — serve must not open a
// browser. One plan's bad refresh token does not abort the others
// (🎯T868). Failure does not fall through to an API key.
func RefreshPlans(ctx context.Context, store Store, login Login) (refreshed, skipped []string, err error) {
	item, err := store.Load(ctx)
	if err != nil {
		return nil, nil, err
	}
	if item.Records == nil {
		item.Records = map[string]Record{}
	}
	want := PlanIDs
	if only := strings.TrimSpace(os.Getenv("OMP_REFRESH_ONLY")); only != "" {
		want = strings.Split(only, ",")
		for _, id := range want {
			if !known(strings.TrimSpace(id)) {
				return nil, nil, fmt.Errorf("omp: %q is not a subscription provider", id)
			}
		}
	}
	for _, raw := range want {
		id := strings.TrimSpace(raw)
		rec := item.Records[id]
		if !usableRefresh(rec) {
			skipped = append(skipped, id)
			continue
		}
		if accessLive(store, rec) && (!login.ForceRefresh || stringGrant(rec)) {
			skipped = append(skipped, id)
			continue
		}
		next, ferr := login.fetch(ctx, id, rec)
		if ferr != nil {
			err = errors.Join(err, fmt.Errorf("omp: %s: %w", id, ferr))
			continue
		}
		item.Records[id] = next
		if serr := store.Save(ctx, item); serr != nil {
			err = errors.Join(err, serr)
			continue
		}
		refreshed = append(refreshed, id)
	}
	return refreshed, skipped, err
}

func accessLive(store Store, rec Record) bool {
	return rec.AccessToken != "" && !rec.Expiry.IsZero() && store.now().Before(rec.Expiry)
}

// stringGrant is a pi-ai login that returned a user key (Cursor), not
// a refreshable OAuth pair. Forcing refresh on that shape is a 401.
func stringGrant(rec Record) bool {
	return rec.RefreshToken != "" && rec.RefreshToken == rec.AccessToken
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
