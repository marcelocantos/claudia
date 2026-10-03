// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// RetryOpen retries a failed startup Keychain read when the owner is present.
// A successful startup copy is never discarded, so active seats keep using it.
func RetryOpen(ctx context.Context, store Store) error {
	shot.mu.Lock()
	defer shot.mu.Unlock()
	if shot.opened && shot.openErr == nil {
		return nil
	}
	shot.opened = true
	if err := rejectUntrustedBroker(store); err != nil {
		shot.openErr = err
		return err
	}
	if store.Run == nil {
		shot.openErr = fmt.Errorf("omp: no keychain runner")
		return shot.openErr
	}
	item, key, err := readItem(ctx, store)
	initial := item
	var bad *unreadableItemError
	if errors.As(err, &bad) {
		// The owner asked for recovery, so keep every record that still
		// decodes and let this login replace the damaged item. An empty
		// initial copy makes the recovery Flush write even when nothing
		// else changes.
		item, key, err = salvageItem(bad.raw), nil, nil
		initial = Item{Records: map[string]Record{}}
	}
	if errors.Is(err, errDataFileMismatch) {
		// The owner asked for recovery, and the plan file no longer opens
		// with the Keychain key: nothing in it can be read (🎯T144). Keep it
		// aside and start from an empty plan; this login's Flush seals a new
		// file under a new key.
		aside := store.DataPath + ".unreadable-" + time.Now().UTC().Format("20060102T150405Z")
		if rerr := os.Rename(store.DataPath, aside); rerr != nil {
			shot.openErr = err
			return err
		}
		item, key, err = Item{Records: map[string]Record{}}, nil, nil
		initial = Item{Records: map[string]Record{}}
	}
	if err != nil {
		shot.openErr = err
		return err
	}
	shot.openErr = nil
	shot.initial = cloneItem(initial)
	shot.item = cloneItem(item)
	shot.key = key
	shot.configErr = nil
	return nil
}

// RecoveryResult is deliberately non-secret: classifications are fixed codes,
// never provider stderr, credential material, or token fingerprints.
type RecoveryResult struct {
	Provider       string `json:"provider"`
	Outcome        string `json:"outcome"`
	Classification string `json:"classification"`
}

const (
	RecoveryHealthyNoOp = "healthy_no_op"
	RecoveryRefreshed   = "refreshed"
	RecoveryFailure     = "failure"
)

// RecoverPlan repairs one plan's login. A plan the broker holds as healthy,
// with a live access token, has nothing to repair: another refresh already
// replaced the token that was refused, and refreshing again would rotate it
// under every seat on the plan (🎯T165). Otherwise it refreshes, and on an
// invalid refresh grant falls back to an interactive sign-in, unless
// login.NoLogin says nobody is at the keyboard: then it marks the plan
// rejected and returns ErrNeedsSignIn.
func RecoverPlan(ctx context.Context, store Store, login Login, provider string) error {
	_, err := recoverPlan(ctx, store, login, provider)
	return err
}

// RecoverPlanNoLogin reports this invocation's branch, not inferred health or
// renewal timestamps. The caller must hold the plan's existing refresh lock.
func RecoverPlanNoLogin(ctx context.Context, store Store, login Login, provider string) RecoveryResult {
	login.NoLogin = true
	login.ForceLogin = false
	result, _ := recoverPlan(ctx, store, login, provider)
	return result
}

func recoverPlan(ctx context.Context, store Store, login Login, provider string) (RecoveryResult, error) {
	fail := func(class string, err error) (RecoveryResult, error) {
		if ctx.Err() != nil {
			class = "cancelled"
		}
		return RecoveryResult{Provider: provider, Outcome: RecoveryFailure, Classification: class}, err
	}
	if !known(provider) {
		return fail("unsupported_provider", fmt.Errorf("omp: %q is not a subscription provider", provider))
	}
	if err := RetryOpen(ctx, store); err != nil {
		return fail("store_unavailable", err)
	}
	if healthy, err := planHealthy(ctx, store, provider); err != nil {
		return fail("store_unavailable", err)
	} else if healthy {
		return RecoveryResult{Provider: provider, Outcome: RecoveryHealthyNoOp, Classification: "none"}, nil
	}
	_, err := login.Refresh(ctx, store, provider)
	if err != nil && ctx.Err() == nil && strings.Contains(strings.ToLower(err.Error()), "invalid_grant") {
		if login.NoLogin {
			MarkRejected(provider, err.Error())
			return fail("needs_sign_in", fmt.Errorf("omp: %s: %w (the refresh was refused: %v)", provider, ErrNeedsSignIn, err))
		}
		login.ForceLogin = true
		_, err = login.Refresh(ctx, store, provider)
	}
	if err != nil {
		if errors.Is(err, ErrNeedsSignIn) {
			return fail("needs_sign_in", err)
		}
		return fail("refresh_failed", err)
	}
	clearRejected(provider)
	if err := Flush(ctx, store); err != nil {
		return fail("persistence_failed", err)
	}
	return RecoveryResult{Provider: provider, Outcome: RecoveryRefreshed, Classification: "none"}, nil
}

// planHealthy reports whether the plan's login is neither marked rejected
// nor past its access token's expiry.
func planHealthy(ctx context.Context, store Store, provider string) (bool, error) {
	rejectedMu.Lock()
	_, marked := rejected[provider]
	rejectedMu.Unlock()
	if marked {
		return false, nil
	}
	item, err := store.Load(ctx)
	if err != nil {
		return false, err
	}
	return accessLive(store, item.Records[provider]), nil
}
