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
	// Limits carries the server's own list, including the per-model
	// weekly windows that have no top-level key (🎯T86).
	Limits []claudeOAuthLimit `json:"limits"`
}

// claudeOAuthLimit is one entry of the response's limits[] array. The
// per-model windows live only here: kind "weekly_scoped", with the model
// named by the server rather than by a codename we would have to guess.
type claudeOAuthLimit struct {
	Kind     string   `json:"kind"`
	Group    string   `json:"group"`
	Percent  *float64 `json:"percent"`
	ResetsAt *string  `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			ID          *string `json:"id"`
			DisplayName string  `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

// claudeScopedWeeklyKind is the limits[] kind that carries a per-model
// weekly allowance. Anything else in that array duplicates a window this
// parser already has from a top-level key.
const claudeScopedWeeklyKind = "weekly_scoped"

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
	pu.Windows = append(pu.Windows, claudeModelWindows(raw.Limits)...)
	if len(pu.Windows) > 0 {
		pu.Status = PlanUsageAvailable
		pu.Reason = ""
	}
	return pu, nil
}

// claudeModelWindows lifts the per-model weekly allowances out of
// limits[] (🎯T86).
//
// These have no top-level key and no fixed name: the account's premium
// model is whatever it is, and the server labels the bucket for us. That
// is why the label is taken from the payload rather than mapped from a
// codename — an earlier reading of this surface guessed the wrong field
// from a string table in the CLI, and a guess here would refuse work on
// a model that is not actually spent.
func claudeModelWindows(limits []claudeOAuthLimit) []PlanWindow {
	var out []PlanWindow
	for _, l := range limits {
		if l.Kind != claudeScopedWeeklyKind || l.Percent == nil {
			continue
		}
		if l.Scope == nil || l.Scope.Model == nil {
			continue
		}
		name := strings.TrimSpace(l.Scope.Model.DisplayName)
		if name == "" {
			// A scoped window whose model has no name cannot be
			// attributed, and an unattributed per-model figure is worse
			// than none: it would read as the plan's own.
			continue
		}
		used := *l.Percent
		w := PlanWindow{
			Name:             PlanWindowModelWeekly,
			Model:            name,
			UsedPercent:      floatPtr(used),
			RemainingPercent: floatPtr(remainingFromUsed(used)),
			LimitWindow:      7 * 24 * time.Hour,
		}
		if l.ResetsAt != nil && strings.TrimSpace(*l.ResetsAt) != "" {
			if t, err := time.Parse(time.RFC3339Nano, *l.ResetsAt); err == nil {
				w.ResetsAt = &t
			} else if t, err := time.Parse(time.RFC3339, *l.ResetsAt); err == nil {
				w.ResetsAt = &t
			}
		}
		out = append(out, w)
	}
	return out
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
