// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSeatLegacyConfigRefusedBeforeTransport(t *testing.T) {
	for _, brokerMode := range []string{"0", "1"} {
		t.Run("no-broker="+brokerMode, func(t *testing.T) {
			t.Setenv("CLAUDIA_NO_BROKER", brokerMode)
			t.Setenv("CLAUDIA_BROKER_SOCKET", t.TempDir()+"/absent.sock")
			for _, p := range []Provider{"", ProviderClaude, "anthropic", ProviderCodex, "openai-codex", ProviderGrok, "xai-oauth", ProviderCursor} {
				for _, tc := range []struct {
					name string
					set  func(*Config)
				}{
					{"PermissionMode", func(c *Config) { c.PermissionMode = "plan" }},
					{"DisallowTools", func(c *Config) { c.DisallowTools = []string{"Bash"} }},
					{"ExtraArgs", func(c *Config) { c.ExtraArgs = []string{"--verbose"} }},
					{"MCPConfig", func(c *Config) { c.MCPConfig = "/missing.json" }},
					{"ConnectURL", func(c *Config) { c.ConnectURL = "ws://unused" }},
					{"ConnectPID", func(c *Config) { c.ConnectPID = 1 }},
					{"GrokConnect", func(c *Config) { c.GrokConnect = true }},
					{"TermLogPath", func(c *Config) { c.TermLogPath = "/unused.term" }},
				} {
					t.Run(string(p)+"/"+tc.name, func(t *testing.T) {
						cfg := Config{Provider: p, Name: "refused", SessionID: "generation", WorkDir: t.TempDir()}
						tc.set(&cfg)
						for _, start := range []func(Config) (*Agent, error){Start, StartDirect, Adopt, func(c Config) (*Agent, error) { return startViaBrokerContext(context.Background(), c) }} {
							_, err := start(cfg)
							var capErr *CapabilityError
							if !errors.As(err, &capErr) || !strings.Contains(err.Error(), tc.name) {
								t.Fatalf("wanted named refusal for %s, got %v", tc.name, err)
							}
						}
					})
				}
			}
		})
	}
}

func TestSeatAliasResumeFence(t *testing.T) {
	for _, p := range []Provider{"", ProviderClaude, "anthropic", ProviderCodex, "openai-codex", ProviderGrok, "xai-oauth", ProviderCursor} {
		t.Run(string(p), func(t *testing.T) {
			path := t.TempDir() + "/registry.json"
			r, err := NewRegistry(path)
			if err != nil {
				t.Fatal(err)
			}
			d := AgentDef{Name: "seat", SessionID: "generation", Provider: p}
			if err := r.Register(d); err != nil {
				t.Fatal(err)
			}
			if r.requireResumeLocked(&d) {
				t.Fatal("fresh registration requires nonexistent history")
			}
			restored, err := NewRegistry(path)
			if err != nil {
				t.Fatal(err)
			}
			if !restored.requireResumeLocked(&d) {
				t.Fatal("persisted generation can silently remint")
			}
			if err := restored.MarkMaterialized("seat"); err != nil {
				t.Fatalf("host attestation incorrectly requires CLI JSONL: %v", err)
			}
		})
	}
}

func TestSeatRequiredResumeAcknowledgement(t *testing.T) {
	f := startOMPFixture(t)
	cfg := Config{Name: "required", SessionID: "generation", Provider: ProviderClaude, WorkDir: t.TempDir(), TermLogPath: "-", RequireResume: true}
	for _, absent := range []bool{true, false} {
		f.noRestoreEcho.Store(absent)
		f.unrestored.Store(true)
		if a, err := StartDirect(cfg); err == nil {
			a.Stop()
			t.Fatal("required resume accepted missing or false restoration acknowledgement")
		} else if !strings.Contains(err.Error(), "did not confirm restoration") {
			t.Fatalf("unexpected refusal: %v", err)
		}
	}
	f.unrestored.Store(false)
	a, err := StartDirect(cfg)
	if err != nil {
		t.Fatal("affirmative restoration:", err)
	}
	a.Stop()
	// Adoption attaches to a matching in-memory generation without recreating
	// it; it does not need to claim an on-disk restoration.
	f.noRestoreEcho.Store(true)
	a, err = Adopt(cfg)
	if err != nil {
		t.Fatal("matching adoption:", err)
	}
	a.Stop()
}
