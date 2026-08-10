// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The policy guard is the structural half of 🎯T2.8: the Clock and
// BackpressureSource seams are only worth building if policy code cannot route
// around them. This file enumerates every declared policy path and fails the
// build when one reads the wall clock or a live socket directly, so the seams
// are the ONLY way policy reads time and backpressure.
//
// Two things make it hard to erode. The scanner itself is a pure function with
// its own table test below, covering both what must trip it and what must not
// (an over-broad rule that flagged time.Unix would be abandoned within a week).
// And the enrolment of pool.go is pinned by name, so quietly deleting its
// marker fails rather than silently shrinking the guarded set.

// policyRule is one banned construct plus the seam that replaces it.
type policyRule struct {
	name string
	pat  *regexp.Regexp
	use  string
	// exemptMarker, when it appears in a file, disables this one rule for that
	// file. Rules with no marker can only be waived by name, in exemptFiles.
	exemptMarker string
}

// transportMarker declares a file as the broker's own socket transport. Such a
// file may dial — that is its job — but the waiver is deliberately narrow: it
// exempts only the live-socket rule, and only for a file that says so out loud.
// Transport may carry backpressure bytes; it must never *interpret* them. That
// stays broker.ClassifyResultLine's job, so the wire shape of a 429 has exactly
// one reader.
const transportMarker = "//claudia:transport"

var policyRules = []policyRule{{
	name: "wall clock",
	// Deliberately narrow: only the calls that *read* or *wait on* real time.
	// time.Unix, time.Duration, time.Time and the duration constants are pure
	// and stay legal, or the rule would be noise and get switched off.
	pat: regexp.MustCompile(`\btime\.(Now|Since|Until|After|Sleep|Tick|NewTimer|NewTicker)\(`),
	use: "read time through broker.Clock",
}, {
	name:         "live socket",
	pat:          regexp.MustCompile(`\b(?:http\.(?:Get|Post|Head|PostForm|DefaultClient|Client\{)|(?:net|tls)\.Dial)`),
	use:          "consume rate-limit signals through broker.BackpressureSource",
	exemptMarker: transportMarker,
}}

// policyViolation is one banned construct found in a policy path.
type policyViolation struct {
	line  int
	rule  string
	match string
	use   string
}

// scanPolicySource is the guard's whole decision, kept pure so it can be tested
// against synthetic sources in both directions.
func scanPolicySource(src []byte) []policyViolation {
	var out []policyViolation
	for _, rule := range policyRules {
		if rule.exemptMarker != "" && bytes.Contains(src, []byte(rule.exemptMarker)) {
			continue
		}
		for _, loc := range rule.pat.FindAllIndex(src, -1) {
			out = append(out, policyViolation{
				line:  1 + strings.Count(string(src[:loc[0]]), "\n"),
				rule:  rule.name,
				match: string(src[loc[0]:loc[1]]),
				use:   rule.use,
			})
		}
	}
	return out
}

// policyMarker enrols a file outside this package as a broker policy path.
const policyMarker = "//claudia:policy"

// exemptFiles waives named rules for named files. The waiver is per-rule, not
// per-file, so exempting the Clock seam from the wall-clock rule does not also
// license it to open a socket. Keyed by repo-relative path, and kept here in
// the guard rather than as a marker in the guarded file, so granting a waiver
// is an edit a reviewer sees next to the rule it weakens.
var exemptFiles = map[string]map[string]bool{
	// clock.go is the seam: it is the one place time.Now/time.After are
	// allowed, because it is what every other policy path reads time through.
	"internal/broker/clock.go": {"wall clock": true},
}

// pinnedPolicyFiles must be enrolled at all times. Erosion is the realistic
// failure mode for a guard like this — someone hits the rule, drops the marker,
// and the guarded set silently shrinks to nothing. Naming the file here turns
// that into a build failure.
var pinnedPolicyFiles = []string{"pool.go"}

// policyPaths returns every file the guard scans: this package (broker policy
// by construction, minus the Clock seam) plus any file elsewhere in the repo
// carrying the //claudia:policy marker.
func policyPaths(t *testing.T) map[string][]byte {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}

	out := map[string][]byte{}
	add := func(path string, requireMarker bool) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if requireMarker && !strings.Contains(string(src), policyMarker) {
			return
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		out[filepath.ToSlash(rel)] = src
	}

	// This package: policy by construction.
	pkgDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		add(filepath.Join(pkgDir, name), false)
	}

	// Elsewhere in the repo: opt-in by marker. The root package is where the
	// pre-broker policy code lives (pool.go), and where 🎯T2.1's client shim
	// will land.
	rootEntries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rootEntries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		add(filepath.Join(root, name), true)
	}
	return out
}

// TestPolicyPathsUseTheInjectedSeams is the build-failing guard. It scans every
// declared policy path and reports each direct wall-clock or live-socket read.
func TestPolicyPathsUseTheInjectedSeams(t *testing.T) {
	paths := policyPaths(t)
	if len(paths) == 0 {
		t.Fatal("no policy paths found — the guard is scanning nothing")
	}
	for name, src := range paths {
		for _, v := range scanPolicySource(src) {
			if exemptFiles[name][v.rule] {
				continue
			}
			t.Errorf("%s:%d: policy code must not call %q (%s rule); %s — 🎯T2.8 seam",
				name, v.line, v.match, v.rule, v.use)
		}
	}
}

