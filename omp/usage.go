// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

const UsagePageLimit = 128

// UsageCounters preserves availability per field. Nil means unknown; a
// pointer to zero means an explicitly observed zero. Reasoning is a subset
// of output, never an additional billable token bucket.
type UsageCounters struct {
	InputTokens              *int64 `json:"input_tokens,omitempty"`
	OutputTokens             *int64 `json:"output_tokens,omitempty"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens,omitempty"`
	ReasoningTokens          *int64 `json:"reasoning_tokens,omitempty"`
}

type UsageRequestRef struct {
	RequestID string `json:"request_id"`
	AttemptID string `json:"attempt_id"`
}

// UsageFact is an observation of one engine invocation, not a census of
// hidden HTTP/WebSocket retries. It shares the durable conversation journal
// but never model context. APIEquivalentCostUSD is an estimate, not quota.
type UsageFact struct {
	Seq                  uint64            `json:"seq"`
	CallID               string            `json:"call_id"`
	Seat                 string            `json:"seat"`
	SessionID            string            `json:"session_id"`
	Kind                 string            `json:"kind"`
	State                string            `json:"state"`
	Provider             string            `json:"provider"`
	Model                string            `json:"model"`
	TurnID               string            `json:"turn_id,omitempty"`
	Requests             []UsageRequestRef `json:"requests"`
	Cause                string            `json:"cause,omitempty"`
	StartedAt            string            `json:"started_at"`
	FinishedAt           string            `json:"finished_at,omitempty"`
	Counters             UsageCounters     `json:"counters"`
	ContextTokens        *int64            `json:"context_tokens,omitempty"`
	APIEquivalentCostUSD *float64          `json:"api_equivalent_cost_usd,omitempty"`
	CounterSource        string            `json:"counter_source"`
	CostSource           string            `json:"cost_source,omitempty"`
	Coverage             string            `json:"coverage"`
}

func (f *UsageFact) ValidateIdentity(seat, session string) error {
	if f == nil || f.Seat != seat || f.SessionID != session || strings.TrimSpace(seat) == "" || strings.TrimSpace(session) == "" || f.Seq == 0 || f.CallID == "" || f.Provider == "" || f.Model == "" || f.Requests == nil || f.Coverage != "engine_invocation" || f.CounterSource != "adapter_normalized_positive" {
		return fmt.Errorf("usage fact: invalid identity or coverage")
	}
	if f.Kind != "model" && f.Kind != "compaction" {
		return fmt.Errorf("usage fact: invalid invocation kind")
	}
	start, err := time.Parse(time.RFC3339Nano, f.StartedAt)
	if err != nil {
		return fmt.Errorf("usage fact: invalid start time")
	}
	switch f.State {
	case "started":
		if f.FinishedAt != "" || f.Counters != (UsageCounters{}) || f.ContextTokens != nil || f.APIEquivalentCostUSD != nil {
			return fmt.Errorf("usage fact: unfinished invocation has counters")
		}
	case "completed", "failed", "cancelled", "unknown":
		finish, err := time.Parse(time.RFC3339Nano, f.FinishedAt)
		if err != nil || finish.Before(start) {
			return fmt.Errorf("usage fact: invalid finish time")
		}
	default:
		return fmt.Errorf("usage fact: invalid state")
	}
	seen := map[string]bool{}
	for _, r := range f.Requests {
		if r.RequestID == "" || r.AttemptID == "" || seen[r.RequestID] {
			return fmt.Errorf("usage fact: invalid request reference")
		}
		seen[r.RequestID] = true
	}
	for _, n := range []*int64{f.Counters.InputTokens, f.Counters.OutputTokens, f.Counters.CacheReadInputTokens, f.Counters.CacheCreationInputTokens, f.Counters.ReasoningTokens, f.ContextTokens} {
		if n != nil && (*n < 0 || *n > 1<<53-1) {
			return fmt.Errorf("usage fact: invalid counter")
		}
	}
	if c := f.APIEquivalentCostUSD; c != nil && (math.IsNaN(*c) || math.IsInf(*c, 0) || *c < 0 || f.CostSource != "provider_adapter_estimate") {
		return fmt.Errorf("usage fact: invalid cost estimate")
	}
	return nil
}

type UsagePage struct {
	Seat      string      `json:"seat"`
	SessionID string      `json:"session_id"`
	Facts     []UsageFact `json:"facts"`
	Next      uint64      `json:"next"`
	More      bool        `json:"more"`
}

func (p *UsagePage) Validate(seat, session string, after uint64, limit int) error {
	if p == nil || strings.TrimSpace(seat) == "" || strings.TrimSpace(session) == "" || limit < 1 || limit > UsagePageLimit || after > 1<<53-1 || p.Seat != seat || p.SessionID != session || p.Facts == nil || len(p.Facts) > limit || p.Next != after+uint64(len(p.Facts)) || (p.More && len(p.Facts) == 0) {
		return fmt.Errorf("usage page: invalid identity or cursor")
	}
	for i := range p.Facts {
		if p.Facts[i].Seq != after+uint64(i)+1 {
			return fmt.Errorf("usage page: noncontiguous sequence")
		}
		if err := p.Facts[i].ValidateIdentity(seat, session); err != nil {
			return err
		}
	}
	return nil
}

// QueryUsage reads a bounded page without loading a seat or credentials.
func QueryUsage(ctx context.Context, seat, session string, after uint64, limit int) (*UsagePage, error) {
	path, err := SocketPath()
	if err != nil {
		return nil, err
	}
	conn, err := Dial(ctx, path)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	rpc := uuid.NewString()
	if err := conn.Send(Message{Op: "usage", Seat: seat, SessionID: session, RPCID: rpc, After: after, Limit: limit}); err != nil {
		return nil, err
	}
	for {
		ev, err := conn.Recv()
		if err != nil {
			return nil, err
		}
		if ev.RPCID != rpc {
			continue
		}
		if ev.Type != "usage_page" {
			return nil, fmt.Errorf("usage query: %s: %s", ev.Type, ev.Text)
		}
		if err := ev.UsagePage.Validate(seat, session, after, limit); err != nil {
			return nil, err
		}
		return ev.UsagePage, nil
	}
}
