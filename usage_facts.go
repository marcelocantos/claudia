// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/marcelocantos/claudia/internal/broker"
	"github.com/marcelocantos/claudia/omp"
)

type UsageFact = omp.UsageFact
type UsageCounters = omp.UsageCounters
type UsageRequestRef = omp.UsageRequestRef
type UsagePage = omp.UsagePage

const UsagePageLimit = omp.UsagePageLimit

type UsageQuery struct {
	Seat, SessionID string
	After           uint64
	Limit           int
}

// QueryUsage returns a bounded durable page, including for unloaded seats.
// It does not obtain credentials, start providers, or alter execution.
func QueryUsage(ctx context.Context, q UsageQuery) (*UsagePage, error) {
	return queryUsage(ctx, q, false)
}

// QueryUsageDirect bypasses broker routing for the broker's own handler.
func QueryUsageDirect(ctx context.Context, q UsageQuery) (*UsagePage, error) {
	return queryUsage(ctx, q, true)
}

func queryUsage(ctx context.Context, q UsageQuery, direct bool) (*UsagePage, error) {
	if q.Limit == 0 {
		q.Limit = UsagePageLimit
	}
	if strings.TrimSpace(q.Seat) == "" || strings.TrimSpace(q.SessionID) == "" || q.Limit < 1 || q.Limit > UsagePageLimit {
		return nil, fmt.Errorf("usage query requires seat/session and a bounded limit")
	}
	ctx, cancel := context.WithTimeout(ctx, receiptOpTimeout)
	defer cancel()
	if !direct && usingBroker() {
		b, err := dialBroker()
		if err != nil {
			return nil, err
		}
		defer b.Close()
		return queryBrokerUsage(ctx, b, q)
	}
	return omp.QueryUsage(ctx, q.Seat, q.SessionID, q.After, q.Limit)
}

func queryBrokerUsage(ctx context.Context, b *brokerClient, q UsageQuery) (*UsagePage, error) {
	request := &broker.UsageFactsRequest{Seat: q.Seat, SessionID: q.SessionID, After: q.After, Limit: q.Limit}
	if deadline, ok := ctx.Deadline(); ok {
		request.ExpiresAt = deadline.UnixMilli()
	}
	resp, err := b.call(ctx, &broker.Request{Type: broker.TypeUsageFacts, UsageFacts: request})
	if err != nil {
		return nil, err
	}
	if resp.Type != broker.TypeUsageFactsResult {
		return nil, fmt.Errorf("broker returned no usage page")
	}
	if err := resp.UsageFacts.Validate(q.Seat, q.SessionID, q.After, q.Limit); err != nil {
		return nil, err
	}
	return resp.UsageFacts, nil
}

// RefreshUsage catches up this handle from the durable journal in bounded
// pages. It emits previously unseen facts through Event and adds each known
// counter once to Usage. Context size, reasoning subsets, estimated dollars
// and unknown counters are never added as token consumption. TotalInputTokens
// remains available in the typed facts, not added again to disjoint buckets.
// Coverage is declared per fact; uninstrumented backends retain retry gaps.
func (a *Agent) RefreshUsage(ctx context.Context) error {
	if a == nil || a.ops.usagePage == nil {
		return fmt.Errorf("backend has no durable usage API")
	}
	ctx, cancel := context.WithTimeout(ctx, receiptOpTimeout)
	defer cancel()
	a.usageReplayMu.Lock()
	defer a.usageReplayMu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := a.ops.usagePage(ctx, a, a.usageCursor, UsagePageLimit)
		if err != nil {
			return err
		}
		if page == nil {
			return fmt.Errorf("backend returned no usage page")
		}
		a.mu.Lock()
		seat, session := a.startCfg.Name, a.sessionID
		a.mu.Unlock()
		if err := page.Validate(seat, session, a.usageCursor, UsagePageLimit); err != nil {
			return err
		}
		for i := range page.Facts {
			f := page.Facts[i]
			a.publishEvent(Event{Type: "usage", SessionID: f.SessionID, UsageFact: &f})
			if err := a.UsageError(); err != nil {
				return err
			}
		}
		a.usageCursor = page.Next
		if !page.More {
			return nil
		}
	}
}

// UsageError reports a sticky accounting observation failure on this handle.
// Nil does not certify completeness: coverage and per-counter availability
// are declared per fact; uninstrumented backends retain provider retry gaps.
func (a *Agent) UsageError() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usageErr
}

// Called with a.mu held. Fact snapshots are not additive. A duplicate or an
// older start replay after its final fact contributes no delta and no event.
func (a *Agent) applyUsageFactLocked(ev *Event) (bool, error) {
	f := ev.UsageFact
	if err := f.ValidateIdentity(f.Seat, a.sessionID); err != nil {
		return false, err
	}
	if name := a.startCfg.Name; name != "" && name != f.Seat {
		return false, fmt.Errorf("usage fact: seat identity mismatch")
	}
	if a.usageCalls == nil {
		a.usageCalls = map[string]UsageFact{}
	}
	key := f.Seat + "\x00" + f.SessionID + "\x00" + f.CallID
	if old, exists := a.usageCalls[key]; exists {
		if f.Seq < old.Seq {
			return false, nil
		}
		if f.Seq == old.Seq {
			if !reflect.DeepEqual(old, *f) {
				return false, fmt.Errorf("usage fact conflicts with recorded identity")
			}
			return false, nil
		}
		if old.State != "started" || f.State == "started" || old.Coverage != f.Coverage || old.CounterSource != f.CounterSource || old.InvocationID != f.InvocationID || old.ProviderAttemptID != f.ProviderAttemptID || old.StartedAt != f.StartedAt || old.Provider != f.Provider || old.Model != f.Model || old.Kind != f.Kind || old.TurnID != f.TurnID || old.Cause != f.Cause || !reflect.DeepEqual(old.Requests, f.Requests) {
			return false, fmt.Errorf("usage fact has invalid invocation transition")
		}
	}
	// Keep an independent copy: an event subscriber may mutate its payload.
	data, err := json.Marshal(f)
	if err != nil {
		return false, err
	}
	var saved UsageFact
	if err := json.Unmarshal(data, &saved); err != nil {
		return false, err
	}
	a.usageCalls[key] = saved
	ev.Usage = Usage{}
	if f.State == "started" {
		return true, nil
	}
	value := func(n *int64) int {
		if n == nil {
			return 0
		}
		return int(*n)
	}
	ev.Usage = Usage{InputTokens: value(f.Counters.InputTokens), OutputTokens: value(f.Counters.OutputTokens), CacheCreationInputTokens: value(f.Counters.CacheCreationInputTokens), CacheReadInputTokens: value(f.Counters.CacheReadInputTokens)}
	return true, nil
}
