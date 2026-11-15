// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marcelocantos/claudia"
	"github.com/marcelocantos/claudia/controlplane"
	"github.com/marcelocantos/claudia/omp"
	"github.com/marcelocantos/sqlpipe/go/sqlpipe"
)

func openUsageGapDB(t *testing.T) *sqlpipe.Database {
	t.Helper()
	db, err := sqlpipe.OpenDatabase(filepath.Join(t.TempDir(), "usage-gap.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := controlplane.EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func usageRowCount(t *testing.T, db *sqlpipe.Database, seat, session string) int {
	t.Helper()
	qr, err := db.Query(`SELECT COUNT(*) FROM `+controlplane.UsageTable+` WHERE seat = ? AND session_id = ?`, seat, session)
	if err != nil {
		t.Fatal(err)
	}
	if len(qr.Rows) != 1 || len(qr.Rows[0]) != 1 {
		t.Fatalf("count shape %+v", qr.Rows)
	}
	n, ok := qr.Rows[0][0].(int64)
	if !ok {
		t.Fatalf("count type %T", qr.Rows[0][0])
	}
	return int(n)
}

func readUsageRows(t *testing.T, db *sqlpipe.Database, seat, session string) []claudia.UsageFact {
	t.Helper()
	qr, err := db.Query(`SELECT fact_json FROM `+controlplane.UsageTable+` WHERE seat = ? AND session_id = ? ORDER BY seq`, seat, session)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]claudia.UsageFact, 0, len(qr.Rows))
	for _, row := range qr.Rows {
		if len(row) != 1 {
			t.Fatalf("row shape %+v", row)
		}
		s, ok := row[0].(string)
		if !ok {
			t.Fatalf("fact_json type %T", row[0])
		}
		var f claudia.UsageFact
		if err := json.Unmarshal([]byte(s), &f); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func lastProducerJSON(raw []byte) []byte {
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "{") {
			return []byte(line)
		}
	}
	return raw
}

func startHermeticSidecar(t *testing.T, bun, socket, spool string) *exec.Cmd {
	t.Helper()
	srv := exec.Command(bun, "sidecar/server.ts", socket)
	srv.Env = append(os.Environ(), "CLAUDIA_OMP_SPOOL_DIR="+spool)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Process.Kill(); _ = srv.Wait() })
	ctx := t.Context()
	for !omp.Listening(ctx, socket) {
		time.Sleep(10 * time.Millisecond)
	}
	return srv
}

func TestUsageGapJournalPoisonPreservesControlPlaneAfterRestart(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("", "cl-usage-gap-cp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	seed := exec.Command(bun, "sidecar/testdata/gap-seq-usage-producer.ts", root)
	raw, err := seed.CombinedOutput()
	if err != nil {
		t.Fatalf("gap seed: %v: %s", err, raw)
	}
	var seeded struct {
		Poisoned         bool                `json:"poisoned"`
		PageError        string              `json:"page_error"`
		CPFacts          []claudia.UsageFact `json:"cp_facts"`
		MessageCount     int                 `json:"message_count"`
		ReloadUsageError string              `json:"reload_usage_error"`
	}
	if err := json.Unmarshal(lastProducerJSON(raw), &seeded); err != nil {
		t.Fatalf("gap seed json: %v: %s", err, raw)
	}
	if !seeded.Poisoned || seeded.PageError == "" || len(seeded.CPFacts) != 2 {
		t.Fatalf("gap fixture: %+v", seeded)
	}
	if seeded.MessageCount < 2 || seeded.ReloadUsageError == "" {
		t.Fatalf("createSeatAgent must reload conversation with poisoned usage: %+v", seeded)
	}

	db := openUsageGapDB(t)
	for i := range seeded.CPFacts {
		if err := controlplane.PublishUsageFacts(db, []claudia.UsageFact{seeded.CPFacts[i]}); err != nil {
			t.Fatal(err)
		}
	}
	cpBefore := readUsageRows(t, db, "seat", "generation")
	cursor := uint64(len(cpBefore))

	socket := filepath.Join(root, "omp.sock")
	spool := filepath.Join(root, "spool")
	srv := startHermeticSidecar(t, bun, socket, spool)
	ctx := t.Context()

	a := claudia.GapTestAgent(t, "seat", "generation", socket, cursor)
	if err := a.RefreshUsage(ctx); err == nil {
		t.Fatal("refresh must fail on poisoned gap journal")
	}
	if got := a.GapTestUsageCursor(); got != cursor {
		t.Fatalf("cursor advanced on poison: got %d want %d", got, cursor)
	}
	if usageRowCount(t, db, "seat", "generation") != len(cpBefore) {
		t.Fatal("CP row count changed before restart")
	}

	_ = srv.Process.Kill()
	_ = srv.Wait()
	startHermeticSidecar(t, bun, socket, spool)

	follow := exec.Command(bun, "sidecar/testdata/gap-seq-usage-producer.ts", root, "prompt-after-gap")
	out, err := follow.CombinedOutput()
	if err != nil {
		t.Fatalf("post-restart prompt: %v: %s", err, out)
	}
	var resumed struct {
		Messages      int    `json:"messages"`
		UsagePoisoned bool   `json:"usage_poisoned"`
		PageError     string `json:"page_error"`
	}
	if err := json.Unmarshal(lastProducerJSON(out), &resumed); err != nil {
		t.Fatalf("post-restart json: %v: %s", err, out)
	}
	if resumed.Messages < 3 || !resumed.UsagePoisoned || resumed.PageError == "" {
		t.Fatalf("post-restart seat: %+v", resumed)
	}

	b := claudia.GapTestAgent(t, "seat", "generation", socket, cursor)
	if err := b.RefreshUsage(context.Background()); err == nil {
		t.Fatal("refresh after restart must still fail")
	}
	if got := b.GapTestUsageCursor(); got != cursor {
		t.Fatalf("cursor advanced after restart: got %d want %d", got, cursor)
	}

	cpAfter := readUsageRows(t, db, "seat", "generation")
	if len(cpAfter) != len(cpBefore) {
		t.Fatalf("CP rows %d -> %d", len(cpBefore), len(cpAfter))
	}
	if !reflect.DeepEqual(cpBefore, cpAfter) {
		t.Fatalf("CP facts changed:\nbefore %+v\nafter %+v", cpBefore, cpAfter)
	}

	intruder := seeded.CPFacts[1]
	intruder.CallID = "gap-intruder-call"
	if err := controlplane.PublishUsageFacts(db, []claudia.UsageFact{intruder}); err == nil {
		t.Fatal("renumbered intruder at stored seq was published")
	}
	if usageRowCount(t, db, "seat", "generation") != len(cpBefore) {
		t.Fatal("intruder publish changed CP row count")
	}
}
