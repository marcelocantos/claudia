// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"maps"
	"strings"
	"sync"
	"time"
)

// Plan login health (🎯T924). Reading it never starts a login.
const (
	// HealthOK is a login that works now or renews without the owner.
	HealthOK = "ok"
	// HealthMissing is a plan with no saved login at all.
	HealthMissing = "missing"
	// HealthExpired is an access token past its expiry with no refresh
	// token to renew it.
	HealthExpired = "expired"
	// HealthRejected is a login the provider refused on its last use and
	// that did not renew.
	HealthRejected = "rejected"
)

// PlanHealth is one plan's login state as the broker's store holds it.
type PlanHealth struct {
	Provider string    `json:"provider"`
	State    string    `json:"state"`
	Detail   string    `json:"detail,omitempty"`
	Since    time.Time `json:"since,omitzero"`
}

// maxRejectionDetail bounds the cause the cockpit shows on a Reauth.
const maxRejectionDetail = 240

type rejection struct {
	at     time.Time
	detail string
}

var (
	rejectedMu sync.Mutex
	rejected   = map[string]rejection{}
)

// MarkRejected records that the provider refused this plan's login and it
// did not renew. It lasts until a new login for the plan is saved.
func MarkRejected(provider, detail string) {
	if !known(provider) {
		return
	}
	rejectedMu.Lock()
	defer rejectedMu.Unlock()
	// The cause is often on a later line of a login's stderr: keep it all,
	// on one line.
	flat := strings.Join(strings.Fields(detail), " ")
	if len(flat) > maxRejectionDetail {
		flat = flat[:maxRejectionDetail] + "…"
	}
	rejected[provider] = rejection{at: time.Now(), detail: flat}
}

// clearRejected forgets a rejection once a new login is saved.
func clearRejected(provider string) {
	rejectedMu.Lock()
	defer rejectedMu.Unlock()
	delete(rejected, provider)
}

// Health reports every plan's login state from the store, without asking
// any provider and without opening a login.
func Health(ctx context.Context, store Store) ([]PlanHealth, error) {
	item, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	rejectedMu.Lock()
	marks := maps.Clone(rejected)
	rejectedMu.Unlock()
	out := make([]PlanHealth, 0, len(PlanIDs))
	for _, id := range PlanIDs {
		rec, ok := item.Records[id]
		h := PlanHealth{Provider: id, State: HealthOK}
		switch {
		case !ok || (rec.AccessToken == "" && !usableRefresh(rec)):
			h.State = HealthMissing
		case !marks[id].at.IsZero():
			h.State, h.Detail, h.Since = HealthRejected, marks[id].detail, marks[id].at
		case usableRefresh(rec) || accessLive(store, rec):
		default:
			h.State, h.Since = HealthExpired, rec.Expiry
		}
		out = append(out, h)
	}
	return out, nil
}
