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
	"os/exec"
	"strings"
	"time"
)

const (
	claudeOAuthUsageURL   = "https://api.anthropic.com/api/oauth/usage"
	claudeOAuthBetaHeader = "oauth-2025-04-20"
	claudeOAuthTokenEnv   = "CLAUDIA_CLAUDE_OAUTH_TOKEN"
	claudeKeychainService = "Claude Code-credentials"
)

// claudeOAuthUsage is the subset of GET /api/oauth/usage we map.
// Live shape (2026-08): five_hour / seven_day with utilization + resets_at.
type claudeOAuthUsage struct {
	FiveHour *claudeOAuthWindow `json:"five_hour"`
	SevenDay *claudeOAuthWindow `json:"seven_day"`
}

type claudeOAuthWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

func queryClaudePlanUsage(ctx context.Context, client *http.Client, args *PlanUsageArgs, now time.Time) (PlanUsage, error) {
	token := strings.TrimSpace(args.ClaudeAccessToken)
	if token == "" {
		token = strings.TrimSpace(os.Getenv(claudeOAuthTokenEnv))
	}
	if token == "" {
		t, err := loadClaudeOAuthAccessToken()
		if err != nil {
			return unavailablePlan(ProviderClaude, now,
				fmt.Sprintf("Claude OAuth credentials unavailable: %v", err)), nil
		}
		token = t
	}
	if token == "" {
		return unavailablePlan(ProviderClaude, now,
			"Claude OAuth access token empty; run `claude` login or set "+claudeOAuthTokenEnv), nil
	}

	url := args.ClaudeUsageURL
	if url == "" {
		url = claudeOAuthUsageURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PlanUsage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", claudeOAuthBetaHeader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claudia/"+Version)

	resp, err := client.Do(req)
	if err != nil {
		return unavailablePlan(ProviderClaude, now,
			fmt.Sprintf("Claude usage fetch failed: %v", err)), nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return unavailablePlan(ProviderClaude, now,
			fmt.Sprintf("Claude usage read failed: %v", err)), nil
	}
	if resp.StatusCode != http.StatusOK {
		return unavailablePlan(ProviderClaude, now,
			fmt.Sprintf("Claude usage HTTP %d: %s", resp.StatusCode, truncateForReason(body, 200))), nil
	}
	return parseClaudeOAuthUsage(body, now)
}

// parseClaudeOAuthUsage maps Anthropic's OAuth usage JSON into PlanUsage.
// Exported only for tests via the package-level tests in plan_usage_test.go.
func parseClaudeOAuthUsage(body []byte, now time.Time) (PlanUsage, error) {
	var raw claudeOAuthUsage
	if err := json.Unmarshal(body, &raw); err != nil {
		return unavailablePlan(ProviderClaude, now,
			fmt.Sprintf("Claude usage JSON invalid: %v", err)), nil
	}

	pu := PlanUsage{
		Provider:  ProviderClaude,
		Status:    PlanUsageUnavailable,
		FetchedAt: now,
		Reason:    "Claude OAuth usage response published no session/weekly windows",
	}

	if w := mapClaudeWindow(PlanWindowSession, raw.FiveHour); w != nil {
		pu.Windows = append(pu.Windows, *w)
	}
	if w := mapClaudeWindow(PlanWindowWeekly, raw.SevenDay); w != nil {
		pu.Windows = append(pu.Windows, *w)
	}
	if len(pu.Windows) > 0 {
		pu.Status = PlanUsageAvailable
		pu.Reason = ""
	}
	return pu, nil
}

func mapClaudeWindow(name PlanWindowName, w *claudeOAuthWindow) *PlanWindow {
	if w == nil || w.Utilization == nil {
		return nil
	}
	used := *w.Utilization
	rem := remainingFromUsed(used)
	out := &PlanWindow{
		Name:             name,
		UsedPercent:      floatPtr(used),
		RemainingPercent: floatPtr(rem),
	}
	if w.ResetsAt != nil && strings.TrimSpace(*w.ResetsAt) != "" {
		if t, err := time.Parse(time.RFC3339Nano, *w.ResetsAt); err == nil {
			out.ResetsAt = &t
		} else if t, err := time.Parse(time.RFC3339, *w.ResetsAt); err == nil {
			out.ResetsAt = &t
		}
	}
	switch name {
	case PlanWindowSession:
		out.LimitWindow = 5 * time.Hour
	case PlanWindowWeekly:
		out.LimitWindow = 7 * 24 * time.Hour
	}
	return out
}

// loadClaudeOAuthAccessToken reads the Claude Code OAuth access token from the
// macOS keychain service used by the official CLI. Other platforms return an
// error directing the host to set CLAUDIA_CLAUDE_OAUTH_TOKEN.
func loadClaudeOAuthAccessToken() (string, error) {
	// Prefer security(1) on macOS — same store Claude Code uses.
	if _, err := exec.LookPath("security"); err == nil {
		cmd := exec.Command("security", "find-generic-password", "-s", claudeKeychainService, "-w")
		out, err := cmd.Output()
		if err == nil {
			raw := strings.TrimSpace(string(out))
			if raw == "" {
				return "", fmt.Errorf("empty keychain item %q", claudeKeychainService)
			}
			var creds struct {
				ClaudeAiOauth *struct {
					AccessToken string `json:"accessToken"`
				} `json:"claudeAiOauth"`
			}
			if err := json.Unmarshal([]byte(raw), &creds); err != nil {
				// Some installs store a bare token; accept that too.
				if strings.HasPrefix(raw, "sk-ant-") || strings.HasPrefix(raw, "eyJ") {
					return raw, nil
				}
				return "", fmt.Errorf("parse keychain credentials: %w", err)
			}
			if creds.ClaudeAiOauth != nil && creds.ClaudeAiOauth.AccessToken != "" {
				return creds.ClaudeAiOauth.AccessToken, nil
			}
			return "", fmt.Errorf("keychain item %q has no claudeAiOauth.accessToken", claudeKeychainService)
		}
	}
	return "", fmt.Errorf("no Claude OAuth token (set %s or sign in with Claude Code)", claudeOAuthTokenEnv)
}

func truncateForReason(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
