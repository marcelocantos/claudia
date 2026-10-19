// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package testhome moves a test binary off the owner's home directory before
// any test runs. A test package imports it for effect, from a _test.go file:
//
//	import _ "github.com/marcelocantos/claudia/internal/testhome"
//
// Claudia resolves its state from $XDG_STATE_HOME, falling back to
// $HOME/.local/state/claudia, and so does everything it starts. On 2026-10-19
// two plain `go test ./...` runs wrote ~190 fixture chain files into the
// running broker's chains directory, and appended to dozens more left there by
// earlier runs; a Jevons gate did the same from its own test binaries. Each
// helper that took care to set XDG_STATE_HOME covered its own test and no
// other, so the default path stayed reachable from every test that forgot.
//
// The init below points HOME and the XDG base directories at a fresh private
// directory, so the default resolves there for this process and every child
// it starts. That directory lives under the user cache directory
// (~/Library/Caches/claudia-testhome on macOS), not under /tmp or
// /private/var/folders: the sidecar's seatbelt always grants those, so a
// HOME inside them would make "a write outside the workspace" writable and
// the sidecar's own confinement tests would have nothing to deny. A test
// binary cannot clean up after m.Run without a TestMain, so each run removes
// the directories of runs whose process has exited. The go, bun and npm caches and the git identity keep their real
// locations: they are caches and configuration, not claudia state, and a
// test that shells out to `go list` must not download the module graph again.
//
// A live run (any CLAUDIA_*_LIVE* variable set, which covers every name in
// testctlenv.LiveGates) is left alone: it means to reach the owner's
// providers and sidecar. The ratchet in this package's tests requires every
// test binary in the module to link this one.
package testhome

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// AccountHomeEnv carries the home directory the process started with, for
// the rare test that must read (never write) something only the real
// account has, such as its login keychain.
const AccountHomeEnv = "CLAUDIA_TEST_ACCOUNT_HOME"

// Dir is the private home this process runs under, or "" in a live run.
var Dir string

func init() {
	if !testing.Testing() {
		panic("testhome is for test binaries only; it rewrites HOME")
	}
	if LiveRun(os.Environ()) {
		return
	}
	home := os.Getenv("HOME")
	dir, err := privateHome("claudia-testhome")
	if err != nil {
		panic(err)
	}

	// Pin each cache and config to the location it resolves to now, before
	// HOME moves, unless the caller already chose one.
	keep := map[string]string{
		"GOPATH":                filepath.Join(home, "go"),
		"BUN_INSTALL_CACHE_DIR": filepath.Join(home, ".bun", "install", "cache"),
		"npm_config_cache":      filepath.Join(home, ".npm"),
		"GIT_CONFIG_GLOBAL":     filepath.Join(home, ".gitconfig"),
		AccountHomeEnv:          home,
	}
	if cache, err := os.UserCacheDir(); err == nil {
		keep["GOCACHE"] = filepath.Join(cache, "go-build")
	}
	if config, err := os.UserConfigDir(); err == nil {
		keep["GOENV"] = filepath.Join(config, "go", "env")
	}
	for name, path := range keep {
		if os.Getenv(name) == "" && home != "" {
			_ = os.Setenv(name, path)
		}
	}

	set := map[string]string{
		"HOME":            dir,
		"XDG_STATE_HOME":  filepath.Join(dir, ".local", "state"),
		"XDG_CONFIG_HOME": filepath.Join(dir, ".config"),
		"XDG_DATA_HOME":   filepath.Join(dir, ".local", "share"),
		"XDG_CACHE_HOME":  filepath.Join(dir, ".cache"),
	}
	for name, path := range set {
		_ = os.Setenv(name, path)
	}
	Dir = dir
}

// LiveRun reports whether env (in os.Environ form) opts into a live run. It
// matches by shape rather than importing testctlenv, whose own tests link
// this package; the tests here pin that the shape covers that registry.
func LiveRun(env []string) bool {
	for _, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		if val != "" && strings.HasPrefix(name, "CLAUDIA_") && strings.Contains(name, "_LIVE") {
			return true
		}
	}
	return false
}

// privateHome makes a fresh directory for this process under
// <user cache dir>/<name>, after removing those of processes that have
// exited. Without a user cache dir it falls back to the temp dir.
func privateHome(name string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return os.MkdirTemp("", name+"-")
	}
	base := filepath.Join(cache, name)
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	entries, _ := os.ReadDir(base)
	for _, e := range entries {
		pidText, _, ok := strings.Cut(e.Name(), "-")
		pid, err := strconv.Atoi(pidText)
		if !ok || err != nil || pid <= 0 {
			continue
		}
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			_ = os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
	return os.MkdirTemp(base, fmt.Sprintf("%d-", os.Getpid()))
}
