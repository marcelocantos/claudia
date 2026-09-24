// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package omp is the subscription-plan seam between the Claudia broker
// and the Oh My Pi sidecar (🎯T864). The broker owns the Keychain item
// and the OAuth refresh. The sidecar never sees an API key.
package omp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	// Anthropic is Claude Pro/Max OAuth. It is not an API key.
	Anthropic = "anthropic"
	// OpenAICodex is ChatGPT Plus/Pro (Codex subscription). OPENAI_API_KEY
	// enables openai/* only, not these models.
	OpenAICodex = "openai-codex"
	// Cursor is Cursor OAuth (PKCE). It is not the Cursor IDE agent CLI.
	Cursor = "cursor"
	// XAIOAuth is SuperGrok device-code login. It is not xai + XAI_API_KEY.
	XAIOAuth = "xai-oauth"

	// KeychainService is the single generic-password item. It is not the
	// jevons pay-as-you-go items openai-api-key and xai-api-key.
	KeychainService = "claudia-plan-credentials"
	keychainAccount = "claudia"
)

// Subscription is a provider id whose model calls go through the sidecar.
// Cursor is not in this set: the Claudia provider id "cursor" is still the
// IDE agent CLI. A seat opts that id onto the sidecar with Config.OMP.
func Subscription(id string) bool {
	switch id {
	case Anthropic, OpenAICodex, XAIOAuth:
		return true
	default:
		return false
	}
}

// Record is one plan login. The Keychain item holds four of these.
type Record struct {
	RefreshToken string    `json:"refresh_token"`
	AccessToken  string    `json:"access_token"`
	Expiry       time.Time `json:"expiry"`
}

// Item is the four records, keyed by provider id.
type Item struct {
	Records map[string]Record `json:"records"`
}

// Runner executes a command. Tests substitute it. Production uses the
// macOS security tool.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Store is the one Keychain item. BrokerPath is the only ACL entry.
type Store struct {
	BrokerPath string
	Run        Runner
	Now        func() time.Time
}

func (s Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Load reads the item. A missing item is an empty Item, not an error
// that falls through to an API key.
func (s Store) Load(ctx context.Context) (Item, error) {
	if s.Run == nil {
		return Item{}, fmt.Errorf("omp: no keychain runner")
	}
	out, err := s.Run(ctx, "security", "find-generic-password",
		"-a", keychainAccount, "-s", KeychainService, "-w")
	if err != nil {
		if isMissing(err, out) {
			return Item{Records: map[string]Record{}}, nil
		}
		return Item{}, err
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return Item{Records: map[string]Record{}}, nil
	}
	var item Item
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		return Item{}, fmt.Errorf("omp: keychain item is not the plan blob: %w", err)
	}
	if item.Records == nil {
		item.Records = map[string]Record{}
	}
	return item, nil
}

// Save writes the blob back. The argv trusts only BrokerPath (-T) and
// never the jevons API-key services.
func (s Store) Save(ctx context.Context, item Item) error {
	if s.BrokerPath == "" {
		return fmt.Errorf("omp: broker path is required for the keychain ACL")
	}
	if s.Run == nil {
		return fmt.Errorf("omp: no keychain runner")
	}
	if item.Records == nil {
		item.Records = map[string]Record{}
	}
	blob, err := json.Marshal(item)
	if err != nil {
		return err
	}
	args := []string{
		"add-generic-password",
		"-U",
		"-a", keychainAccount,
		"-s", KeychainService,
		"-T", s.BrokerPath,
		"-w", string(blob),
	}
	if err := trustedPathOnly(args, s.BrokerPath); err != nil {
		return err
	}
	_, err = s.Run(ctx, "security", args...)
	return err
}

// Put replaces one record and writes the item.
func (s Store) Put(ctx context.Context, provider string, rec Record) error {
	if !known(provider) {
		return fmt.Errorf("omp: %q is not a subscription provider", provider)
	}
	item, err := s.Load(ctx)
	if err != nil {
		return err
	}
	item.Records[provider] = rec
	return s.Save(ctx, item)
}

// AccessToken returns the stored access token when it is still valid.
// It does not refresh and it does not read the environment.
func (s Store) AccessToken(ctx context.Context, provider string) (string, error) {
	item, err := s.Load(ctx)
	if err != nil {
		return "", err
	}
	rec, ok := item.Records[provider]
	if !ok || rec.AccessToken == "" {
		return "", fmt.Errorf("omp: no %s login in the keychain item", provider)
	}
	if !rec.Expiry.IsZero() && !s.now().Before(rec.Expiry) {
		return "", fmt.Errorf("omp: %s login expired", provider)
	}
	return rec.AccessToken, nil
}

func known(provider string) bool {
	switch provider {
	case Anthropic, OpenAICodex, Cursor, XAIOAuth:
		return true
	default:
		return false
	}
}

func trustedPathOnly(args []string, broker string) error {
	trusted := 0
	for i := 0; i < len(args); i++ {
		if args[i] == "-T" && i+1 < len(args) {
			trusted++
			if args[i+1] != broker {
				return fmt.Errorf("omp: ACL entry %q is not the broker", args[i+1])
			}
		}
		if args[i] == "-A" {
			return fmt.Errorf("omp: -A would trust every program")
		}
		if args[i] == "-s" && i+1 < len(args) {
			switch args[i+1] {
			case "openai-api-key", "xai-api-key", "Claude Code-credentials":
				return fmt.Errorf("omp: refusing to write service %q", args[i+1])
			}
		}
	}
	if trusted != 1 {
		return fmt.Errorf("omp: ACL must name exactly one binary, named %d", trusted)
	}
	return nil
}

func isMissing(err error, out []byte) bool {
	blob := strings.ToLower(err.Error() + " " + string(out))
	return strings.Contains(blob, "could not be found") || strings.Contains(blob, "item not found")
}

// ScrubEnv removes plan tokens and the pay-as-you-go key variables from
// a sidecar environment. The access token travels in the IPC message,
// not in the environment.
func ScrubEnv(env []string) []string {
	drop := map[string]bool{
		"ANTHROPIC_API_KEY":   true,
		"OPENAI_API_KEY":      true,
		"XAI_API_KEY":         true,
		"CURSOR_ACCESS_TOKEN": true,
		"OMP_ACCESS_TOKEN":    true,
		"OMP_REFRESH_TOKEN":   true,
	}
	var out []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if drop[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}
