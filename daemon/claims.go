// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Which seats a restart brings back first, and which it leaves for a
// consumer to ask for (🎯T161).
//
// A restart resumed every AutoStart grant, two at a time, in name order. On
// 2026-09-30 that was 25 of them, most of them workers no consumer had held
// in days — "cl-t…" sorts ahead of "claudia-po" and "jevons-po" — so the
// product owners a consumer was waiting on came back last, behind seats
// whose restart nudge then set them to work nobody had asked for.
//
// The evidence the daemon has is ownership: a grant a consumer connection
// holds is one somebody is using. The daemon records when each grant was
// last held, resumes the most recently held first, and does not resume a
// grant nobody has held for DefaultResumeUnclaimedAfter. Not resuming a
// grant does not forget it: it stays registered, and a consumer that asks
// for it by name is granted it exactly as before, started on demand.

const (
	// claimsFile holds the claim ledger under StateDir.
	claimsFile = "claims.json"
	// claimsVersion is written in the ledger and required when reading it.
	claimsVersion = 1
	// DefaultResumeUnclaimedAfter is how long a grant may go without a
	// consumer before a restart stops resuming it.
	DefaultResumeUnclaimedAfter = 24 * time.Hour
	// claimRefreshInterval is how often grants that are still held are
	// stamped. A crash skips the stamp an owner's disconnect would have
	// written, so the ledger is at most this stale for a seat held
	// throughout.
	claimRefreshInterval = 10 * time.Minute
)

// seatClaim is what the ledger knows about one grant.
type seatClaim struct {
	// Held is the last time a consumer connection owned the grant.
	Held time.Time `json:"held,omitzero"`
	// Seen is when the daemon first found the grant with no ownership on
	// record. Its unclaimed time counts from here, so a ledger that
	// predates a grant never condemns it on first sight.
	Seen time.Time `json:"seen,omitzero"`
}

type claimsDoc struct {
	Version int                  `json:"version"`
	Seats   map[string]seatClaim `json:"seats"`
}

// seatClaims is the daemon's claim ledger, persisted by write-and-rename.
type seatClaims struct {
	path string

	mu    sync.Mutex
	seats map[string]seatClaim
}

// loadSeatClaims reads the ledger. A missing file is an empty ledger; a
// file that cannot be read is an error, not a reset.
func loadSeatClaims(path string) (*seatClaims, error) {
	c := &seatClaims{path: path, seats: map[string]seatClaim{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	var doc claimsDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if doc.Version != claimsVersion {
		return nil, fmt.Errorf("%s: version %d, want %d", path, doc.Version, claimsVersion)
	}
	for name, claim := range doc.Seats {
		c.seats[name] = claim
	}
	return c, nil
}

// held stamps names as owned by a consumer at at.
func (c *seatClaims) held(at time.Time, names ...string) error {
	if len(names) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, name := range names {
		claim := c.seats[name]
		if at.After(claim.Held) {
			claim.Held = at
		}
		c.seats[name] = claim
	}
	return c.saveLocked()
}

// reconcile makes the ledger name exactly the registered grants: a grant
// with no entry is seen at at, and entries for grants that are gone are
// dropped.
func (c *seatClaims) reconcile(at time.Time, registered []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	want := make(map[string]bool, len(registered))
	for _, name := range registered {
		want[name] = true
		if _, ok := c.seats[name]; !ok {
			c.seats[name] = seatClaim{Seen: at}
		}
	}
	for name := range c.seats {
		if !want[name] {
			delete(c.seats, name)
		}
	}
	return c.saveLocked()
}

// resumeOrder splits names into the grants to resume, most recently held
// first, and those left for a consumer to ask for. A grant never held
// resumes after every held one, in name order, until it has gone
// unclaimed for unclaimedAfter since it was first seen. A negative
// unclaimedAfter leaves nothing out.
func (c *seatClaims) resumeOrder(now time.Time, unclaimedAfter time.Duration, names []string) (resume, left []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var held, unheld []string
	for _, name := range names {
		claim := c.seats[name]
		last := claim.Held
		if last.IsZero() {
			last = claim.Seen
		}
		if unclaimedAfter >= 0 && !last.IsZero() && now.Sub(last) > unclaimedAfter {
			left = append(left, name)
			continue
		}
		if claim.Held.IsZero() {
			unheld = append(unheld, name)
		} else {
			held = append(held, name)
		}
	}
	sort.SliceStable(held, func(i, j int) bool {
		a, b := c.seats[held[i]].Held, c.seats[held[j]].Held
		if !a.Equal(b) {
			return a.After(b)
		}
		return held[i] < held[j]
	})
	sort.Strings(unheld)
	sort.Strings(left)
	return append(held, unheld...), left
}

func (c *seatClaims) saveLocked() error {
	raw, err := json.MarshalIndent(claimsDoc{Version: claimsVersion, Seats: c.seats}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".claims-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.path)
}
