// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Ephemeral plumbing seats are short-lived grants a Pimp process holds.
// The name prefix is the marker (pimp-smoke-<id> or pimp-handoff-<id>).
// Parent is pimp. Purpose stays the fleet enum: work, aside, or overseer.
// A jevons grant is any other name, and the daemon does not put it on
// this clock.

const (
	// ParentPimp is the Parent of an ephemeral plumbing seat.
	ParentPimp = "pimp"

	// EphemeralSmokePrefix starts a smoke seat's name. The id follows the dash.
	EphemeralSmokePrefix = "pimp-smoke-"
	// EphemeralHandoffPrefix starts a handoff seat's name. The id follows the dash.
	EphemeralHandoffPrefix = "pimp-handoff-"
)

// EphemeralKind selects the name prefix of a plumbing seat.
type EphemeralKind string

const (
	EphemeralSmoke   EphemeralKind = "smoke"
	EphemeralHandoff EphemeralKind = "handoff"
)

// EphemeralGrantTTL is how long an unowned plumbing seat stays reclaimable
// by name. A crashed Pimp reconnects inside this window and gets the same
// seat; after it, the daemon stops the process and drops the grant.
// Long-lived grants (jevons and any other name) are not subject to it.
const EphemeralGrantTTL = 2 * time.Minute

// EphemeralSeatDef builds a plumbing seat. id is the name suffix
// (pimp-smoke-<id> or pimp-handoff-<id>). purpose is work, aside, or
// overseer; empty means work. The workdir is under the system temp
// directory, one directory per seat. SessionID is new, so the definition
// can be registered as-is.
func EphemeralSeatDef(kind EphemeralKind, id, purpose string) (AgentDef, error) {
	prefix, err := kind.prefix()
	if err != nil {
		return AgentDef{}, err
	}
	if !validEphemeralID(id) {
		return AgentDef{}, fmt.Errorf("ephemeral seat id %q must be a single path segment", id)
	}
	if purpose == "" {
		purpose = PurposeWork
	}
	if !validPurpose(purpose) {
		return AgentDef{}, fmt.Errorf("ephemeral seat purpose %q is not %q, %q, or %q",
			purpose, PurposeWork, PurposeAside, PurposeOverseer)
	}
	name := prefix + id
	wd, err := IsolateEphemeralWorkDir(name, "")
	if err != nil {
		return AgentDef{}, err
	}
	return AgentDef{
		Name:      name,
		Parent:    ParentPimp,
		Purpose:   purpose,
		WorkDir:   wd,
		SessionID: uuid.NewString(),
	}, nil
}

func (k EphemeralKind) prefix() (string, error) {
	switch k {
	case EphemeralSmoke:
		return EphemeralSmokePrefix, nil
	case EphemeralHandoff:
		return EphemeralHandoffPrefix, nil
	default:
		return "", fmt.Errorf("ephemeral kind %q is not %q or %q", k, EphemeralSmoke, EphemeralHandoff)
	}
}

// EphemeralSeatName reports whether name is a plumbing seat
// (pimp-smoke-<id> or pimp-handoff-<id>). The suffix is required:
// pimp-smoke alone is not one.
func EphemeralSeatName(name string) bool {
	_, _, ok := ephemeralID(name)
	return ok
}

// IsEphemeralGrant reports whether def is a plumbing seat the daemon
// should isolate and reap. A matching name whose parent is empty or pimp
// qualifies. A different parent does not: that grant is refused.
func IsEphemeralGrant(def AgentDef) bool {
	if !EphemeralSeatName(def.Name) {
		return false
	}
	return def.Parent == "" || def.Parent == ParentPimp
}

// PrepareEphemeralGrant applies the plumbing convention to def when its
// name matches. A non-matching name, including a jevons seat, is left
// alone. A matching name with a parent other than pimp, or a purpose
// outside work/aside/overseer, is an error. An empty parent becomes pimp,
// an empty purpose becomes work, and a workdir outside the temp directory
// or a _scratchpad directory is replaced with one under the temp root.
func PrepareEphemeralGrant(def *AgentDef) error {
	if def == nil || !EphemeralSeatName(def.Name) {
		return nil
	}
	if def.Parent != "" && def.Parent != ParentPimp {
		return fmt.Errorf("ephemeral seat %s: parent %q is not %q", def.Name, def.Parent, ParentPimp)
	}
	if !validPurpose(def.Purpose) {
		return fmt.Errorf("ephemeral seat %s: purpose %q is not %q, %q, or %q",
			def.Name, def.Purpose, PurposeWork, PurposeAside, PurposeOverseer)
	}
	wd, err := IsolateEphemeralWorkDir(def.Name, def.WorkDir)
	if err != nil {
		return err
	}
	def.WorkDir = wd
	if def.Parent == "" {
		def.Parent = ParentPimp
	}
	if def.Purpose == "" {
		def.Purpose = PurposeWork
	}
	return nil
}

// EphemeralWorkRoot is the directory plumbing seats land in when their
// requested workdir is not already isolated.
func EphemeralWorkRoot() string {
	return filepath.Join(os.TempDir(), "claudia-pimp")
}

// IsolateEphemeralWorkDir returns the workdir a plumbing seat runs in.
// An empty path or "." becomes EphemeralWorkRoot/name. A path already
// under the system temp directory, or inside a _scratchpad directory,
// is kept. Any other path is replaced with EphemeralWorkRoot/name so the
// seat does not write into a repo.
func IsolateEphemeralWorkDir(name, workDir string) (string, error) {
	workDir = strings.TrimSpace(workDir)
	if workDir != "" && workDir != "." {
		abs, err := filepath.Abs(workDir)
		if err != nil {
			return "", fmt.Errorf("ephemeral seat %s: workdir: %w", name, err)
		}
		abs = filepath.Clean(abs)
		if ephemeralWorkDirOK(abs) {
			return abs, nil
		}
	}
	return defaultEphemeralWorkDir(name)
}

func defaultEphemeralWorkDir(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("ephemeral workdir: name is empty")
	}
	root, err := filepath.Abs(EphemeralWorkRoot())
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	dir := filepath.Join(root, name)
	if !underDir(dir, root) {
		return "", fmt.Errorf("ephemeral seat %s: workdir %q escapes %s", name, dir, root)
	}
	return dir, nil
}

func ephemeralWorkDirOK(abs string) bool {
	tmp, err := filepath.Abs(os.TempDir())
	if err == nil && underDir(abs, filepath.Clean(tmp)) {
		return true
	}
	return hasPathElem(abs, "_scratchpad")
}

func underDir(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(path, root+sep)
}

func hasPathElem(path, elem string) bool {
	for _, p := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if p == elem {
			return true
		}
	}
	return false
}

func ephemeralID(name string) (kind EphemeralKind, id string, ok bool) {
	for _, pre := range []struct {
		prefix string
		kind   EphemeralKind
	}{
		{EphemeralHandoffPrefix, EphemeralHandoff},
		{EphemeralSmokePrefix, EphemeralSmoke},
	} {
		rest, found := strings.CutPrefix(name, pre.prefix)
		if found && validEphemeralID(rest) {
			return pre.kind, rest, true
		}
	}
	return "", "", false
}

func validEphemeralID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	return !strings.ContainsAny(id, `/\`)
}

func validPurpose(p string) bool {
	switch p {
	case "", PurposeWork, PurposeAside, PurposeOverseer:
		return true
	default:
		return false
	}
}