// TestExemptionsAreLiveAndNarrow keeps the waiver list from rotting into a
// blanket pass. Every exemption must name a real file, name a real rule, and
// still be needed — an exemption for a rule the file no longer breaks is one
// nobody will notice has become a licence.
func TestExemptionsAreLiveAndNarrow(t *testing.T) {
	paths := policyPaths(t)
	known := map[string]bool{}
	for _, r := range policyRules {
		known[r.name] = true
	}
	for file, rules := range exemptFiles {
		src, ok := paths[file]
		if !ok {
			t.Errorf("exemptFiles names %s, which is not a scanned policy path", file)
			continue
		}
		for rule := range rules {
			if !known[rule] {
				t.Errorf("exemptFiles[%s] waives unknown rule %q", file, rule)
				continue
			}
			used := false
			for _, v := range scanPolicySource(src) {
				if v.rule == rule {
					used = true
					break
				}
			}
			if !used {
				t.Errorf("exemptFiles[%s] waives %q but the file no longer breaks that rule — drop the waiver",
					file, rule)
			}
		}
	}
}

// TestPinnedPolicyFilesStayEnrolled stops the guarded set being emptied by
// quietly deleting a marker.
func TestPinnedPolicyFilesStayEnrolled(t *testing.T) {
	paths := policyPaths(t)
	for _, want := range pinnedPolicyFiles {
		if _, ok := paths[want]; !ok {
			t.Errorf("%s is no longer enrolled as a policy path: it must carry the %s marker (🎯T2.8)",
				want, policyMarker)
		}
	}
}

// TestScanPolicySourceHasTeeth is the oracle for the oracle. Each "must trip"
// case is a way real policy code would reach around a seam; each "must not"
// case is legal code the rule would be scrapped for flagging.
func TestScanPolicySourceHasTeeth(t *testing.T) {
	mustTrip := map[string]string{
		"wall clock read":      "func f() { now := time.Now() }",
		"elapsed since":        "func f() { d := time.Since(start) }",
		"blocking sleep":       "func f() { time.Sleep(5 * time.Second) }",
		"raw timer channel":    "func f() { <-time.After(ttl) }",
		"ticker loop":          "func f() { t := time.NewTicker(interval) }",
		"direct http get":      `func f() { resp, _ := http.Get("https://api.anthropic.com/v1/messages") }`,
		"shared http client":   "func f() { http.DefaultClient.Do(req) }",
		"hand-rolled dial":     `func f() { c, _ := net.Dial("tcp", addr) }`,
		"reads the clock late": "func f() {\n\t_ = 1\n\treturn time.Now().Unix()\n}",
	}
	for name, src := range mustTrip {
		t.Run("trips/"+name, func(t *testing.T) {
			if got := scanPolicySource([]byte(src)); len(got) == 0 {
				t.Fatalf("guard did not trip on %q", src)
			}
		})
	}

	mustNotTrip := map[string]string{
		"clock seam":         "func f() { now := c.Now() }",
		"package-level seam": "func f() { now := poolClock.Now() }",
		"pure conversion":    "func f() { t := time.Unix(deadline, 0) }",
		"duration literal":   "func f() { d := 5 * time.Minute }",
		"duration cast":      "func f() { d := time.Duration(secs) * time.Second }",
		"type reference":     "func f(at time.Time) time.Duration { return 0 }",
		"other package Now":  "func f() { now := mytime.Now() }",
		"injected source":    "func f(src BackpressureSource) { <-src.Signals() }",
		"comment mentioning": "// policy must never call time.Now directly\nfunc f() {}",
	}
	for name, src := range mustNotTrip {
		t.Run("allows/"+name, func(t *testing.T) {
			if got := scanPolicySource([]byte(src)); len(got) != 0 {
				t.Fatalf("guard is over-broad: tripped on %q with %+v", src, got)
			}
		})
	}
}

// TestTransportMarkerWaivesOnlyTheSocketRule pins the narrowness of the
// transport waiver. The broker's own socket transport (🎯T2.1) must be able to
// dial without the guard blocking it, but declaring a file transport must not
// hand it the wall clock as well, and must not let it interpret a 429 itself.
func TestTransportMarkerWaivesOnlyTheSocketRule(t *testing.T) {
	dialer := "\n" + transportMarker + "\nfunc dial() { c, _ := net.Dial(\"unix\", sock) }\n"
	if got := scanPolicySource([]byte(dialer)); len(got) != 0 {
		t.Errorf("transport file was blocked from dialing: %+v", got)
	}

	dialerWithClock := dialer + "func stamp() { at := time.Now() }\n"
	got := scanPolicySource([]byte(dialerWithClock))
	if len(got) != 1 || got[0].rule != "wall clock" {
		t.Errorf("transport marker leaked into the wall-clock rule: %+v", got)
	}

	undeclared := "func dial() { c, _ := net.Dial(\"unix\", sock) }\n"
	if got := scanPolicySource([]byte(undeclared)); len(got) != 1 || got[0].rule != "live socket" {
		t.Errorf("an undeclared file was allowed to dial: %+v", got)
	}
}

// TestScanPolicySourceReportsEveryHit checks the guard does not stop at the
// first violation — a file with two wall-clock reads must report both, or a
// fix-one-rebuild loop hides the second.
func TestScanPolicySourceReportsEveryHit(t *testing.T) {
	src := "package p\n\nfunc a() { _ = time.Now() }\nfunc b() { time.Sleep(d) }\n"
	got := scanPolicySource([]byte(src))
	if len(got) != 2 {
		t.Fatalf("got %d violations, want 2: %+v", len(got), got)
	}
	if got[0].line != 3 || got[1].line != 4 {
		t.Errorf("violations reported at lines %d,%d; want 3,4", got[0].line, got[1].line)
	}
}
