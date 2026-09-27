// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	if err != nil {
		shot.openErr = err
		return err
	}
	shot.openErr = nil
	shot.initial = cloneItem(initial)
	shot.item = cloneItem(item)
	shot.key = key
	shot.flushed = false
	shot.flushErr = nil
	return nil
}

// RecoverPlan first repairs a missed Keychain read, then asks pi-ai to
// refresh this provider. Only a rejected or absent refresh grant opens the
// provider's interactive login. No other provider's credential is changed.
func RecoverPlan(ctx context.Context, store Store, login Login, provider string) error {
	if !known(provider) {
		return fmt.Errorf("omp: %q is not a subscription provider", provider)
	}
	if err := RetryOpen(ctx, store); err != nil {
		return err
	}
	_, err := login.Refresh(ctx, store, provider)
	if err != nil && ctx.Err() == nil && strings.Contains(strings.ToLower(err.Error()), "invalid_grant") {
		login.ForceLogin = true
		_, err = login.Refresh(ctx, store, provider)
	}
	if err != nil {
		return err
	}
	// Flush normally runs once at broker shutdown. Recovery is an explicit
	// owner action: persist the new grant now, including after an earlier flush.
	shot.mu.Lock()
	shot.flushed = false
	shot.flushErr = nil
	shot.mu.Unlock()
	return Flush(ctx, store)
}
