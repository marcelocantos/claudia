// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package livegate

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/internal/testctlenv"
)

// load reads this module's real inputs: the source census, the `live:` recipe,
// AGENTS.md and the exclusions file.
func load(t *testing.T) (Census, LiveTarget, string, Exclusions, string) {
	t.Helper()
	root, err := ModuleRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Enumerate(root)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	lt, err := ParseLiveTarget(mk)
	if err != nil {
		t.Fatalf("Makefile: %v", err)
	}
	agents, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	x, err := LoadExclusions(filepath.Join(root, ExclusionsFile))
	if err != nil {
		t.Fatal(err)
	}
	return c, lt, string(agents), x, root
}

// TestLiveGateCoverage is the gate: every live test in this module is
// reachable from `make live` and named in AGENTS.md's live-test table, or is
// recorded in live-gate-exclusions.json with a reason (T100).
//
// It runs on the ordinary hermetic path — `go test ./...`, so `make gate` and
// the pre-push hook — because a check that only runs when someone remembers
// to run it is the thing it is checking for.
func TestLiveGateCoverage(t *testing.T) {
	c, lt, agents, x, _ := load(t)
	if len(c.Live) == 0 {
		t.Fatal("no live tests found — the census is broken, not the repo")
	}
	for _, v := range Check(c, lt, agents, x, testctlenv.LiveGates()) {
		t.Errorf("%s", v)
	}
	t.Logf("%d live tests across %d gates, all reachable", len(c.Live), len(c.Gates))
}

// TestLiveTargetIsWhatMakeRuns holds the reading to the thing it is a reading
// of. Every other test here reasons about lt.Run; this one asks make what it
// would actually hand the shell and requires the two to be the same string.
// T111 was exactly that gap: the source named four tests that the expanded
// recipe did not.
func TestLiveTargetIsWhatMakeRuns(t *testing.T) {
	_, lt, _, _, root := load(t)
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not on PATH")
	}
	cmd := exec.Command(makeBin, "--no-print-directory", "-n", "live")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("make -n live: %v", err)
	}
	run := runFlag.FindSubmatch(out)
	if run == nil {
		t.Fatalf("make -n live printed no -run '…' expression:\n%s", out)
	}
	if got := string(run[1]); got != lt.Run {
		t.Fatalf("make hands the shell a different -run expression than this check reads.\nmake:     %s\nlivegate: %s", got, lt.Run)
	}
	// And each name the recipe spells survives as an alternative of its own.
	alts := lt.Alternatives()
	for _, want := range []string{"TestCrashSurvival$", "TestRewindSessionLive", "TestPoolCrashSurvival$", "TestGrokTaskRunSmoke"} {
		if !slices.Contains(alts, want) {
			t.Errorf("%s is not an alternative of its own in %q", want, alts)
		}
	}
}

