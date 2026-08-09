// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// PlanUsageStatus reports whether subscription-style plan remaining was
// obtained for a provider.
type PlanUsageStatus string

const (
	// PlanUsageAvailable means at least one plan window was published.
	PlanUsageAvailable PlanUsageStatus = "available"
	// PlanUsageUnavailable means the provider does not publish remaining
	// or rollover (or credentials/surface were missing). Numbers are never
	// invented in this case — Windows is empty and Reason is set.
	PlanUsageUnavailable PlanUsageStatus = "unavailable"
)

// PlanWindowName identifies a subscription plan window.
// Session is the short rolling window (Claude 5-hour / Codex short primary);
// weekly is the longer plan window (Claude 7-day / Codex weekly).
type PlanWindowName string

const (
	// PlanWindowSession is the short rolling allowance (Claude five_hour,
	// Codex primary when limit_window_seconds is ~5h).
	PlanWindowSession PlanWindowName = "session"
	// PlanWindowWeekly is the longer plan allowance (Claude seven_day,
	// Codex primary/secondary when limit_window_seconds is ~7d).
	PlanWindowWeekly PlanWindowName = "weekly"
)

// PlanWindow is one published remaining/rollover window.
// Providers publish used% and/or remaining%; both are filled when either is
// known. ResetsAt is the next rollover when the provider publishes one.
type PlanWindow struct {
	Name PlanWindowName `json:"name"`
	// UsedPercent is 0–100 of the window consumed, when known.
	UsedPercent *float64 `json:"used_percent,omitempty"`
	// RemainingPercent is 0–100 of the window still available, when known.
	RemainingPercent *float64 `json:"remaining_percent,omitempty"`
	// ResetsAt is when this window rolls over, when known.
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	// LimitWindow is the provider's published window length when known.
	LimitWindow time.Duration `json:"limit_window,omitempty"`
}

// PlanUsage is a snapshot of subscription-style plan remaining for one
// provider. Distinct from per-run token [Usage] / CostUSD on Task events.
type PlanUsage struct {
	Provider Provider        `json:"provider"`
	Status   PlanUsageStatus `json:"status"`
	// Reason explains Status==unavailable (or notes for partial surfaces).
	Reason    string       `json:"reason,omitempty"`
	Windows   []PlanWindow `json:"windows,omitempty"`
	FetchedAt time.Time    `json:"fetched_at"`
	// PlanType is an optional provider plan label when published
	// (e.g. Codex "pro", Claude subscription tier).
	PlanType string `json:"plan_type,omitempty"`
}

// PlanUsageArgs configures [QueryPlanUsage].
// Zero values use package defaults (real HTTP, keychain / auth files).
// Never use functional options — pass a struct pointer.
type PlanUsageArgs struct {
	// Provider is required.
	Provider Provider
	// HTTPClient is used for outbound fetches. Nil uses http.DefaultClient.
	HTTPClient *http.Client
	// ClaudeAccessToken overrides Claude.ai OAuth (keychain / env).
	// Empty means load via the default credential path.
	ClaudeAccessToken string
	// CodexAccessToken overrides ChatGPT access token from ~/.codex/auth.json.
	CodexAccessToken string
	// CodexAccountID is sent as ChatGPT-Account-Id when non-empty.
	CodexAccountID string
	// CodexAuthPath overrides the path to Codex auth.json (tests).
	// Empty means ~/.codex/auth.json.
	CodexAuthPath string
	// ClaudeUsageURL overrides the Claude OAuth usage endpoint (tests).
	ClaudeUsageURL string
	// CodexUsageURL overrides the Codex/ChatGPT usage endpoint (tests).
	CodexUsageURL string
	// Now overrides wall clock for FetchedAt (tests). Zero uses time.Now.
	Now time.Time
}

// AllPlanUsageArgs configures [QueryAllPlanUsage].
type AllPlanUsageArgs struct {
	// HTTPClient is used for outbound fetches. Nil uses http.DefaultClient.
	HTTPClient *http.Client
	// ClaudeAccessToken overrides Claude.ai OAuth (keychain / env).
	ClaudeAccessToken string
	// CodexAccessToken overrides ChatGPT access token from ~/.codex/auth.json.
	CodexAccessToken string
	// CodexAccountID is sent as ChatGPT-Account-Id when non-empty.
	CodexAccountID string
	// CodexAuthPath overrides the path to Codex auth.json (tests).
	CodexAuthPath string
	// ClaudeUsageURL overrides the Claude OAuth usage endpoint (tests).
	ClaudeUsageURL string
	// CodexUsageURL overrides the Codex/ChatGPT usage endpoint (tests).
	CodexUsageURL string
	// Now overrides wall clock for FetchedAt (tests). Zero uses time.Now.
	Now time.Time
	// Providers limits which providers to query. Empty means all supported
	// providers (Claude, Codex, Grok, Bedrock).
	Providers []Provider
}

