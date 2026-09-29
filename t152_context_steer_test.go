// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/marcelocantos/claudia/omp"
)

// 🎯T152: a host's compaction steer — what the summary must keep, and facts
// kept verbatim — reaches the Oh My Pi sidecar on the seat's load.
func TestT152HostSteerReachesTheSidecar(t *testing.T) {
	var refreshes atomic.Int32
	s := startT141Sidecar(t)
	t141Plan(t, "live", &refreshes, "")
	agent, err := StartDirect(Config{
		Name: "po", Provider: Provider(omp.Anthropic), Model: "claude-sonnet",
		WorkDir: t.TempDir(), TermLogPath: "-", SessionID: "s1",
		ContextPreserve: "Keep the owner's release policy.",
		ContextPins:     []string{"the owner ships MINOR releases only"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Stop() })
	if err := agent.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	load := s.nextLoad()
	if load.Preserve != "Keep the owner's release policy." || strings.Join(load.Pins, "|") != "the owner ships MINOR releases only" {
		t.Fatalf("load preserve %q pins %q", load.Preserve, load.Pins)
	}
}

// 🎯T152: the steer is part of a seat's definition, so it survives the
// registry and the broker's grant wire.
func TestT152SteerIsCarriedByTheDefinition(t *testing.T) {
	def := AgentDef{Name: "po", ContextPreserve: "keep X", ContextPins: []string{"pin"}}
	cfg := registryConfig(&def, false)
	if cfg.ContextPreserve != "keep X" || len(cfg.ContextPins) != 1 {
		t.Fatalf("registry config lost the steer: %+v", cfg)
	}
	raw, err := EncodeGrantDefinition(configToGrantDef("po", cfg, nil))
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeGrantDefinition(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.ContextPreserve != "keep X" || strings.Join(back.ContextPins, "|") != "pin" {
		t.Fatalf("grant wire lost the steer: %+v", back.AgentDef)
	}
}