// TestLiveGateCheckHasTeeth is the other half, and the reason this is an
// oracle rather than a list. Each case takes the module's real inputs and
// breaks exactly one thing, and the check must go red naming it. A check
// nobody has watched fail is not evidence that it would.
//
// The first case is the one T100's acceptance asks for by name: a brand-new
// live test that nobody wired. It is built end to end — a real .go file,
// parsed by the real Enumerate — so the proof runs through the same code path
// that reads the repo, not a hand-made Census that could agree with a broken
// parser.
func TestLiveGateCheckHasTeeth(t *testing.T) {
	c, lt, agents, x, _ := load(t)
	reg := testctlenv.LiveGates()

	if vs := Check(c, lt, agents, x, reg); len(vs) != 0 {
		t.Fatalf("the repo is already red, so nothing below proves anything:\n%s", join(vs))
	}

	t.Run("new live test nobody wired", func(t *testing.T) {
		dir := t.TempDir()
		src := `package scratch

import (
	"os"
	"testing"
)

func TestBrandNewProviderLiveSmoke(t *testing.T) {
	if os.Getenv("CLAUDIA_GROK_LIVE") == "" {
		t.Skip("CLAUDIA_GROK_LIVE not set")
	}
}
`
		if err := os.WriteFile(filepath.Join(dir, "brand_new_live_test.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		fresh, err := Enumerate(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(fresh.Live) != 1 || fresh.Live[0].Name != "TestBrandNewProviderLiveSmoke" {
			t.Fatalf("the census did not see the new live test: %+v", fresh.Live)
		}

		grown := c
		grown.Live = append(append([]Test{}, c.Live...), fresh.Live[0])
		vs := Check(grown, lt, agents, x, reg)
		wantBoth := []string{"make live", "AGENTS.md"}
		for _, want := range wantBoth {
			if !mentions(vs, "TestBrandNewProviderLiveSmoke", want) {
				t.Errorf("an unwired live test must be reported against %s; got:\n%s", want, join(vs))
			}
		}
	})

	t.Run("wired but no row in AGENTS.md", func(t *testing.T) {
		wired := c
		wired.Live = append(append([]Test{}, c.Live...), Test{
			Name: "TestTaskRunSmokeExtra", Pkg: ".", File: "extra_test.go", Line: 1,
			Gates: []string{"CLAUDIA_LIVE"},
		})
		// The -run expression matches it by prefix, as go test would, so the
		// only thing missing is the row an agent would go looking for.
		vs := Check(wired, lt, agents, x, reg)
		if !mentions(vs, "TestTaskRunSmokeExtra", "AGENTS.md") {
			t.Errorf("a live test with no AGENTS.md row must be reported; got:\n%s", join(vs))
		}
		if mentions(vs, "TestTaskRunSmokeExtra", "make live") {
			t.Errorf("that test IS reachable from make live; the check said otherwise:\n%s", join(vs))
		}
	})

	t.Run("named in make live but in an unlisted package", func(t *testing.T) {
		far := c
		far.Live = append(append([]Test{}, c.Live...), Test{
			Name: "TestTaskRunSmoke", Pkg: "someplace/else", File: "someplace/else/x_test.go", Line: 1,
			Gates: []string{"CLAUDIA_LIVE"},
		})
		narrowed := LiveTarget{Run: lt.Run, Pkgs: []string{".", "./daemon/"}}
		if !mentions(Check(far, narrowed, agents, x, reg), "TestTaskRunSmoke", "does not list its package") {
			t.Error("a live test outside the recipe's packages is unreachable however well it is named")
		}
	})

	t.Run("exclusion without a reason", func(t *testing.T) {
		bad := Exclusions{Exclusions: []Exclusion{
			{Test: c.Live[0].Name, Waives: []string{ObligationMakeLive}},
		}}
		if !mentions(Check(c, lt, agents, bad, reg), "exclusion "+c.Live[0].Name, "no reason") {
			t.Error("an exclusion with no reason must be refused")
		}
	})

	t.Run("exclusion that is no longer needed", func(t *testing.T) {
		stale := Exclusions{Exclusions: append(append([]Exclusion{}, x.Exclusions...), Exclusion{
			Test: "TestTaskRunSmoke", Waives: []string{ObligationMakeLive}, Reason: "it used to be expensive",
		})}
		if !mentions(Check(c, lt, agents, stale, reg), "TestTaskRunSmoke", "Drop the exclusion") {
			t.Error("an exclusion the repo has outgrown must be reported, or the list rots")
		}
	})

	t.Run("exclusion for a test that does not exist", func(t *testing.T) {
		ghost := Exclusions{Exclusions: []Exclusion{
			{Test: "TestSomethingDeletedLive", Waives: []string{ObligationMakeLive}, Reason: "gone"},
		}}
		if !mentions(Check(c, lt, agents, ghost, reg), "exclusion TestSomethingDeletedLive", "no live test") {
			t.Error("an exclusion naming a vanished test must be reported")
		}
	})

	t.Run("live gate the strip registry does not know", func(t *testing.T) {
		short := []string{"CLAUDIA_LIVE"}
		if !mentions(Check(c, lt, agents, x, short), "CLAUDIA_GROK_LIVE", "testctlenv.LiveGates()") {
			t.Error("a live gate missing from the strip registry must be reported (T20)")
		}
	})

	// T111: `$|` is make's order-only-prerequisites variable. It expands to
	// nothing, so the anchor and the separator both vanish and two names
	// reach go test fused into one that matches no test — while the Makefile
	// source still shows every name, which is all this check used to read.
	t.Run("a $| that make swallows", func(t *testing.T) {
		_, _, _, _, root := load(t)
		mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
		if err != nil {
			t.Fatal(err)
		}
		const escaped, swallowed = "TestCrashSurvival$$|", "TestCrashSurvival$|"
		if !strings.Contains(string(mk), escaped) {
			t.Fatalf("the Makefile no longer spells %s; this case needs a new specimen", escaped)
		}
		broken := strings.Replace(string(mk), escaped, swallowed, 1)
		if _, err := ParseLiveTarget([]byte(broken)); err == nil || !strings.Contains(err.Error(), `"$|"`) {
			t.Errorf("a recipe make would fuse must be refused naming $|; got %v", err)
		}
	})

	t.Run("two names fused into one", func(t *testing.T) {
		// The expression as make delivered it before T111, should one ever
		// reach Check by another road.
		fused := LiveTarget{Run: strings.Replace(lt.Run, "TestCrashSurvival$|", "TestCrashSurvival", 1), Pkgs: lt.Pkgs}
		if fused.Run == lt.Run {
			t.Fatal("nothing was fused; this case needs a new specimen")
		}
		vs := Check(c, fused, agents, x, reg)
		for _, name := range []string{"TestCrashSurvival", "TestRewindSessionLive"} {
			if !mentions(vs, name, "make live") {
				t.Errorf("%s is unreachable once fused and must be reported; got:\n%s", name, join(vs))
			}
		}
		if !mentions(vs, "Makefile", "matches no test") {
			t.Errorf("the fused name matches no test and must be reported; got:\n%s", join(vs))
		}
	})

	t.Run("a -run name that matches nothing", func(t *testing.T) {
		dead := LiveTarget{Run: lt.Run + "|TestThisWasRenamedAgesAgo", Pkgs: lt.Pkgs}
		if !mentions(Check(c, dead, agents, x, reg), "Makefile", "matches no test") {
			t.Error("a dead name in the -run expression reads like coverage and runs nothing")
		}
	})

	t.Run("a test whose gating cannot be read", func(t *testing.T) {
		dir := t.TempDir()
		src := `package scratch

import (
	"os"
	"testing"
)

func TestMurkyLive(t *testing.T) {
	if os.Getenv(whicheverGateItIs()) == "" {
		t.Skip("skip")
	}
}
`
		if err := os.WriteFile(filepath.Join(dir, "murky_test.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		fresh, err := Enumerate(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(fresh.Undecidable) != 1 {
			t.Fatalf("want one undecidable test, got %+v", fresh.Undecidable)
		}
		murky := c
		murky.Undecidable = fresh.Undecidable
		if !mentions(Check(murky, lt, agents, x, reg), "TestMurkyLive", "cannot tell whether the test is live") {
			t.Error("an unreadable gate must fail loud, not be dropped from the census")
		}
	})

	t.Run("gate passed into a helper", func(t *testing.T) {
		dir := t.TempDir()
		src := `package scratch

import (
	"os"
	"testing"
)

func measure(t *testing.T, gate string) {
	if os.Getenv(gate) == "" {
		t.Skip("skip")
	}
}

func TestMeasureSomething(t *testing.T) { measure(t, "CLAUDIA_CURSOR_LIVE") }
`
		if err := os.WriteFile(filepath.Join(dir, "helper_test.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		fresh, err := Enumerate(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(fresh.Live) != 1 || fresh.Live[0].Name != "TestMeasureSomething" ||
			!contains(fresh.Live[0].Gates, "CLAUDIA_CURSOR_LIVE") {
			t.Fatalf("a test whose gate is an argument to a helper must still be live: %+v", fresh.Live)
		}
	})

	t.Run("helper declared hermetic is not descended into", func(t *testing.T) {
		dir := t.TempDir()
		src := `package scratch

import (
	"os"
	"testing"
)

//livegate:hermetic a fixture Getenv's fall-through, not a gate
func fixtureEnv(k string) string { return os.Getenv(k) }

func TestUsesFixtureEnv(t *testing.T) { _ = fixtureEnv("CLAUDIA_LIVE") }
`
		if err := os.WriteFile(filepath.Join(dir, "fixture_test.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		fresh, err := Enumerate(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(fresh.Live) != 0 || len(fresh.Undecidable) != 0 {
			t.Fatalf("the directive should have settled it: live=%+v undecidable=%+v", fresh.Live, fresh.Undecidable)
		}
	})

	t.Run("hermetic directive without a reason", func(t *testing.T) {
		dir := t.TempDir()
		src := `package scratch

import (
	"os"
	"testing"
)

//livegate:hermetic
func fixtureEnv(k string) string { return os.Getenv(k) }

func TestUsesFixtureEnv(t *testing.T) { _ = fixtureEnv("CLAUDIA_LIVE") }
`
		if err := os.WriteFile(filepath.Join(dir, "bare_test.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		fresh, err := Enumerate(dir)
		if err != nil {
			t.Fatal(err)
		}
		bare := c
		bare.Undecidable = fresh.Undecidable
		if !mentions(Check(bare, lt, agents, x, reg), "fixtureEnv", "without stating why") {
			t.Errorf("a bare directive must be refused; got %+v", fresh.Undecidable)
		}
	})

	t.Run("hermetic directive that contradicts the source", func(t *testing.T) {
		dir := t.TempDir()
		src := `package scratch

import (
	"os"
	"testing"
)

func TestLyingLive(t *testing.T) {
	//livegate:hermetic it really is not
	if os.Getenv("CLAUDIA_LIVE") == "" {
		t.Skip("skip")
	}
}
`
		if err := os.WriteFile(filepath.Join(dir, "lying_test.go"), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		fresh, err := Enumerate(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(fresh.Live) != 0 || len(fresh.Undecidable) != 1 {
			t.Fatalf("want the claim refused, got live=%+v undecidable=%+v", fresh.Live, fresh.Undecidable)
		}
		if !strings.Contains(fresh.Undecidable[0].Why, "One of the two is wrong") {
			t.Errorf("why = %q", fresh.Undecidable[0].Why)
		}
	})
}

// TestLiveGateCensusReadsTheAwkwardForms pins the three shapes that a naive
// census gets wrong, using this module's own tests as the fixtures. Each one
// cost something to discover: a name scan reports two of them backwards, and
// a literal-only scan of os.Getenv misses the other two entirely.
func TestLiveGateCensusReadsTheAwkwardForms(t *testing.T) {
	c, _, _, _, _ := load(t)
	live := map[string][]string{}
	for _, tc := range c.Live {
		live[tc.Name] = tc.Gates
	}

	for _, want := range []struct {
		name, gate, why string
	}{
		{"TestT30LargePayloadSubmitsOnRealPath", "CLAUDIA_LIVE_SEND",
			"gate named through a package constant, and no \"Live\" in the test's name"},
		{"TestBrokerReclaimLiveBackends", "CLAUDIA_GROK_LIVE",
			"gate is a field of a table-driven case, unresolvable without type information"},
		{"TestLiveCodexTaskRun", "CLAUDIA_CODEX_LIVE",
			"lives in a sub-package the live recipe once did not list"},
		{"TestT96MeasureSilenceGrok", "CLAUDIA_GROK_LIVE",
			"whole body is one call to a helper, with the gate passed as an argument"},
		{"TestAcquireColdAndReturn", "CLAUDIA_LIVE",
			"gate is read by a requireLive-style helper, which is how a whole file of live tests hid"},
	} {
		gates, ok := live[want.name]
		if !ok {
			t.Errorf("%s is missing from the census (%s)", want.name, want.why)
			continue
		}
		if !contains(gates, want.gate) {
			t.Errorf("%s gates = %v, want %s (%s)", want.name, gates, want.gate, want.why)
		}
	}

	// And the inverse: "Live" in the name proves nothing. This one replays
	// two committed frames and spends not a cent.
	if _, ok := live["TestT30LiveReproSendSucceeds"]; ok {
		t.Error("TestT30LiveReproSendSucceeds is a fixture replay; a census that calls it live is counting names")
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func mentions(vs []Violation, where, want string) bool {
	for _, v := range vs {
		if strings.Contains(v.Where, where) && strings.Contains(v.Want, want) {
			return true
		}
	}
	return false
}

func join(vs []Violation) string {
	var b strings.Builder
	for _, v := range vs {
		b.WriteString("  " + v.String() + "\n")
	}
	return b.String()
}
