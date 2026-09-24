// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSeatHasHistoryReadsDatedSpool(t *testing.T) {
	dir := t.TempDir()
	if SeatHasHistory(dir, "jevons-po") {
		t.Fatal("empty spool must not look like a conversation")
	}
	older := filepath.Join(dir, "events-2026-09-24.log")
	live := filepath.Join(dir, "events-2026-09-25.log")
	if err := os.WriteFile(older, []byte(`{"ts":"2026-09-24T23:00:00.000Z","seat":"other","type":"text","text":"no"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte(`{"ts":"2026-09-25T01:00:00.000Z","seat":"jevons-po","type":"text","text":"hi"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !SeatHasHistory(dir, "jevons-po") {
		t.Fatal("want history for jevons-po")
	}
	recs, err := ReadSeat(dir, "jevons-po")
	if err != nil || len(recs) != 1 || recs[0].Text != "hi" {
		t.Fatalf("ReadSeat = %+v %v", recs, err)
	}
}

func TestReadSeatOlderDatesFirst(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "events-2026-09-26.log"), []byte(`{"ts":"2026-09-26T00:00:00.000Z","seat":"s","type":"text","text":"new"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events-2026-09-24.log"), []byte(`{"ts":"2026-09-24T00:00:00.000Z","seat":"s","type":"text","text":"old"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, err := ReadSeat(dir, "s")
	if err != nil || len(recs) != 2 {
		t.Fatalf("ReadSeat = %+v %v", recs, err)
	}
	if recs[0].Text != "old" || recs[1].Text != "new" {
		t.Fatalf("order = %q then %q, want old then new", recs[0].Text, recs[1].Text)
	}
}
