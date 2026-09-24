// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSaveTrustsOnlyTheBroker(t *testing.T) {
	var got []string
	s := Store{
		BrokerPath: "/usr/local/bin/claudia",
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			got = append([]string{name}, args...)
			return nil, nil
		},
	}
	err := s.Save(context.Background(), Item{Records: map[string]Record{
		Anthropic: {AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(time.Hour)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.Join(got, " ")
	if !strings.Contains(blob, "-T /usr/local/bin/claudia") {
		t.Fatalf("ACL = %s", blob)
	}
	if strings.Contains(blob, "-A") || strings.Contains(blob, "jevonsd") || strings.Contains(blob, "bun") {
		t.Fatalf("ACL trusts more than the broker: %s", blob)
	}
	if strings.Contains(blob, "openai-api-key") || strings.Contains(blob, "xai-api-key") {
		t.Fatalf("wrote a pay-as-you-go item: %s", blob)
	}
	if !strings.Contains(blob, "-s "+KeychainService) {
		t.Fatalf("service = %s", blob)
	}
}

func TestRefreshFailureDoesNotFallThrough(t *testing.T) {
	s := Store{
		BrokerPath: "/usr/local/bin/claudia",
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "security" && len(args) > 0 && args[0] == "find-generic-password" {
				return nil, errors.New("The specified item could not be found in the keychain.")
			}
			t.Fatalf("refresh failure wrote the keychain: %s %v", name, args)
			return nil, nil
		},
	}
	login := Login{
		Script: "auth.ts",
		Run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("oauth refresh rejected")
		},
	}
	_, err := login.Refresh(context.Background(), s, Anthropic)
	if err == nil || !strings.Contains(err.Error(), "refresh failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestScrubEnvDropsPlanKeys(t *testing.T) {
	got := ScrubEnv([]string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk",
		"OPENAI_API_KEY=sk",
		"XAI_API_KEY=sk",
		"CURSOR_ACCESS_TOKEN=tok",
		"HOME=/Users/me",
	})
	blob := strings.Join(got, "\n")
	for _, banned := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY", "CURSOR_ACCESS_TOKEN"} {
		if strings.Contains(blob, banned) {
			t.Fatalf("sidecar env still has %s: %s", banned, blob)
		}
	}
	if !strings.Contains(blob, "PATH=/usr/bin") {
		t.Fatalf("dropped unrelated env: %s", blob)
	}
}
