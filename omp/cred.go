// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package omp is the subscription-plan seam between the Claudia broker
// and the Oh My Pi sidecar (🎯T864). The broker owns the Keychain item
// and the OAuth refresh. The sidecar never sees an API key.
package omp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// Anthropic is Claude Pro/Max OAuth. It is not an API key.
	Anthropic = "anthropic"
	// OpenAICodex is ChatGPT Plus/Pro (Codex subscription). OPENAI_API_KEY
	// enables openai/* only, not these models.
	OpenAICodex = "openai-codex"
	// Cursor is Cursor OAuth (PKCE). It is not the Cursor IDE agent CLI.
	Cursor = "cursor"
	// XAIOAuth is SuperGrok device-code login. It is not xai + XAI_API_KEY.
	XAIOAuth = "xai-oauth"

	// KeychainService is the single generic-password item. It is not the
	// jevons pay-as-you-go items openai-api-key and xai-api-key.
	KeychainService = "claudia-plan-credentials"
	keychainAccount = "claudia"
)

// Subscription is a provider id whose model calls go through the sidecar.
// Cursor is in this set: Launch does not need Config.OMP (🎯T866.5).
func Subscription(id string) bool {
	switch id {
	case Anthropic, OpenAICodex, Cursor, XAIOAuth:
		return true
	default:
		return false
	}
}

// Record is one plan login. The Keychain item holds four of these.
type Record struct {
	RefreshToken string    `json:"refresh_token"`
	AccessToken  string    `json:"access_token"`
	Expiry       time.Time `json:"expiry"`
}

// Item is the four records, keyed by provider id.
type Item struct {
	Records map[string]Record `json:"records"`
}

// Runner executes a command. Tests substitute it. Production uses the
// macOS security tool.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// StdinRunner executes a command with stdin. Flush uses it so plan
// JSON is not in argv (🎯T131). Tests substitute it.
type StdinRunner func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)

// Store is the one Keychain item. BrokerPath is the ACL entry Flush
// adds; a retained jevons-broker entry is left in place.
//
// Open reads it once at startup. Load, Save, and Put then use that
// memory copy. Flush writes it back once, on the way out, when the
// copy differs from what Open read.
type Store struct {
	BrokerPath string
	Run        Runner
	RunStdin   StdinRunner
	Now        func() time.Time
	// SealPath refuses Open/Flush unless this process is BrokerPath.
	// A rebuilt copy at another path cannot read or write the item
	// (🎯T865). Tests that mock Run leave this false.
	SealPath bool
	// Keychain, if set, is the keychain file. Empty is the default
	// keychain. Disposable-keychain tests set this.
	Keychain string
	// DataPath is the encrypted plan file. The Keychain item holds only
	// its key: security -i cuts stdin lines at 4 KiB, and the hex plan
	// blob is larger than that.
	DataPath string
}

// keychainShot is the process-wide copy. Store values are copied at
// each call site, so the startup read cannot live on Store.
type keychainShot struct {
	mu       sync.Mutex
	opened   bool
	openErr  error
	initial  Item
	item     Item
	key      []byte // nil until the Keychain item holds a data key
	flushed  bool
	flushErr error
}

var shot keychainShot

func resetKeychainShot() {
	shot = keychainShot{}
}

// ResetKeychainShot drops the process read and write. Tests call it so
// one case does not spend the run's shot. Production does not.
func ResetKeychainShot() { resetKeychainShot() }

func cloneItem(item Item) Item {
	out := Item{Records: map[string]Record{}}
	for k, v := range item.Records {
		out.Records[k] = v
	}
	return out
}

func (s Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Open reads the Keychain once. A later Open returns that result and
// does not read again. A missing item is an empty Item, not an error
// that falls through to an API key.
func Open(ctx context.Context, store Store) error {
	shot.mu.Lock()
	defer shot.mu.Unlock()
	if shot.opened {
		return shot.openErr
	}
	shot.opened = true
	if err := rejectUntrustedBroker(store); err != nil {
		shot.openErr = err
		return err
	}
	if store.Run == nil {
		shot.openErr = fmt.Errorf("omp: no keychain runner")
		return shot.openErr
	}
	item, key, err := readItem(ctx, store)
	if err != nil {
		shot.openErr = err
		return err
	}
	shot.initial = cloneItem(item)
	shot.item = cloneItem(item)
	shot.key = key
	return nil
}

// readItem reads the Keychain item and, when it holds a data key, the
// encrypted plan file. A legacy item holds the plan JSON itself; it has
// no key, and the next Flush moves it into the file.
func readItem(ctx context.Context, store Store) (Item, []byte, error) {
	find := []string{"find-generic-password",
		"-a", keychainAccount, "-s", KeychainService, "-w"}
	if store.Keychain != "" {
		find = append(find, store.Keychain)
	}
	out, err := store.Run(ctx, "security", find...)
	if err != nil && !isMissing(err, out) {
		return Item{}, nil, err
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return Item{Records: map[string]Record{}}, nil, nil
	}
	var kc struct {
		DataKey string `json:"data_key"`
	}
	if err := json.Unmarshal([]byte(raw), &kc); err != nil || kc.DataKey == "" {
		item, err := decodeItem(out)
		return item, nil, err
	}
	key, err := hex.DecodeString(kc.DataKey)
	if err != nil || len(key) != dataKeyLen {
		return Item{}, nil, fmt.Errorf("omp: keychain item holds a malformed data key")
	}
	item, err := readDataFile(store.DataPath, key)
	if err != nil {
		return Item{}, nil, err
	}
	return item, key, nil
}

func decodeItem(out []byte) (Item, error) {
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return Item{Records: map[string]Record{}}, nil
	}
	var item Item
	if err := json.Unmarshal([]byte(raw), &item); err != nil {
		return Item{}, &unreadableItemError{raw: []byte(raw), err: err}
	}
	if item.Records == nil {
		item.Records = map[string]Record{}
	}
	return item, nil
}

