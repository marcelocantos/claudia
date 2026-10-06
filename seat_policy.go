// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/marcelocantos/claudia/internal/broker"
)

// 🎯T1013.2: per-seat provider policy — prefer/allow/exclude plus the host's
// interrupt/park consent and any in-flight migration bookmark — lives here,
// in Claudia, not in a jevons-private sidecar. This is a general capability:
// any client of this module can read and write a seat's policy by name, the
// same name that client uses as its own [AgentDef.Name] / registry key.
// Claudia does not invent a second identity scheme for "seat" — it is keyed
// on whatever string the caller already uses to name the session.

// SeatPolicy is one seat's host-set provider constraints and host consent
// flags, plus an in-flight migration bookmark. The zero value means
// "no policy set": every provider is eligible, no preference, host may
// interrupt and may park.
type SeatPolicy struct {
	// PreferProvider is a preference, never a ban on other eligible
	// providers (see [SeatPlacementArgs.PreferProvider]).
	PreferProvider claudiaProvider `json:"prefer_provider,omitempty"`
	// AllowedProviders nil permits every published destination; a non-nil
	// empty slice (with AllowNone set) permits none. AllowNone
	// disambiguates "unset" from "explicitly none" across a JSON
	// round-trip, where a decoded empty slice and a decoded nil slice are
	// otherwise indistinguishable.
	AllowedProviders []claudiaProvider `json:"allowed_providers,omitempty"`
	AllowNone        bool              `json:"allow_none,omitempty"`
	ExcludeProviders []claudiaProvider `json:"exclude_providers,omitempty"`
	// AllowInterrupt and NeverPark are host consent flags: whether the
	// host owning this seat's process permits Claudia's migration flow to
	// interrupt an in-flight turn, and whether it refuses an automatic
	// park. These are host policy, not a placement input Resolve itself
	// consults — callers that gate interruption/parking on them do so at
	// their own call sites.
	HostMayInterrupt bool `json:"host_may_interrupt,omitempty"`
	HostNeverPark    bool `json:"host_never_park,omitempty"`

	// MigrationSeed, MigrationFrom, MigrationFromSession and
	// MigrationPendingStart bookmark an in-flight cross-provider
	// migration so a restarted host can resume it.
	MigrationSeed         string          `json:"migration_seed,omitempty"`
	MigrationFrom         claudiaProvider `json:"migration_from,omitempty"`
	MigrationFromSession  string          `json:"migration_from_session,omitempty"`
	MigrationPendingStart bool            `json:"migration_pending_start,omitempty"`
}

// claudiaProvider is a local alias so this file reads independently of
// where [Provider] is declared; it is always identical to Provider.
type claudiaProvider = Provider

// Allowed reports the destination allow-list. restricted is false when
// every published destination is eligible.
func (p SeatPolicy) Allowed() (providers []Provider, restricted bool) {
	if p.AllowNone {
		return []Provider{}, true
	}
	if p.AllowedProviders == nil {
		return nil, false
	}
	return append([]Provider(nil), p.AllowedProviders...), true
}

// SeatPolicyDirName is the seat-policy store's directory name under a
// Claudia state directory.
const SeatPolicyDirName = "seat-policy"

const seatPolicyFile = "seats.json"

// seatPolicyEnvDir overrides the store directory (tests, or a client that
// wants a dedicated location rather than Claudia's shared state dir).
const seatPolicyEnvDir = "CLAUDIA_SEAT_POLICY_DIR"

// DefaultSeatPolicyDir is where every client of this module shares seat
// policy by default: CLAUDIA_SEAT_POLICY_DIR if set, else
// <claudia state dir>/seat-policy.
func DefaultSeatPolicyDir() (string, error) {
	if p := os.Getenv(seatPolicyEnvDir); p != "" {
		return p, nil
	}
	state, err := broker.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, SeatPolicyDirName), nil
}

