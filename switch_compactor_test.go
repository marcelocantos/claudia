// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"context"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/omp"
)

func TestSwitchModelPassesCompactOnWire(t *testing.T) {
	f := startFakeSidecar(t, []string{`{"type":"text","text":"ok"}`})
	dir := t.TempDir()
	agent, err := StartDirect(Config{
		Name: "sw-compact", WorkDir: dir, Provider: Provider(omp.Anthropic), Model: "claude-opus", TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Stop() })
	if err := agent.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := agent.SwitchModel(ProviderCodex, "gpt-5.5", SwitchOptions{CompactOn: CompactOnDestination}); err != nil {
		t.Fatalf("SwitchModel: %v", err)
	}
	loads := f.loaded()
	if len(loads) < 2 {
		t.Fatalf("loads=%d, want switch load", len(loads))
	}
	last := loads[len(loads)-1]
	if last.CompactOn != "destination" {
		t.Fatalf("CompactOn=%q, want destination", last.CompactOn)
	}
	if last.RequestID == "" {
		t.Fatal("switch load missing request_id")
	}
}

func TestSwitchOptionsValidateExplicit(t *testing.T) {
	if err := (SwitchOptions{CompactOn: CompactOnExplicit}).Validate(); err == nil {
		t.Fatal("expected error for missing explicit model")
	}
}

func TestSwitchOptionsFromWire(t *testing.T) {
	o, err := SwitchOptionsFromWire("destination", "", "")
	if err != nil || o.CompactOn != CompactOnDestination {
		t.Fatalf("destination: o=%+v err=%v", o, err)
	}
	_, err = SwitchOptionsFromWire("bogus", "", "")
	if err == nil {
		t.Fatal("expected error for unknown compact_on")
	}
}

func TestSwitchModelExplicitOnWire(t *testing.T) {
	f := startFakeSidecar(t, nil)
	dir := t.TempDir()
	agent, err := StartDirect(Config{
		Name: "sw-explicit", WorkDir: dir, Provider: Provider(omp.Anthropic), Model: "claude-opus", TermLogPath: "-",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Stop() })
	if err := agent.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- agent.SwitchModel(ProviderCodex, "gpt-5.5", SwitchOptions{
			CompactOn: CompactOnExplicit, CompactProvider: Provider(omp.Anthropic), CompactModel: "claude-opus",
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SwitchModel: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("switch hung")
	}
	last := f.loaded()[len(f.loaded())-1]
	if last.CompactOn != "explicit" || last.CompactProvider != omp.Anthropic || last.CompactModel != "claude-opus" {
		t.Fatalf("load compact fields: %+v", last)
	}
}