// Load returns the startup copy. It does not read the Keychain.
func (s Store) Load(ctx context.Context) (Item, error) {
	shot.mu.Lock()
	defer shot.mu.Unlock()
	if !shot.opened {
		return Item{}, fmt.Errorf("omp: keychain was not read at startup")
	}
	if shot.openErr != nil {
		return Item{}, shot.openErr
	}
	return cloneItem(shot.item), nil
}

// Save replaces the memory copy. It does not write the Keychain.
func (s Store) Save(ctx context.Context, item Item) error {
	if item.Records == nil {
		item.Records = map[string]Record{}
	}
	shot.mu.Lock()
	defer shot.mu.Unlock()
	if !shot.opened {
		return fmt.Errorf("omp: keychain was not read at startup")
	}
	if shot.openErr != nil {
		return shot.openErr
	}
	shot.item = cloneItem(item)
	return nil
}

// Flush writes the memory copy back once, when it differs from the
// startup read. A later Flush does not write again.
func Flush(ctx context.Context, store Store) error {
	shot.mu.Lock()
	defer shot.mu.Unlock()
	if !shot.opened || shot.openErr != nil {
		return shot.openErr
	}
	if shot.flushed {
		return shot.flushErr
	}
	if sameItem(shot.initial, shot.item) {
		shot.flushed = true
		return nil
	}
	if err := rejectUntrustedBroker(store); err != nil {
		shot.flushed = true
		shot.flushErr = err
		return err
	}
	if store.BrokerPath == "" {
		shot.flushed = true
		shot.flushErr = fmt.Errorf("omp: broker path is required for the keychain ACL")
		return shot.flushErr
	}
	if store.DataPath == "" {
		shot.flushed = true
		shot.flushErr = fmt.Errorf("omp: plan data path is required")
		return shot.flushErr
	}
	blob, err := json.Marshal(shot.item)
	if err != nil {
		shot.flushed = true
		shot.flushErr = err
		return err
	}
	key := shot.key
	if key == nil {
		key = make([]byte, dataKeyLen)
		if _, err := rand.Read(key); err != nil {
			shot.flushed = true
			shot.flushErr = fmt.Errorf("omp: generate plan data key: %w", err)
			return shot.flushErr
		}
	}
	// The file goes first. If the Keychain write then fails, the item
	// still holds the old plan (or nothing) and the next Open reads that.
	err = writeDataFile(store.DataPath, key, blob)
	if err == nil && shot.key == nil {
		if store.Run == nil && store.RunStdin == nil {
			err = fmt.Errorf("omp: no keychain runner")
		} else {
			err = writeKey(ctx, store, shot.item, key)
		}
		if err == nil {
			shot.key = key
		}
	}
	shot.flushed = true
	shot.flushErr = err
	return err
}

// writeKey stores the data key in the Keychain item. Update the secret
// in place: deleting the item and creating it again throws away Always
// Allow, so every login prompts again. -T on an update adds the broker;
// it does not replace the ACL. The key travels on security -i stdin as
// -X hex, so neither it nor any plan token appears in argv (🎯T131).
func writeKey(ctx context.Context, store Store, item Item, key []byte) error {
	args := []string{
		"add-generic-password",
		"-U",
		"-a", keychainAccount,
		"-s", KeychainService,
		"-T", store.BrokerPath,
	}
	if err := trustedPathOnly(args, store.BrokerPath); err != nil {
		return err
	}
	value, err := json.Marshal(struct {
		DataKey string `json:"data_key"`
	}{hex.EncodeToString(key)})
	if err != nil {
		return err
	}
	args = append(args, "-X", hex.EncodeToString(value))
	if store.Keychain != "" {
		args = append(args, store.Keychain)
	}
	stdin := []byte(interactiveLine(args))
	argv := []string{"-q", "-i"}
	spawned := append([]string{"security"}, argv...)
	if err := refuseSecretArgv(spawned, item); err != nil {
		return err
	}
	if store.RunStdin != nil {
		_, err := store.RunStdin(ctx, stdin, "security", argv...)
		return err
	}
	_, err = store.Run(ctx, "security", argv...)
	return err
}

func interactiveLine(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if interactiveSafe(a) {
			parts[i] = a
		} else {
			parts[i] = "'" + strings.ReplaceAll(a, "'", "'\\''") + "'"
		}
	}
	return strings.Join(parts, " ") + "\n"
}

