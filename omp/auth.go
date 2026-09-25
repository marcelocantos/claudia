// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

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
}

// Refresh renews provider and writes the record back to the Keychain
// item. The seat must not start when this returns an error.
func (l Login) Refresh(ctx context.Context, store Store, provider string) (Record, error) {
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
	existing, err := store.Load(ctx)
	if err != nil {
		return Record{}, err
	}
	blob, err := json.Marshal(existing.Records[provider])
	if err != nil {
		return Record{}, err
	}
	verb := "refresh"
	if rec, ok := existing.Records[provider]; l.ForceLogin || !ok || !usableRefresh(rec) {
		verb = "login"
	}
	out, err := l.Run(ctx, cmd, l.Script, verb, provider, string(blob))
	if err != nil {
		return Record{}, fmt.Errorf("omp: %s refresh failed: %w", provider, err)
	}
	var rec Record
	if err := json.Unmarshal(out, &rec); err != nil {
		return Record{}, fmt.Errorf("omp: %s refresh returned %s: %w", provider, strings.TrimSpace(string(out)), err)
	}
	if rec.AccessToken == "" || rec.RefreshToken == "" || rec.Expiry.IsZero() {
		return Record{}, fmt.Errorf("omp: %s refresh omitted access token, refresh token, or expiry", provider)
	}
	if rec.Expiry.Before(time.Now().Add(-time.Minute)) {
		return Record{}, fmt.Errorf("omp: %s refresh returned an already-expired token", provider)
	}
	if err := store.Put(ctx, provider, rec); err != nil {
		return Record{}, err
	}
	return rec, nil
}