// QueryPlanUsage returns subscription plan remaining for one provider.
// Backends that do not publish remaining/rollover return Status unavailable
// with an explicit Reason — never invented percentages.
func QueryPlanUsage(ctx context.Context, args *PlanUsageArgs) (PlanUsage, error) {
	if args == nil {
		return PlanUsage{}, fmt.Errorf("PlanUsageArgs is required")
	}
	if args.Provider == "" {
		return PlanUsage{}, fmt.Errorf("Provider is required")
	}
	now := args.Now
	if now.IsZero() {
		now = time.Now()
	}
	client := args.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	switch args.Provider {
	case ProviderClaude:
		return queryClaudePlanUsage(ctx, client, args, now)
	case ProviderCodex:
		return queryCodexPlanUsage(ctx, client, args, now)
	case ProviderGrok:
		return unavailablePlan(ProviderGrok, now,
			"SuperGrok weekly/session remaining has no documented public API; "+
				"use TUI /usage or grok.com Settings → Usage (see docs/grok-usage-billing.md)"), nil
	case ProviderBedrock:
		return unavailablePlan(ProviderBedrock, now,
			"AWS Bedrock does not publish Claude-style session/weekly subscription remaining; "+
				"account quotas and spend limits are managed in AWS, not via this surface"), nil
	default:
		return PlanUsage{}, fmt.Errorf("unknown provider %q", args.Provider)
	}
}

// QueryAllPlanUsage returns plan usage for every supported provider
// (or the Providers list when set). Individual backends that fail with a
// hard transport error still return an unavailable snapshot with Reason set
// rather than inventing numbers; only programmer errors (nil args) fail.
func QueryAllPlanUsage(ctx context.Context, args *AllPlanUsageArgs) ([]PlanUsage, error) {
	if args == nil {
		args = &AllPlanUsageArgs{}
	}
	providers := args.Providers
	if len(providers) == 0 {
		providers = []Provider{ProviderClaude, ProviderCodex, ProviderGrok, ProviderBedrock}
	}
	out := make([]PlanUsage, 0, len(providers))
	for _, p := range providers {
		pu, err := QueryPlanUsage(ctx, &PlanUsageArgs{
			Provider:          p,
			HTTPClient:        args.HTTPClient,
			ClaudeAccessToken: args.ClaudeAccessToken,
			CodexAccessToken:  args.CodexAccessToken,
			CodexAccountID:    args.CodexAccountID,
			CodexAuthPath:     args.CodexAuthPath,
			ClaudeUsageURL:    args.ClaudeUsageURL,
			CodexUsageURL:     args.CodexUsageURL,
			Now:               args.Now,
		})
		if err != nil {
			// Programmer / unknown-provider errors propagate.
			return nil, err
		}
		out = append(out, pu)
	}
	return out, nil
}

func unavailablePlan(p Provider, now time.Time, reason string) PlanUsage {
	return PlanUsage{
		Provider:  p,
		Status:    PlanUsageUnavailable,
		Reason:    reason,
		FetchedAt: now,
	}
}

func remainingFromUsed(used float64) float64 {
	r := 100 - used
	if r < 0 {
		return 0
	}
	if r > 100 {
		return 100
	}
	return r
}

func floatPtr(v float64) *float64 { return &v }

// classifyCodexWindow maps Codex limit_window_seconds onto session/weekly
// when the duration matches known product windows; otherwise returns a
// synthetic name so we still surface published data without inventing labels.
func classifyCodexWindow(limitWindowSeconds int) PlanWindowName {
	switch {
	case limitWindowSeconds <= 0:
		return PlanWindowSession
	case limitWindowSeconds <= 6*3600:
		// ~5h rolling session window.
		return PlanWindowSession
	case limitWindowSeconds >= 6*24*3600:
		// ~7d weekly window.
		return PlanWindowWeekly
	default:
		return PlanWindowName(fmt.Sprintf("%ds", limitWindowSeconds))
	}
}
