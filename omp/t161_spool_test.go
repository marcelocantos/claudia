// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 🎯T161: the RequireResume spool check runs on every sidecar seat launch
// during a broker restart. It decoded the whole dated spool (2.5 GB) for
// each one, ignored the launch deadline, and gave up at the first line over
// 8 MiB. These pin the three properties the rewrite rests on.

func writeDay(t *testing.T, dir, day string, lines ...string) {
	t.Helper()
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "events-"+day+".log"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestT161SeatHasHistoryReadsPastAnOverlongLine: a snapshot line longer
// than the old scanner's 8 MiB bound ahead of the seat's conversation used
// to end the search and read as "no history" — a RequireResume seat
// refused as having no conversation.
func TestT161SeatHasHistoryReadsPastAnOverlongLine(t *testing.T) {
	dir := t.TempDir()
	huge := `{"ts":"2026-09-30T01:00:00.000Z","seat":"other","type":"turn_end","snapshot":"` + strings.Repeat("x", 9<<20) + `"}`
	writeDay(t, dir, "2026-09-30",
		huge,
		`{"ts":"2026-09-30T01:00:01.000Z","seat":"claudia-po","type":"text","text":"hi"}`)
	if !SeatHasHistory(dir, "claudia-po") {
		t.Fatal("a conversation record after an overlong line was not found")
	}
}

// TestT161SeatHasHistoryHonoursContext: the search stops when the launch
// that asked for it is out of time, and says so rather than "no history".
func TestT161SeatHasHistoryHonoursContext(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for range 2000 {
		lines = append(lines, `{"ts":"2026-09-30T01:00:00.000Z","seat":"other","type":"text","text":"busy"}`)
	}
	writeDay(t, dir, "2026-09-30", lines...)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	found, err := SeatHasHistoryContext(ctx, dir, "claudia-po")
	if found || !errors.Is(err, context.Canceled) {
		t.Fatalf("SeatHasHistoryContext = %v, %v; want false, context.Canceled", found, err)
	}
}

// TestT161SeatHasHistoryMatchesTheSeatField: the byte prefilter is only a
// prefilter. A seat named inside another record's text, or a seat whose
// name is a prefix of another's, is not history.
func TestT161SeatHasHistoryMatchesTheSeatField(t *testing.T) {
	dir := t.TempDir()
	writeDay(t, dir, "2026-09-30",
		`{"ts":"2026-09-30T01:00:00.000Z","seat":"jevons-po","type":"text","text":"ask \"jevons\" about it"}`,
		`{"ts":"2026-09-30T01:00:01.000Z","seat":"other","type":"text","text":"jevons"}`,
		`{"ts":"2026-09-30T01:00:02.000Z","seat":"jevons","type":"ready"}`,
		`not json "jevons"`)
	if SeatHasHistory(dir, "jevons") {
		t.Fatal("records of other seats, a bookkeeping line and a torn line read as jevons's history")
	}
	if !SeatHasHistory(dir, "jevons-po") {
		t.Fatal("jevons-po's text record was missed")
	}
	// A name with no stable JSON spelling is decoded line by line.
	writeDay(t, dir, "2026-09-29", `{"ts":"2026-09-29T01:00:00.000Z","seat":"odd\"seat","type":"text","text":"hi"}`)
	if !SeatHasHistory(dir, `odd"seat`) {
		t.Fatal("a seat name with a quote was missed")
	}
}
