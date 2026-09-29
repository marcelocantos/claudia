// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The sidecar's scripts import packages that live in node_modules beside
// them, and a checkout has none until `bun install` runs: a fresh clone, a
// clean-checkout gate, a CI runner, or a pull that changed the lockfile.
// Started without them, bun dies on the first import and the caller waits
// out its whole ready deadline for a sidecar that could never come up
// (🎯T156). So whatever starts a sidecar script first installs what that
// script's lockfile names, once, and fails at once, naming the install,
// when it cannot.
const (
	// depsStamp records which manifest and lockfile node_modules was
	// installed from.
	depsStamp = ".claudia-deps"
	// depsLock serialises installs across processes and goroutines.
	depsLock = ".claudia-deps.lock"
	// depsInstallTimeout bounds one install. A cold one downloads the
	// platform's native addon (~170 MB); a warm one copies from bun's cache.
	depsInstallTimeout = 10 * time.Minute
)

// depsManifests are the files an install is derived from, in hash order.
var depsManifests = []string{"package.json", "bun.lockb", "bun.lock"}

// depsInstall runs the install in dir. Tests replace it.
var depsInstall = func(ctx context.Context, dir string) ([]byte, error) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		return nil, fmt.Errorf("bun is required for the sidecar: %w", err)
	}
	cmd := exec.CommandContext(ctx, bun, "install", "--frozen-lockfile")
	cmd.Dir = dir
	cmd.Env = ScrubEnv(os.Environ())
	return cmd.CombinedOutput()
}

// EnsureDeps installs the dependencies of the package holding script when
// node_modules is missing or was installed from another manifest or
// lockfile. A script with no package.json beside it (a self-contained
// bundle) needs nothing.
func EnsureDeps(ctx context.Context, script string) error {
	dir := filepath.Dir(script)
	want, err := depsHash(dir)
	if err != nil || want == "" {
		return err
	}
	modules := filepath.Join(dir, "node_modules")
	stamp := filepath.Join(modules, depsStamp)
	if depsCurrent(stamp, want) {
		return nil
	}
	if err := os.MkdirAll(modules, 0o755); err != nil {
		return fmt.Errorf("omp: sidecar dependencies in %s: %w", dir, err)
	}
	lock, err := os.OpenFile(filepath.Join(modules, depsLock), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("omp: sidecar dependencies in %s: %w", dir, err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("omp: sidecar dependencies in %s: lock: %w", dir, err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck // Close releases it too
	// Another process may have installed while this one waited.
	if depsCurrent(stamp, want) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, depsInstallTimeout)
	defer cancel()
	out, err := depsInstall(ctx, dir)
	if err != nil {
		return fmt.Errorf("omp: installing sidecar dependencies in %s (bun install --frozen-lockfile): %w: %s",
			dir, err, strings.TrimSpace(string(out)))
	}
	tmp := stamp + ".tmp"
	if err := os.WriteFile(tmp, []byte(want+"\n"), 0o644); err != nil {
		return fmt.Errorf("omp: sidecar dependencies in %s: %w", dir, err)
	}
	if err := os.Rename(tmp, stamp); err != nil {
		return fmt.Errorf("omp: sidecar dependencies in %s: %w", dir, err)
	}
	return nil
}

// depsHash identifies the install dir's manifests call for, or "" when dir
// holds no package.json.
func depsHash(dir string) (string, error) {
	h := sha256.New()
	for i, name := range depsManifests {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			if i == 0 {
				return "", nil
			}
			continue
		}
		if err != nil {
			return "", fmt.Errorf("omp: sidecar dependencies: %w", err)
		}
		fmt.Fprintf(h, "%s %d\n", name, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func depsCurrent(stamp, want string) bool {
	b, err := os.ReadFile(stamp)
	return err == nil && string(bytes.TrimSpace(b)) == want
}
