// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia/internal/broker"
)

// 🎯T207: a seat the consumer stood down is not resumed on broker restart.
func TestT207BrokerRestartSkipsStoodDownSeat(t *testing.T) {
	f := newFixture(t)
	starts := recordStarts(f)
	now := f.clock.Now()
	writeGrantsTable(t, f.state, autoStart(t, "parked-worker", "kept-worker"))
	writeClaims(t, f.state, map[string]seatClaim{
		"kept-worker":   {Held: now.Add(-time.Minute)},
		"parked-worker": {Held: now.Add(-time.Hour), StoodDown: now},
	})
	opts := f.options(nil)
	opts.RestartNudge = "-"
	opts.ResumeConcurrency = 1
	f.bootWith(t, opts)
	<-f.d.resumeDone
	if got := strings.Join(starts(), ","); got != "kept-worker" {
		t.Fatalf("resume starts = %q, want only kept-worker", got)
	}
}

// 🎯T207: a consumer park (release detach) records stand-down in claims.
func TestT207ReleaseDetachRecordsStoodDown(t *testing.T) {
	f := newFixture(t)
	f.boot(t, nil)
	owner := rawSeat(t, f.sock, "parked-worker")
	resp := rawCall(t, owner, &broker.Request{ID: "r", Type: broker.TypeRelease,
		Release: &broker.ReleaseRequest{Name: "parked-worker", Disposition: broker.DispositionDetach}})
	if resp.Type != broker.TypeReleased {
		t.Fatalf("release detach = %s %+v", resp.Type, resp.Error)
	}
	claims := readClaims(t, f.state)
	if claims["parked-worker"].StoodDown.IsZero() {
		t.Fatal("release detach did not record stood_down in claims")
	}
}

func TestT207ResumeOrderSkipsStoodDown(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	writeClaims(t, dir, map[string]seatClaim{
		"held":   {Held: now.Add(-time.Hour)},
		"parked": {Held: now.Add(-time.Minute), StoodDown: now},
	})
	c, err := loadSeatClaims(filepath.Join(dir, claimsFile))
	if err != nil {
		t.Fatal(err)
	}
	resume, left := c.resumeOrder(now, time.Hour, []string{"held", "parked"})
	if len(resume) != 1 || resume[0] != "held" || len(left) != 1 || left[0] != "parked" {
		t.Fatalf("resume=%v left=%v", resume, left)
	}
}
