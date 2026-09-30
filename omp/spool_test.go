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

func TestSeatHasHistoryIgnoresSidecarBookkeeping(t *testing.T) {
	dir := t.TempDir()
	// The T935 isolated-broker specimen: after the spool was cleared, adopt
	// wrote "seat is not loaded" and the launch wrote ready. Those are not
	// a conversation (🎯T153).
	bookkeeping := "" +
		`{"ts":"2026-09-30T15:55:01.000Z","seat":"t935-broker-lost","type":"error","text":"seat is not loaded"}` + "\n" +
		`{"ts":"2026-09-30T15:55:07.000Z","seat":"t935-broker-lost","type":"ready"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "events-2026-09-30.log"), []byte(bookkeeping), 0o644); err != nil {
		t.Fatal(err)
	}
	if SeatHasHistory(dir, "t935-broker-lost") {
		t.Fatal("bookkeeping-only spool must not look like a conversation")
	}
	dropped := filepath.Join(dir, "events-2026-09-29.log")
	if err := os.WriteFile(dropped, []byte(`{"ts":"2026-09-29T00:00:00.000Z","seat":"t935-broker-lost","type":"dropped"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if SeatHasHistory(dir, "t935-broker-lost") {
		t.Fatal("dropped is handshake, not history")
	}
	live := filepath.Join(dir, "events-2026-09-30.log")
	if err := os.WriteFile(live, []byte(bookkeeping+`{"ts":"2026-09-30T16:00:00.000Z","seat":"t935-broker-lost","type":"text","text":"hi"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !SeatHasHistory(dir, "t935-broker-lost") {
		t.Fatal("a text record among bookkeeping is history")
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
