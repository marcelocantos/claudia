// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package wallclockguard

import (
	"bufio"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The fixture marks every clock the scanner must refuse with a VIOLATION
// comment on its line, so the expectation is read from the fixture rather
// than kept in step with it by hand.
func TestScanFindsExactlyTheUnansweredClocks(t *testing.T) {
	dir := filepath.Join("testdata", "mod")
	rep, err := Scan(dir, dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scanned != 1 {
		t.Fatalf("scanned %d files, want 1: fixture_test.go, not the *_live_test.go file or product code", rep.Scanned)
	}

	var got []string
	for _, c := range rep.Violations {
		got = append(got, c.Pos())
	}
	if want := linesMarked(t, filepath.Join(dir, "fixture_test.go"), "// VIOLATION"); !reflect.DeepEqual(got, want) {
		t.Errorf("violations\n got %v\nwant %v", got, want)
	}

	var markers []string
	for _, m := range rep.Markers {
		markers = append(markers, m.Pos()+" "+m.Why)
	}
	if len(markers) != 2 ||
		!strings.Contains(markers[0], "states no reason") ||
		!strings.Contains(markers[1], "answers for no clock") {
		t.Errorf("marker problems = %q, want the reasonless one then the stale one", markers)
	}

	// Seven exempted: two by func doc, line above, same line, the
	// multi-line statement, the const used twice, and nothing else.
	if len(rep.Exempted) != 7 {
		t.Errorf("exempted %d clocks, want 7: %+v", len(rep.Exempted), rep.Exempted)
	}
	for _, c := range rep.Exempted {
		if c.Reason == "" {
			t.Errorf("%s exempted with no reason", c.Pos())
		}
	}
}

// ScanModule reads every package directory the go command compiles and
// names each clock by its module-relative path.
func TestScanModuleCoversSubpackagesAndSkipsIgnoredDirs(t *testing.T) {
	dir := filepath.Join("testdata", "mod")
	rep, err := ScanModule(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scanned != 2 || rep.ScannedDirs != 2 {
		t.Fatalf("scanned %d files in %d dirs, want 2 in 2: mod and mod/sub, not mod/_scratch",
			rep.Scanned, rep.ScannedDirs)
	}
	var got []string
	for _, c := range rep.Violations {
		got = append(got, c.Pos())
	}
	want := linesMarked(t, filepath.Join(dir, "fixture_test.go"), "// VIOLATION")
	for _, pos := range linesMarked(t, filepath.Join(dir, "sub", "sub_test.go"), "// VIOLATION") {
		want = append(want, "sub/"+pos)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("violations\n got %v\nwant %v", got, want)
	}
}

// A guard that reads nothing passes forever; the caller must be able to tell.
func TestScanReportsAnEmptyDirectory(t *testing.T) {
	dir := filepath.Join("testdata", "empty")
	rep, err := Scan(dir, dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scanned != 0 || len(rep.Violations) != 0 {
		t.Fatalf("empty directory: %+v", rep)
	}
}

func TestMarkerReasonNeedsTheMarkerAtALineStart(t *testing.T) {
	for text, want := range map[string]string{
		"🎯T97 exemption: slow only strengthens it.\n":             "slow only strengthens it.",
		"setup.\n🎯T97 exemption, both below.\nNeither decides.\n": "both below. Neither decides.",
		"🎯T97 exemption\n":         "",
		"names a 🎯T97 exemption\n": "-",
	} {
		reason, ok := markerReason(text)
		if want == "-" {
			if ok {
				t.Errorf("%q: a mid-line mention was read as a marker", text)
			}
			continue
		}
		if !ok || reason != want {
			t.Errorf("%q: got (%q, %v), want %q", text, reason, ok, want)
		}
	}
}

func linesMarked(t *testing.T, path, mark string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		if strings.Contains(sc.Text(), mark) {
			out = append(out, filepath.Base(path)+":"+strconv.Itoa(n))
		}
	}
	return out
}