func interactiveSafe(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '/' || r == ':' || r == '=' || r == '+':
		default:
			return false
		}
	}
	return true
}

func refuseSecretArgv(args []string, item Item) error {
	for _, rec := range item.Records {
		for _, secret := range []string{rec.AccessToken, rec.RefreshToken} {
			if len(secret) < 8 {
				continue
			}
			for _, a := range args {
				if strings.Contains(a, secret) {
					return fmt.Errorf("omp: refusing to place a plan token in process argv")
				}
			}
		}
	}
	return nil
}

func sameItem(a, b Item) bool {
	ab, err1 := json.Marshal(normalize(a))
	bb, err2 := json.Marshal(normalize(b))
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

func normalize(item Item) Item {
	if item.Records == nil {
		item.Records = map[string]Record{}
	}
	return item
}

// Put replaces one record and writes the item.
func (s Store) Put(ctx context.Context, provider string, rec Record) error {
	if !known(provider) {
		return fmt.Errorf("omp: %q is not a subscription provider", provider)
	}
	item, err := s.Load(ctx)
	if err != nil {
		return err
	}
	item.Records[provider] = rec
	if err := s.Save(ctx, item); err != nil {
		return err
	}
	clearRejected(provider)
	return nil
}

// AccessToken returns the stored access token when it is still valid.
// It does not refresh and it does not read the environment.
func (s Store) AccessToken(ctx context.Context, provider string) (string, error) {
	item, err := s.Load(ctx)
	if err != nil {
		return "", err
	}
	rec, ok := item.Records[provider]
	if !ok || rec.AccessToken == "" {
		return "", fmt.Errorf("omp: no %s login in the keychain item", provider)
	}
	if !rec.Expiry.IsZero() && !s.now().Before(rec.Expiry) {
		return "", fmt.Errorf("omp: %s login expired", provider)
	}
	return rec.AccessToken, nil
}

// Ensure returns a live access token. A missing or expired record is
// renewed through login (pi-ai). Failure does not fall through to an
// API key, models.yml, agent.db, or the process environment.
func (s Store) Ensure(ctx context.Context, provider string, login Login) (string, error) {
	tok, err := s.AccessToken(ctx, provider)
	if err == nil {
		return tok, nil
	}
	rec, err := login.Refresh(ctx, s, provider)
	if err != nil {
		return "", err
	}
	return rec.AccessToken, nil
}

func usableRefresh(rec Record) bool {
	return rec.RefreshToken != "" && rec.RefreshToken != "undefined"
}

func known(provider string) bool {
	switch provider {
	case Anthropic, OpenAICodex, Cursor, XAIOAuth:
		return true
	default:
		return false
	}
}

func rejectUntrustedBroker(store Store) error {
	if !store.SealPath {
		return nil
	}
	if store.BrokerPath == "" {
		return fmt.Errorf("omp: broker path is required for the keychain ACL")
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("omp: keychain ACL did not approve this binary: %w", err)
	}
	self, want := resolvePath(self), resolvePath(store.BrokerPath)
	if self == "" || self != want {
		return fmt.Errorf("omp: keychain ACL did not approve this binary (path %s, want %s)", self, want)
	}
	return nil
}

func resolvePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// ProductBrokerPath is the Claudia binary the Keychain ACL trusts (🎯T875).
func ProductBrokerPath() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	if filepath.Base(self) != "claudia" {
		return ""
	}
	return resolvePath(self)
}

func trustedPathOnly(args []string, broker string) error {
	trusted := 0
	for i := 0; i < len(args); i++ {
		if args[i] == "-T" && i+1 < len(args) {
			trusted++
			if args[i+1] != broker {
				return fmt.Errorf("omp: ACL entry %q is not the broker", args[i+1])
			}
		}
		if args[i] == "-A" {
			return fmt.Errorf("omp: -A would trust every program")
		}
		if args[i] == "-s" && i+1 < len(args) {
			switch args[i+1] {
			case "openai-api-key", "xai-api-key", "Claude Code-credentials":
				return fmt.Errorf("omp: refusing to write service %q", args[i+1])
			}
		}
	}
	if trusted != 1 {
		return fmt.Errorf("omp: ACL must name exactly one binary, named %d", trusted)
	}
	return nil
}

func isMissing(err error, out []byte) bool {
	blob := strings.ToLower(err.Error() + " " + string(out))
	return strings.Contains(blob, "could not be found") || strings.Contains(blob, "item not found")
}

// ScrubEnv removes plan tokens and the pay-as-you-go key variables from
// a sidecar environment. The access token travels in the IPC message,
// not in the environment.
func ScrubEnv(env []string) []string {
	drop := map[string]bool{
		"ANTHROPIC_API_KEY":   true,
		"OPENAI_API_KEY":      true,
		"XAI_API_KEY":         true,
		"CURSOR_ACCESS_TOKEN": true,
		"OMP_ACCESS_TOKEN":    true,
		"OMP_REFRESH_TOKEN":   true,
	}
	var out []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if drop[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}
