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
	"github.com/marcelocantos/claudia/daemon"
	"github.com/marcelocantos/claudia/internal/broker"
)

const (
	overrideSubdir     = "usage-fixture-override"
	overrideSocketName = "control.sock"
	overrideCapability = "capability"
)

func parseUsageOverrideProviders(raw string) ([]claudia.Provider, error) {
	parts := strings.Split(raw, ",")
	out := make([]claudia.Provider, 0, len(parts))
	seen := map[claudia.Provider]bool{}
	for _, part := range parts {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		p := claudia.Provider(name)
		switch p {
		case claudia.ProviderClaude, claudia.ProviderCodex, claudia.ProviderGrok, claudia.ProviderCursor, claudia.ProviderBedrock:
		default:
			return nil, fmt.Errorf("usage override: unknown provider %q", name)
		}
		if seen[p] {
			return nil, fmt.Errorf("usage override: duplicate provider %q", name)
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("usage override: empty provider allowlist")
	}
	return out, nil
}

func productionBrokerSocket() (string, error) {
	dir, err := broker.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "broker.sock"), nil
}

func confinedTo(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}

func supervisorContext() bool {
	for _, key := range []string{"SUPERVISOR_ENABLED", "SUPERVISOR_SERVER_URL", "SUPERVISOR_PROCESS_NAME"} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return true
		}
	}
	return false
}

func prepareUsageOverride(stateDir, socket string, consumer daemon.Options) (string, error) {
	if supervisorContext() {
		return "", fmt.Errorf("usage override refuses supervisor context")
	}
	if err := validateFixtureDir(stateDir); err != nil {
		return "", fmt.Errorf("usage override: %w", err)
	}
	prodSock, err := productionBrokerSocket()
	if err != nil {
		return "", err
	}
	forced := filepath.Join(stateDir, "broker.sock")
	if socket == "" {
		socket = forced
	}
	if filepath.Clean(socket) == filepath.Clean(prodSock) {
		return "", fmt.Errorf("usage override refuses production socket")
	}
	if !confinedTo(socket, stateDir) {
		return "", fmt.Errorf("usage override refuses socket outside private state directory")
	}
	if env := strings.TrimSpace(os.Getenv(broker.SocketPathEnv)); env != "" {
		if filepath.Clean(env) == filepath.Clean(prodSock) || !confinedTo(env, stateDir) {
			return "", fmt.Errorf("usage override refuses broker socket environment override")
		}
	}
	for _, seed := range consumer.MCPSeedStateDirs {
		if seed == "" || !filepath.IsAbs(seed) || !confinedTo(seed, stateDir) {
			return "", fmt.Errorf("usage override refuses MCP seed outside private state")
		}
		st, err := os.Lstat(seed)
		if err != nil {
			return "", fmt.Errorf("usage override: MCP seed: %w", err)
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("usage override refuses symlink %s", seed)
		}
	}
	if consumer.ConsumerStateDir != "" {
		if !filepath.IsAbs(consumer.ConsumerStateDir) || !confinedTo(consumer.ConsumerStateDir, stateDir) {
			return "", fmt.Errorf("usage override refuses consumer state escape")
		}
	}
	overrideDir := filepath.Join(stateDir, overrideSubdir)
	if err := os.MkdirAll(overrideDir, 0o700); err != nil {
		return "", err
	}
	st, err := os.Lstat(overrideDir)
	if err != nil {
		return "", err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("usage override refuses symlink %s", overrideDir)
	}
	if st.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("usage override state directory must be mode 0700")
	}
	if !ownedByCurrentUser(st) {
		return "", fmt.Errorf("usage override directory must be owned by current user")
	}
	return socket, nil
}

func startUsageOverride(dir string) (func(), error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	overrideDir := filepath.Join(dir, overrideSubdir)
	capPath := filepath.Join(overrideDir, overrideCapability)
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
	path := filepath.Join(overrideDir, overrideSocketName)
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
		if err := claudia.StoreBrokerUsageOverride(snapshot.Backends); err != nil {
			http.Error(w, err.Error(), 400)
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
	return func() {
		_ = srv.Shutdown(context.Background())
		_ = os.Remove(path)
		_ = os.Remove(capPath)
	}, nil
}
