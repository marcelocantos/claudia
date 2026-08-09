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
	"strings"
	"time"
)

const (
	codexWhamUsageURL   = "https://chatgpt.com/backend-api/wham/usage"
	codexAccessTokenEnv = "CLAUDIA_CODEX_ACCESS_TOKEN"
	codexAccountIDEnv   = "CLAUDIA_CODEX_ACCOUNT_ID"
)

// codexWhamUsage is the subset of ChatGPT Codex wham/usage we map.
// Live shape (2026-08): rate_limit.primary_window / secondary_window with
// used_percent, limit_window_seconds, reset_at (unix seconds).
type codexWhamUsage struct {
	PlanType  string          `json:"plan_type"`
	RateLimit *codexRateLimit `json:"rate_limit"`
}

type codexRateLimit struct {
	Allowed         bool              `json:"allowed"`
	LimitReached    bool              `json:"limit_reached"`
	PrimaryWindow   *codexLimitWindow `json:"primary_window"`
	SecondaryWindow *codexLimitWindow `json:"secondary_window"`
}

type codexLimitWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds int      `json:"limit_window_seconds"`
	ResetAfterSeconds  int      `json:"reset_after_seconds"`
	ResetAt            *int64   `json:"reset_at"`
}

type codexAuthFile struct {
	Tokens *struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

func queryCodexPlanUsage(ctx context.Context, client *http.Client, args *PlanUsageArgs, now time.Time) (PlanUsage, error) {
	token := strings.TrimSpace(args.CodexAccessToken)
	accountID := strings.TrimSpace(args.CodexAccountID)
	if token == "" {
		token = strings.TrimSpace(os.Getenv(codexAccessTokenEnv))
	}
	if accountID == "" {
		accountID = strings.TrimSpace(os.Getenv(codexAccountIDEnv))
	}
	if token == "" {
		t, a, err := loadCodexChatGPTTokens(args.CodexAuthPath)
		if err != nil {
			return unavailablePlan(ProviderCodex, now,
				fmt.Sprintf("Codex ChatGPT credentials unavailable: %v", err)), nil
		}
		token = t
		if accountID == "" {
			accountID = a
		}
	}
	if token == "" {
		return unavailablePlan(ProviderCodex, now,
			"Codex access token empty; sign in with Codex CLI or set "+codexAccessTokenEnv), nil
	}

	url := args.CodexUsageURL
	if url == "" {
		url = codexWhamUsageURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PlanUsage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claudia/"+Version)
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}

	resp, err := client.Do(req)
	if err != nil {
		return unavailablePlan(ProviderCodex, now,
			fmt.Sprintf("Codex usage fetch failed: %v", err)), nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return unavailablePlan(ProviderCodex, now,
			fmt.Sprintf("Codex usage read failed: %v", err)), nil
	}
	if resp.StatusCode != http.StatusOK {
		return unavailablePlan(ProviderCodex, now,
			fmt.Sprintf("Codex usage HTTP %d: %s", resp.StatusCode, truncateForReason(body, 200))), nil
	}
	return parseCodexWhamUsage(body, now)
}

func parseCodexWhamUsage(body []byte, now time.Time) (PlanUsage, error) {
	var raw codexWhamUsage
	if err := json.Unmarshal(body, &raw); err != nil {
		return unavailablePlan(ProviderCodex, now,
			fmt.Sprintf("Codex usage JSON invalid: %v", err)), nil
	}

	pu := PlanUsage{
		Provider:  ProviderCodex,
		Status:    PlanUsageUnavailable,
		FetchedAt: now,
		PlanType:  raw.PlanType,
		Reason:    "Codex usage response published no rate-limit windows",
	}
	if raw.RateLimit == nil {
		return pu, nil
	}

	seen := map[PlanWindowName]bool{}
	add := func(w *codexLimitWindow) {
		if w == nil || w.UsedPercent == nil {
			return
		}
		name := classifyCodexWindow(w.LimitWindowSeconds)
		// If both primary and secondary map to the same name, keep the first.
		if seen[name] {
			// Prefer explicit secondary naming when collision: tag with duration.
			if w.LimitWindowSeconds > 0 {
				name = PlanWindowName(fmt.Sprintf("%ds", w.LimitWindowSeconds))
				if seen[name] {
					return
				}
			} else {
				return
			}
		}
		seen[name] = true
		used := *w.UsedPercent
		rem := remainingFromUsed(used)
		pw := PlanWindow{
			Name:             name,
			UsedPercent:      floatPtr(used),
			RemainingPercent: floatPtr(rem),
		}
		if w.LimitWindowSeconds > 0 {
			pw.LimitWindow = time.Duration(w.LimitWindowSeconds) * time.Second
		}
		if w.ResetAt != nil && *w.ResetAt > 0 {
			t := time.Unix(*w.ResetAt, 0).UTC()
			pw.ResetsAt = &t
		}
		pu.Windows = append(pu.Windows, pw)
	}
	add(raw.RateLimit.PrimaryWindow)
	add(raw.RateLimit.SecondaryWindow)

	if len(pu.Windows) > 0 {
		pu.Status = PlanUsageAvailable
		pu.Reason = ""
	}
	return pu, nil
}

func loadCodexChatGPTTokens(authPath string) (accessToken, accountID string, err error) {
	path := authPath
	if path == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", "", fmt.Errorf("home dir: %w", herr)
		}
		path = filepath.Join(home, ".codex", "auth.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", path, err)
	}
	var auth codexAuthFile
	if err := json.Unmarshal(raw, &auth); err != nil {
		return "", "", fmt.Errorf("parse %s: %w", path, err)
	}
	if auth.Tokens == nil || auth.Tokens.AccessToken == "" {
		return "", "", fmt.Errorf("%s has no tokens.access_token (ChatGPT login required)", path)
	}
	return auth.Tokens.AccessToken, auth.Tokens.AccountID, nil
}
