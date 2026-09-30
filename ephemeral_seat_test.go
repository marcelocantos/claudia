// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEphemeralSeatDefConvention(t *testing.T) {
	smoke, err := EphemeralSeatDef(EphemeralSmoke, "run-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if smoke.Name != "pimp-smoke-run-1" || smoke.Parent != ParentPimp || smoke.Purpose != PurposeWork {
		t.Fatalf("smoke def = %+v", smoke)
	}
	if smoke.SessionID == "" {
		t.Fatal("session id was not minted")
	}
	root, err := filepath.Abs(EphemeralWorkRoot())
	if err != nil {
		t.Fatal(err)
	}
	if !underDir(smoke.WorkDir, root) {
		t.Fatalf("workdir %q is not under %s", smoke.WorkDir, root)
	}

	handoff, err := EphemeralSeatDef(EphemeralHandoff, "h1", PurposeAside)
	if err != nil {
		t.Fatal(err)
	}
	if handoff.Name != "pimp-handoff-h1" || handoff.Purpose != PurposeAside || handoff.Parent != ParentPimp {
		t.Fatalf("handoff def = %+v", handoff)
	}

	overseer, err := EphemeralSeatDef(EphemeralSmoke, "eye", PurposeOverseer)
	if err != nil || overseer.Purpose != PurposeOverseer {
		t.Fatalf("overseer = %+v err=%v", overseer, err)
	}

	if _, err := EphemeralSeatDef(EphemeralSmoke, "", PurposeWork); err == nil {
		t.Fatal("empty id was accepted")
	}
	if _, err := EphemeralSeatDef(EphemeralSmoke, "a/b", PurposeWork); err == nil {
		t.Fatal("id with a slash was accepted")
	}
	if _, err := EphemeralSeatDef(EphemeralSmoke, "..", PurposeWork); err == nil {
		t.Fatal("id .. was accepted")
	}
	if _, err := EphemeralSeatDef(EphemeralKind("pool"), "1", PurposeWork); err == nil {
		t.Fatal("unknown kind was accepted")
	}
	if _, err := EphemeralSeatDef(EphemeralSmoke, "1", "pimp-smoke"); err == nil {
		t.Fatal("purpose pimp-smoke was accepted; purpose stays work|aside|overseer")
	}
}

func TestEphemeralNameRequiresSuffix(t *testing.T) {
	if EphemeralSeatName("pimp-smoke") || EphemeralSeatName("pimp-handoff") || EphemeralSeatName("jevons-po") {
		t.Fatal("a name without a suffix, or a jevons name, is not a plumbing seat")
	}
	if !EphemeralSeatName("pimp-smoke-1") || !EphemeralSeatName("pimp-handoff-abc") {
		t.Fatal("prefixed names with an id should match")
	}
	def := AgentDef{Name: "pimp-smoke", Parent: ParentPimp, Purpose: PurposeWork}
	if IsEphemeralGrant(def) {
		t.Fatal("parent pimp with a non-prefixed name is a long-lived grant")
	}
	if err := PrepareEphemeralGrant(&def); err != nil {
		t.Fatal(err)
	}
	if def.WorkDir != "" || def.Parent != ParentPimp {
		t.Fatalf("non-plumbing def was rewritten: %+v", def)
	}
}

func TestPrepareEphemeralGrant(t *testing.T) {
	jevons := AgentDef{Name: "jevons-po", Parent: "jevons", Purpose: PurposeOverseer, WorkDir: "/srv/jevons"}
	if err := PrepareEphemeralGrant(&jevons); err != nil {
		t.Fatal(err)
	}
	if jevons.WorkDir != "/srv/jevons" || jevons.Parent != "jevons" {
		t.Fatalf("jevons def changed: %+v", jevons)
	}

	foreign := AgentDef{Name: "pimp-smoke-x", Parent: "jevons-po", Purpose: PurposeWork, WorkDir: t.TempDir()}
	if err := PrepareEphemeralGrant(&foreign); err == nil || !strings.Contains(err.Error(), "parent") {
		t.Fatalf("foreign parent err = %v", err)
	}

	badPurpose := AgentDef{Name: "pimp-handoff-x", Parent: ParentPimp, Purpose: "coding"}
	if err := PrepareEphemeralGrant(&badPurpose); err == nil || !strings.Contains(err.Error(), "purpose") {
		t.Fatalf("bad purpose err = %v", err)
	}

	bare := AgentDef{Name: "pimp-smoke-bare", WorkDir: "/workspace/repo"}
	if err := PrepareEphemeralGrant(&bare); err != nil {
		t.Fatal(err)
	}
	if bare.Parent != ParentPimp || bare.Purpose != PurposeWork {
		t.Fatalf("defaults = %+v", bare)
	}
	root, _ := filepath.Abs(EphemeralWorkRoot())
	if !underDir(bare.WorkDir, root) || strings.Contains(bare.WorkDir, "workspace") {
		t.Fatalf("repo workdir was kept: %s", bare.WorkDir)
	}

	scratch := filepath.Join(string(filepath.Separator), "var", "build", "_scratchpad", "seat")
	kept := AgentDef{Name: "pimp-smoke-scratch", Parent: ParentPimp, Purpose: PurposeWork, WorkDir: scratch}
	if err := PrepareEphemeralGrant(&kept); err != nil {
		t.Fatal(err)
	}
	if kept.WorkDir != filepath.Clean(scratch) {
		t.Fatalf("scratch workdir = %s", kept.WorkDir)
	}

	tmp := filepath.Join(os.TempDir(), "pimp-explicit")
	explicit := AgentDef{Name: "pimp-handoff-tmp", Parent: ParentPimp, WorkDir: tmp}
	if err := PrepareEphemeralGrant(&explicit); err != nil {
		t.Fatal(err)
	}
	absTmp, err := filepath.Abs(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if explicit.WorkDir != filepath.Clean(absTmp) || explicit.Purpose != PurposeWork {
		t.Fatalf("temp workdir = %+v", explicit)
	}

	escaped := AgentDef{Name: "pimp-smoke-esc", Parent: ParentPimp, WorkDir: filepath.Join(os.TempDir(), "..", "etc")}
	if err := PrepareEphemeralGrant(&escaped); err != nil {
		t.Fatal(err)
	}
	if !underDir(escaped.WorkDir, root) {
		t.Fatalf("escaped temp path was kept: %s", escaped.WorkDir)
	}
}
