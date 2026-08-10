// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// grokUsageEnv opts into the undocumented Grok plan-usage surface.
const grokUsageEnv = "CLAUDIA_GROK_USAGE"

// grokBillingURL is the undocumented Grok Build billing endpoint the CLI's own
// /usage panel reads (observed from the grok binary + a captured request). It is
// private and unversioned — the whole reason this path is opt-in.
const grokBillingURL = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"

// grokBillingConfig mirrors the /billing?format=credits response — an
// UNDOCUMENTED, unversioned Grok private surface (shape captured 2026-08-10).
// Only the fields this maps are declared; unknown fields (productUsage,
// onDemand*, prepaidBalance, …) are ignored. Grok publishes only the weekly
// SuperGrok pool here — there is no rolling session window.
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
		BillingPeriodEnd string `json:"billingPeriodEnd"`
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
	if r.Config.CreditUsagePercent == nil {
		return unavailablePlan(ProviderGrok, now,
			"grok billing: response carries no creditUsagePercent (private surface may have changed)")
	}
	used := *r.Config.CreditUsagePercent
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
	return PlanUsage{
		Provider:  ProviderGrok,
		Status:    PlanUsageAvailable,
		Windows:   []PlanWindow{w},
		PlanType:  r.SubscriptionTier,
		FetchedAt: now,
	}
}

// queryGrokPlanUsage returns SuperGrok weekly plan remaining from the
// undocumented Grok billing endpoint. It is OPT-IN: the surface is private and
// unversioned, so without the opt-in it stays unavailable with a reason. Any
// transport or parse failure returns unavailable — never a fabricated number.
func queryGrokPlanUsage(ctx context.Context, args *PlanUsageArgs, now time.Time) PlanUsage {
	// Test injection: parse a captured response without any network call.
	if len(args.GrokBillingRaw) > 0 {
		return parseGrokBilling(args.GrokBillingRaw, now)
	}
	if !args.GrokUnstableUsage && os.Getenv(grokUsageEnv) != "1" {
		return unavailablePlan(ProviderGrok, now,
			"grok plan usage is opt-in — it reads the undocumented "+grokBillingURL+" endpoint. "+
				"Set "+grokUsageEnv+"=1 (or PlanUsageArgs.GrokUnstableUsage=true) to enable; it may "+
				"break on any grok update, in which case this reports unavailable, never a wrong number.")
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
// token. A non-200 (e.g. 401 on an expired token) is a loud error, not a guess.
func fetchGrokBilling(ctx context.Context, args *PlanUsageArgs) ([]byte, error) {
	tok := args.GrokAccessToken
	if tok == "" {
		t, err := loadGrokToken(args.GrokAuthPath)
		if err != nil {
			return nil, err
		}
		tok = t
	}
	url := args.GrokBillingURL
	if url == "" {
		url = grokBillingURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d (token may be expired — run `grok login`)", resp.StatusCode)
	}
	return body, nil
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
