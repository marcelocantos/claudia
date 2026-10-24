// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// grokBillingURL is the undocumented Grok Build billing endpoint the CLI's own
// /usage panel reads (observed from the grok binary + a captured request). It is
// private and unversioned. A break fails loud (unavailable + reason), never a
// fabricated percent — we fix the parser rather than gate the read.
const grokBillingURL = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"

// grokBillingConfig mirrors the /billing?format=credits response — an
// UNDOCUMENTED, unversioned Grok private surface (shape captured 2026-08-10).
// Only the fields this maps are declared; unknown fields (productUsage,
// prepaidBalance, …) are ignored. Grok publishes only the weekly
// SuperGrok pool here — there is no rolling session window.
// grokWeeklyPeriod is the currentPeriod.type of the weekly SuperGrok pool.
const grokWeeklyPeriod = "USAGE_PERIOD_TYPE_WEEKLY"

// grokMoneyVal is the protobuf-JSON shape for onDemandCap/onDemandUsed
// (and similar) on the private billing surface (🎯T967.2).
type grokMoneyVal struct {
	Val int64 `json:"val"`
}

type grokBillingConfig struct {
	Config struct {
		// CreditUsagePercent is the weekly pool USED percent (0–100), NOT
		// remaining — verified against the CLI panel ("Weekly limit left: 0%"
		// at creditUsagePercent 100). Pointer so absent is distinct from 0.
		CreditUsagePercent *float64 `json:"creditUsagePercent"`
		CurrentPeriod      struct {
			Type  string `json:"type"`
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"currentPeriod"`
		OnDemandCap      *grokMoneyVal `json:"onDemandCap"`
		OnDemandUsed     *grokMoneyVal `json:"onDemandUsed"`
		BillingPeriodEnd string        `json:"billingPeriodEnd"`
	} `json:"config"`
	// SubscriptionTier is present on the ACP variant of this payload; the HTTP
	// endpoint omits it, so PlanType is best-effort.
	SubscriptionTier string `json:"subscription_tier"`
}

// parseGrokBilling maps a /billing response onto a PlanUsage (weekly window
// only; remaining = 100 - creditUsagePercent). It never invents numbers: a
// missing or out-of-range percent yields an explicit unavailable with a reason,
// so a changed surface degrades loudly rather than reporting a wrong figure.
func parseGrokBilling(raw []byte, now time.Time) PlanUsage {
	var r grokBillingConfig
	if err := json.Unmarshal(raw, &r); err != nil {
		return unavailablePlan(ProviderGrok, now, "grok billing: unparseable response: "+err.Error())
	}
	used := 0.0
	switch {
	case r.Config.CreditUsagePercent != nil:
		used = *r.Config.CreditUsagePercent
	case r.Config.CurrentPeriod.Type == grokWeeklyPeriod:
		// A weekly period with nothing spent yet carries no percent at all.
		// Observed 2026-09-19, hours after a period rolled over: the
		// endpoint answered 200 with the weekly descriptor and no percent
		// while Grok's own usage panel read "0% used", resetting at exactly
		// the end this payload carries. Whether the field is dropped as a
		// zero-valued scalar (the payload is protobuf-JSON) or returned as
		// null until the period has entries to sum is not established; both
		// mean the same thing here. It is omitted, not withdrawn: grok CLI
		// 1.0.34 still reads it. The weekly descriptor is the anchor that
		// says this really is the weekly view, so absence means zero rather
		// than a surprise.
		used = 0
	default:
		return unavailablePlan(ProviderGrok, now,
			"grok billing: response carries no creditUsagePercent (private surface may have changed)")
	}
	if used < 0 || used > 100 {
		return unavailablePlan(ProviderGrok, now,
			fmt.Sprintf("grok billing: creditUsagePercent %.2f is outside 0–100 (surface may have changed)", used))
	}
	w := PlanWindow{
		Name:             PlanWindowWeekly,
		UsedPercent:      floatPtr(used),
		RemainingPercent: floatPtr(remainingFromUsed(used)),
		LimitWindow:      7 * 24 * time.Hour,
	}
	end := r.Config.CurrentPeriod.End
	if end == "" {
		end = r.Config.BillingPeriodEnd
	}
	if end != "" {
		if t, err := time.Parse(time.RFC3339, end); err == nil {
			w.ResetsAt = &t
		}
	}
	pu := PlanUsage{
		Provider:  ProviderGrok,
		Status:    PlanUsageAvailable,
		Windows:   []PlanWindow{w},
		PlanType:  r.SubscriptionTier,
		FetchedAt: now,
	}
	pu.Spend = mapGrokOnDemandSpend(r.Config.OnDemandCap, r.Config.OnDemandUsed)
	return pu
}

// mapGrokOnDemandSpend maps Grok on-demand auto top-up spend (🎯T967.2).
// onDemandCap.val == 0 means on-demand is off — no PlanSpend. When cap > 0
// but onDemandUsed is still zero, the unit of val is not yet established
// from a live non-zero spend reading, so nothing is mapped. Once used is
// non-zero, vals are treated as AUD minor units (exponent 2), matching the
// SuperGrok consumer panel's AUD figures (residue until a live non-zero
// owner specimen pins this).
func mapGrokOnDemandSpend(cap, used *grokMoneyVal) *PlanSpend {
	if cap == nil || cap.Val <= 0 {
		return nil
	}
	usedMinor := int64(0)
	if used != nil {
		usedMinor = used.Val
	}
	if usedMinor <= 0 {
		return nil
	}
	limitMinor := cap.Val
	if limitMinor <= 0 {
		return nil
	}
	pct := float64(usedMinor) / float64(limitMinor) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return &PlanSpend{
		Enabled: true,
		Used:    &PlanMoney{AmountMinor: usedMinor, Exponent: 2, Currency: "AUD"},
		Limit:   &PlanMoney{AmountMinor: limitMinor, Exponent: 2, Currency: "AUD"},
		Percent: &pct,
	}
}

// queryGrokPlanUsage returns SuperGrok weekly plan remaining from the
// undocumented Grok billing endpoint. Always fetched. Transport or parse
// failure returns unavailable — never a fabricated number.
func queryGrokPlanUsage(ctx context.Context, args *PlanUsageArgs, now time.Time) PlanUsage {
	// Test injection: parse a captured response without any network call.
	if len(args.GrokBillingRaw) > 0 {
		return parseGrokBilling(args.GrokBillingRaw, now)
	}
	raw, err := fetchGrokBilling(ctx, args)
	if err != nil {
		return unavailablePlan(ProviderGrok, now, "grok billing: "+err.Error())
	}
	return parseGrokBilling(raw, now)
}

// fetchGrokBilling GETs the Grok billing endpoint with the grok login OIDC token
// (from ~/.grok/auth.json), the same way the CLI's /usage panel does. The grok
// agent normally owns this call; we replicate it read-only with the user's own
// token.
//
// 🎯T74: xAI issues a six-hour access token plus a refresh token, and the CLI
// rotates the pair only when it starts. A 401 here therefore usually means
// "the token expired and nothing has run grok since", not "the login is gone".
// A caller-supplied GrokTokenRefresh may rotate and retry once, rate-limited
// to one rotation per grokRefreshWindow per process. A nil refresh does not
// spawn a grok process; it fails with the headless-CLI refusal. A 401 that
// survives a rotation, or a rotation that fails, is a loud error naming the
// attempt. Any other non-200 stays a loud error with no rotation.
func fetchGrokBilling(ctx context.Context, args *PlanUsageArgs) ([]byte, error) {
	tok := args.GrokAccessToken
	brokerSource := tok == "" && args.GrokTokenSource != nil
	if brokerSource {
		var err error
		tok, err = args.GrokTokenSource(ctx)
		if err != nil {
			return nil, fmt.Errorf("broker Grok billing credential unavailable: %w; recover the Grok subscription login in Claudia", err)
		}
		if tok == "" {
			return nil, fmt.Errorf("broker Grok billing credential is empty; recover the Grok subscription login in Claudia")
		}
	}
	if tok == "" {
		t, err := loadGrokToken(args.GrokAuthPath)
		if err != nil {
			return nil, err
		}
		tok = t
	}
	status, body, err := grokBillingGET(ctx, args, tok)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized && !args.GrokRefreshDisabled && !brokerSource {
		now := args.Now
		if now.IsZero() {
			now = time.Now()
		}
		if grokRefreshGate.allow(now) {
			refresh := args.GrokTokenRefresh
			if refresh == nil {
				refresh = defaultGrokTokenRefresh
			}
			if rerr := refresh(ctx); rerr != nil {
				return nil, fmt.Errorf("HTTP 401; token rotation via a grok one-shot failed: %v (run `grok login`)", rerr)
			}
			if args.GrokAccessToken == "" {
				t, lerr := loadGrokToken(args.GrokAuthPath)
				if lerr != nil {
					return nil, fmt.Errorf("HTTP 401; token rotated but auth.json unreadable: %w", lerr)
				}
				tok = t
			}
			status, body, err = grokBillingGET(ctx, args, tok)
			if err != nil {
				return nil, err
			}
			if status != http.StatusOK {
				return nil, fmt.Errorf("HTTP %d after rotating the token (run `grok login`)", status)
			}
			return body, nil
		}
	}
	if status != http.StatusOK {
		if brokerSource && status == http.StatusUnauthorized {
			return nil, fmt.Errorf("HTTP 401: Grok billing rejected the broker subscription credential (agent login health is unchanged)")
		}
		return nil, fmt.Errorf("HTTP %d (token may be expired — run `grok login`)", status)
	}
	return body, nil
}

// grokBillingGET is one GET with the bearer token; the caller reads the status.
func grokBillingGET(ctx context.Context, args *PlanUsageArgs, tok string) (int, []byte, error) {
	url := args.GrokBillingURL
	if url == "" {
		url = grokBillingURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("Content-Type", "application/json")

	client := args.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

// grokRefreshWindow is the least time between two token rotations in one
// process: a persistent 401 must not spend a model call on every poll.
const grokRefreshWindow = 10 * time.Minute

type refreshGate struct {
	mu   sync.Mutex
	last time.Time
}

// allow reports whether a rotation may run now and records it.
func (g *refreshGate) allow(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.last.IsZero() && now.Sub(g.last) < grokRefreshWindow {
		return false
	}
	g.last = now
	return true
}

func (g *refreshGate) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.last = time.Time{}
}

var grokRefreshGate = &refreshGate{}

// errGrokCLIRefreshRemoved is the refusal for a nil GrokTokenRefresh.
// The headless `grok -p` turn that used to rewrite auth.json is gone.
var errGrokCLIRefreshRemoved = errors.New("grok token refresh: headless CLI turn removed")

// defaultGrokTokenRefresh refuses before it looks up a grok binary or
// creates a directory. A consumer that can rotate the token sets
// PlanUsageArgs.GrokTokenRefresh.
func defaultGrokTokenRefresh(context.Context) error {
	return errGrokCLIRefreshRemoved
}

// loadGrokToken reads the grok login access token (the long-lived OIDC "key")
// from ~/.grok/auth.json. auth.json is keyed by an "https://auth.x.ai::<id>"
// entry whose "key" field is the access token. grok owns refreshing it; if it is
// expired the billing GET returns 401 and we report unavailable.
func loadGrokToken(path string) (string, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home for grok auth: %w", err)
		}
		path = filepath.Join(home, ".grok", "auth.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read grok auth (run `grok login`): %w", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("parse grok auth: %w", err)
	}
	for _, v := range m {
		var e struct {
			Key string `json:"key"`
		}
		if json.Unmarshal(v, &e) == nil && len(e.Key) > 200 {
			return e.Key, nil
		}
	}
	return "", fmt.Errorf("no access token in grok auth.json (run `grok login`)")
}