// SeatPolicyStore is a file-backed, name-keyed table of [SeatPolicy]. It is
// safe for concurrent use within one process; across processes the last
// writer to call Put/Update wins (same tradeoff as the rest of this
// package's small JSON-file stores — see [RecordPlanRawPayload]).
type SeatPolicyStore struct {
	path string
	mu   sync.Mutex
	rows map[string]SeatPolicy
}

// OpenSeatPolicyStore loads dir/seats.json, creating it on first write. An
// empty dir is memory-only (tests that do not need persistence).
func OpenSeatPolicyStore(dir string) (*SeatPolicyStore, error) {
	st := &SeatPolicyStore{rows: map[string]SeatPolicy{}}
	if dir == "" {
		return st, nil
	}
	st.path = filepath.Join(dir, seatPolicyFile)
	body, err := os.ReadFile(st.path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, err
	}
	if len(body) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(body, &st.rows); err != nil {
		return nil, fmt.Errorf("seat policy %s: %w", st.path, err)
	}
	if st.rows == nil {
		st.rows = map[string]SeatPolicy{}
	}
	return st, nil
}

// OpenDefaultSeatPolicyStore opens [DefaultSeatPolicyDir].
func OpenDefaultSeatPolicyStore() (*SeatPolicyStore, error) {
	dir, err := DefaultSeatPolicyDir()
	if err != nil {
		return nil, err
	}
	return OpenSeatPolicyStore(dir)
}

// Get returns name's policy. A seat with no policy set is the zero value.
func (s *SeatPolicyStore) Get(name string) SeatPolicy {
	if s == nil {
		return SeatPolicy{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[name]
}

// Put replaces name's policy and persists the store.
func (s *SeatPolicyStore) Put(name string, policy SeatPolicy) error {
	if s == nil {
		return fmt.Errorf("seat policy store is not configured")
	}
	return s.Update(name, func(cur *SeatPolicy) { *cur = policy })
}

// Update mutates name's policy under the lock and persists the store.
func (s *SeatPolicyStore) Update(name string, fn func(*SeatPolicy)) error {
	if s == nil {
		return fmt.Errorf("seat policy store is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.rows[name]
	fn(&cur)
	if s.rows == nil {
		s.rows = map[string]SeatPolicy{}
	}
	s.rows[name] = cur
	return s.saveLocked()
}

// All returns every seat with a stored (non-zero) policy, by name. The
// returned map is a copy; mutating it does not affect the store.
func (s *SeatPolicyStore) All() map[string]SeatPolicy {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]SeatPolicy, len(s.rows))
	for k, v := range s.rows {
		out[k] = v
	}
	return out
}

// ImportLegacyFile merges a legacy JSON file of the same map[string]SeatPolicy
// shape into this store, for the one-time move off a predecessor's private
// sidecar (jevons' retired internal/seatplan.Store wrote exactly this
// shape). An entry already present in this store is left alone — Claudia is
// the source of truth from the moment it has an opinion, a legacy file never
// overwrites it. Returns the number of entries imported. A missing legacy
// file is not an error (nothing to import). The legacy file is left on disk;
// callers that want it removed do so themselves once satisfied.
func (s *SeatPolicyStore) ImportLegacyFile(path string) (int, error) {
	if s == nil {
		return 0, fmt.Errorf("seat policy store is not configured")
	}
	if path == "" {
		return 0, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if len(body) == 0 {
		return 0, nil
	}
	var legacy map[string]SeatPolicy
	if err := json.Unmarshal(body, &legacy); err != nil {
		return 0, fmt.Errorf("seat policy legacy import %s: %w", path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[string]SeatPolicy{}
	}
	imported := 0
	for name, policy := range legacy {
		if _, exists := s.rows[name]; exists {
			continue
		}
		s.rows[name] = policy
		imported++
	}
	if imported == 0 {
		return 0, nil
	}
	return imported, s.saveLocked()
}

func (s *SeatPolicyStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	body, err := json.MarshalIndent(s.rows, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
