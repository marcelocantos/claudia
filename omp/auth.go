// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// NoNetworkEnv, when set to any non-empty value, refuses every login/refresh
// call against the OAuth provider. An isolated journey broker sets this so it
// can only ever use whatever unexpired access token its store already holds
// — it never contacts the provider, so it can never rotate (and invalidate)
// the single shared refresh token a production broker also holds (🎯T940).
const NoNetworkEnv = "CLAUDIA_OMP_NO_REFRESH"

func noNetworkLogin() bool {
	return os.Getenv(NoNetworkEnv) != ""
}

// Login runs one provider's existing pi-ai login or refresh.
// The command's stdout is a Record JSON object. A non-zero exit
// does not fall through to an API key, models.yml, or agent.db.
type Login struct {
	// Command is the pinned helper, typically "bun".
	Command string
	// Script is the helper path, typically the sidecar auth script.
	Script string
	Run    Runner
	// ForceLogin runs pi-ai login even when a refresh token exists.
	ForceLogin bool
	// ForceRefresh runs pi-ai refresh even when the access token is still live.
	ForceRefresh bool
}

// Refresh renews one provider and writes that record back. A resync of
// several providers uses fetch and a single Save instead.
func (l Login) Refresh(ctx context.Context, store Store, provider string) (Record, error) {
	if !known(provider) {
		return Record{}, fmt.Errorf("omp: %q is not a subscription provider", provider)
	}
	existing, err := store.Load(ctx)
	if err != nil {
		return Record{}, err
	}
	rec, err := l.fetch(ctx, provider, existing.Records[provider])
	if err != nil {
		return Record{}, err
	}
	if err := store.Put(ctx, provider, rec); err != nil {
		return Record{}, err
	}
	// The refresh spent the refresh token it replaced: save the new one
	// before anything can restart and read the old (🎯T155).
	persist(ctx, store, provider)
	return rec, nil
}

// persist saves the plan store after a refresh. A failure is logged, not
// returned: the refreshed token works in this process, and the next change
// or shutdown tries the save again.
func persist(ctx context.Context, store Store, provider string) {
	if err := Flush(ctx, store); err != nil {
		slog.Warn("omp: refreshed plan login not saved yet; the next save retries", "provider", provider, "err", err)
	}
}

// fetch asks pi-ai for one new record. It does not touch the Keychain.
func (l Login) fetch(ctx context.Context, provider string, existing Record) (Record, error) {
	if !known(provider) {
		return Record{}, fmt.Errorf("omp: %q is not a subscription provider", provider)
	}
	if l.Run == nil {
		return Record{}, fmt.Errorf("omp: no login runner")
	}
	cmd := l.Command
	if cmd == "" {
		cmd = "bun"
	}
	if l.Script == "" {
		return Record{}, fmt.Errorf("omp: login script is required")
	}
	blob, err := json.Marshal(existing)
	if err != nil {
		return Record{}, err
	}
	verb := "refresh"
	if l.ForceLogin || !usableRefresh(existing) {
		verb = "login"
	}
	if noNetworkLogin() {
		return Record{}, fmt.Errorf("omp: %s %s refused: %s is set, this broker never contacts the OAuth provider", provider, verb, NoNetworkEnv)
	}
	out, err := l.Run(ctx, cmd, l.Script, verb, provider, string(blob))
	if err != nil {
		return Record{}, fmt.Errorf("omp: %s %s failed: %w", provider, verb, err)
	}
	var rec Record
	if err := json.Unmarshal(out, &rec); err != nil {
		return Record{}, fmt.Errorf("omp: %s %s returned %s: %w", provider, verb, strings.TrimSpace(string(out)), err)
	}
	if rec.AccessToken == "" || rec.RefreshToken == "" || rec.Expiry.IsZero() {
		return Record{}, fmt.Errorf("omp: %s %s omitted access token, refresh token, or expiry", provider, verb)
	}
	if rec.Expiry.Before(time.Now().Add(-time.Minute)) {
		return Record{}, fmt.Errorf("omp: %s %s returned an already-expired token", provider, verb)
	}
	return rec, nil
}
