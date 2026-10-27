// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/internal/broker"
)

const fixtureSocket = "usage-fixture.sock"
const fixtureCapability = "usage-fixture.cap"

// The flag is not usable on an ordinary broker or the default/shared state dir.
// Inspect path components without following symlinks before opening logs or databases.
func validateFixtureDir(dir string) error {
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return fmt.Errorf("usage fixture requires an explicit absolute private state directory")
	}
	normal, err := broker.StateDir()
	if err != nil {
		return err
	}
	if dir == normal || strings.HasPrefix(dir, normal+string(os.PathSeparator)) {
		return fmt.Errorf("usage fixture refuses default broker state directory")
	}
	for p := dir; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("usage fixture refuses symlink %s", p)
		}
		if !st.IsDir() {
			return fmt.Errorf("usage fixture path is not a directory: %s", p)
		}
		if p == dir && st.Mode().Perm() != 0o700 {
			return fmt.Errorf("usage fixture state directory must be mode 0700")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	// os.Geteuid is available on the Unix platforms supporting broker sockets.
	st, _ := os.Stat(dir)
	if !ownedByCurrentUser(st) {
		return fmt.Errorf("usage fixture directory must be owned by current user")
	}
	return filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) || info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("usage fixture refuses non-private or symlink entry %s", path)
		}
		return nil
	})
}

func startUsageFixture(dir string) (func(), error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	capPath := filepath.Join(dir, fixtureCapability)
	f, err := os.OpenFile(capPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	secret := hex.EncodeToString(token)
	if _, err = f.WriteString(secret); err != nil {
		f.Close()
		os.Remove(capPath)
		return nil, err
	}
	if err = f.Close(); err != nil {
		os.Remove(capPath)
		return nil, err
	}
	path := filepath.Join(dir, fixtureSocket)
	// Never unlink a pre-existing socket or symlink: it may belong to another process.
	ln, err := net.Listen("unix", path)
	if err != nil {
		os.Remove(capPath)
		return nil, err
	}
	if err = os.Chmod(path, 0o600); err != nil {
		ln.Close()
		os.Remove(path)
		os.Remove(capPath)
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /usage", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Fixture-Capability")), []byte(secret)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		defer r.Body.Close()
		dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
		dec.DisallowUnknownFields()
		var snapshot struct {
			Backends []claudia.PlanUsage `json:"backends"`
		}
		if err := dec.Decode(&snapshot); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			http.Error(w, "trailing data", 400)
			return
		}
		if err := claudia.CommitBrokerUsageFixture(snapshot.Backends); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"committed_at": time.Now().UTC()})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Shutdown(context.Background()); _ = os.Remove(path); _ = os.Remove(capPath) }, nil
}
