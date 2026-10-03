// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/omp"
)

func TestT171LiteralSubscriptionProviderBoundary(t *testing.T) {
	for _, id := range omp.PlanIDs {
		t.Run(id, func(t *testing.T) {
			fake := startSeatFake(t)
			captureStdout(t, func() error {
				return grantCmd([]string{"--name", "t171-literal", "--provider", id, "--model", "explicit-model", "--timeout", "2s", "--json"})
			})
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.grant == nil {
				t.Fatal("no grant reached broker")
			}
			def, err := claudia.DecodeGrantDefinition(fake.grant.Def)
			if err != nil {
				t.Fatal(err)
			}
			if string(def.Provider) != id || def.Model != "explicit-model" || fake.grant.Pick != "" {
				t.Fatalf("literal routing changed: %+v", def)
			}
		})
	}
	fake := startSeatFake(t)
	if err := grantCmd([]string{"--name", "t171-unknown", "--provider", "unknown-provider"}); err == nil {
		t.Fatal("unknown provider accepted")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.ops) != 0 {
		t.Fatalf("unknown provider reached broker: %v", fake.ops)
	}
	for _, id := range []string{"claude", "codex", "grok", "cursor", "bedrock", "ollama"} {
		if err := checkProvider(id); err != nil {
			t.Fatalf("legacy provider %s: %v", id, err)
		}
	}
}
