// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

// OMPRenewMargin is how long before a plan's access token expires the broker
// renews it (🎯T168).
const OMPRenewMargin = 10 * time.Minute

// OMPRenewInterval is how often the broker looks for a token about to expire.
// It is well inside OMPRenewMargin, so a token is renewed before it lapses.
const OMPRenewInterval = time.Minute

// ompRenewRetry is how long a renewal that failed for a transient reason
// waits before the next attempt, so a network outage costs one log line a
// few minutes rather than one a tick.
const ompRenewRetry = 5 * time.Minute

var (
	ompRenewFailedMu sync.Mutex
	ompRenewFailed   = map[string]time.Time{} // provider -> last failed renewal
)

// RenewExpiringOMPPlans renews every plan whose access token expires within
// margin, and hands the new token to that plan's seats (🎯T168).
//
// Plan tokens were renewed only at broker boot and when a seat was refused.
// Since 🎯T166 the sidecar holds no refresh token, so nothing renewed a token
// before it lapsed: on 2026-10-01 the owner's first message after eleven idle
// hours was refused with "OAuth access token has expired", and the turn was
// lost although the refresh that followed succeeded.
//
// A renewal here never opens a sign-in. A plan that needs one, or is already
// marked rejected, is left for the owner's Reauth.
func RenewExpiringOMPPlans(ctx context.Context, margin time.Duration) (renewed []string, err error) {
	store := planStore()
	item, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	health, err := omp.Health(ctx, store)
	if err != nil {
		return nil, err
	}
	rejected := map[string]bool{}
	for _, h := range health {
		if h.State == omp.HealthRejected {
			rejected[h.Provider] = true
		}
	}
	now := time.Now()
	for _, id := range omp.PlanIDs {
		if rejected[id] || !renewDue(item.Records[id], now, margin) {
			continue
		}
		ompRenewFailedMu.Lock()
		failed := ompRenewFailed[id]
		ompRenewFailedMu.Unlock()
		if !failed.IsZero() && now.Sub(failed) < ompRenewRetry {
			continue
		}
		ok, rerr := renewOMPPlan(ctx, id, margin)
		if rerr != nil {
			err = errors.Join(err, rerr)
			continue
		}
		if ok {
			renewed = append(renewed, id)
		}
	}
	return renewed, err
}

// renewDue reports whether rec is a refreshable OAuth login whose access
// token expires within margin of now. A login with no expiry, no refresh
// token, or a user key in place of an OAuth pair (Cursor) is never due.
func renewDue(rec omp.Record, now time.Time, margin time.Duration) bool {
	if rec.RefreshToken == "" || rec.RefreshToken == "undefined" || rec.RefreshToken == rec.AccessToken {
		return false
	}
	if rec.Expiry.IsZero() {
		return false
	}
	return !now.Add(margin).Before(rec.Expiry)
}

// renewOMPPlan renews one plan under its refresh lock, the one a refused
// seat's own refresh takes (🎯T158), so the two never present the same
// refresh token. It reports false when another renewal got there first.
func renewOMPPlan(ctx context.Context, id string, margin time.Duration) (bool, error) {
	mu := ompRefreshLock(id)
	mu.Lock()
	defer mu.Unlock()
	store := planStore()
	item, err := store.Load(ctx)
	if err != nil {
		return false, err
	}
	if !renewDue(item.Records[id], time.Now(), margin) {
		return false, nil
	}
	login := ompLogin
	if login.Run == nil {
		login.Run = execBunLogin
	}
	if login.Command == "" {
		login.Command = "bun"
	}
	if login.Script == "" {
		login.Script = sidecarAuthScript()
	}
	login.Caller = "expiry ahead"
	login.NoLogin = true
	if _, err := login.Refresh(ctx, store, id); err != nil {
		ompRenewFailedMu.Lock()
		ompRenewFailed[id] = time.Now()
		ompRenewFailedMu.Unlock()
		if errors.Is(err, omp.ErrNeedsSignIn) || strings.Contains(strings.ToLower(err.Error()), "invalid_grant") {
			// 🎯T924: the cockpit offers the owner a Reauth for this plan.
			omp.MarkRejected(id, err.Error())
		}
		return false, err
	}
	ompRenewFailedMu.Lock()
	delete(ompRenewFailed, id)
	ompRenewFailedMu.Unlock()
	if n := reloadOMPSeats(ctx, id); n > 0 {
		slog.Info("omp plan renewed ahead of expiry; seats took the new token", "provider", id, "seats", n)
	}
	return true, nil
}

// RunOMPPlanRenewal renews plans ahead of expiry every interval until ctx
// ends (🎯T168). The broker runs it for its whole life.
func RunOMPPlanRenewal(ctx context.Context, interval, margin time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := RenewExpiringOMPPlans(ctx, margin); err != nil && ctx.Err() == nil {
			slog.Warn("omp plan renewal ahead of expiry failed; retrying later", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
