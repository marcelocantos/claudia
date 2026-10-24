// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// BrokerPlanFetchArgs uses the broker's already-open in-memory subscription
// store for Claude and Grok usage. An expired, refreshable record can renew
// noninteractively through the existing auth owner; no Keychain read, browser
// sign-in or CLI credential fallback is permitted. Ordinary library callers
// keep DefaultPlanFetchArgs.
// Resolve on each fetch, rather than capturing a token that can be rotated.
func BrokerPlanFetchArgs() *AllPlanUsageArgs {
	args := DefaultPlanFetchArgs()
	args.ClaudeTokenSource = func(ctx context.Context) (string, error) {
		return brokerUsageToken(ctx, omp.Anthropic)
	}
	args.GrokTokenSource = func(ctx context.Context) (string, error) {
		return brokerUsageToken(ctx, omp.XAIOAuth)
	}
	return args
}

const planUsageRefreshTimeout = 30 * time.Second

func brokerUsageToken(ctx context.Context, provider string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	store := planStore()
	if token, err := store.AccessToken(ctx, provider); err == nil {
		return token, nil
	}
	mu := ompRefreshLock(provider)
	if !mu.TryLock() {
		return "", fmt.Errorf("subscription renewal already in progress; usage will retry on its next paced fetch")
	}
	defer mu.Unlock()
	// Another caller may have renewed between the first read and the lock.
	if token, err := store.AccessToken(ctx, provider); err == nil {
		return token, nil
	}
	item, err := store.Load(ctx)
	if err != nil {
		return "", fmt.Errorf("subscription store unavailable; explicit authentication recovery required")
	}
	rec, ok := item.Records[provider]
	if !ok || rec.AccessToken == "" || rec.Expiry.IsZero() || time.Now().Before(rec.Expiry) {
		return "", fmt.Errorf("subscription access credential missing; explicit authentication recovery required")
	}
	if !renewDue(rec, time.Now(), 0) {
		return "", fmt.Errorf("subscription access expired without a refresh credential; explicit sign-in required")
	}
	if err := store.BackgroundRefreshReady(); err != nil {
		return "", fmt.Errorf("expired subscription cannot renew: %w", err)
	}
	renewCtx, cancel := context.WithTimeout(ctx, planUsageRefreshTimeout)
	defer cancel()
	// Reuse the auth owner's rejection, backoff, persistence and seat-update
	// path. The usage endpoint never asks this path to rotate a valid token.
	_, err = renewOMPPlanLocked(renewCtx, provider, 0, "broker usage expiry")
	if err != nil {
		if strings.Contains(err.Error(), "invalid_grant") {
			return "", fmt.Errorf("subscription refresh grant rejected; explicit sign-in required")
		}
		if renewCtx.Err() != nil {
			return "", fmt.Errorf("subscription renewal cancelled or timed out: %w", renewCtx.Err())
		}
		return "", err
	}
	return store.AccessToken(ctx, provider)
}
