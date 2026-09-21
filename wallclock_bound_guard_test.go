// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	goast "go/ast"
	goparser "go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// shortenableBounds is the declared set of product timeouts a hermetic test
// in this package may write to, and why each one is safe to write to.
//
// A reason is not prose for its own sake. It has to answer the question
// that 🎯T93 was lost on: which single step does this bound speak for, and
// what stops another step on the same path answering for it when the host
// is slow? A bound that cannot be described that way is one knob over two
// waits, and shortening it makes a test that cannot say what it tested.
var shortenableBounds = map[string]string{
	"codexAppServerThreadTimeout": "bounds thread/start, thread/resume and thread/name/set only; " +
		"initialize is bounded separately by codexAppServerInitializeTimeout, which a test " +
		"leaves at 20s, so the earlier step cannot expire first and answer for this one (🎯T93)",
	"lsofTimeout": "bounds one exec of the store-holder lsof probe (cursor_reap.go) and nothing " +
		"else on its path; the test's fake lsof hangs for 60s, so the bound is the only " +
		"wait that can end the call and no earlier step shares it",
	"cursorPromptSilenceBound": "bounds silence before the first inbound message of a prompt and " +
		"nothing else — the first thing the peer says disarms it, so no other wait on that path " +
		"shares it; cursor_acp.go documents it as a var solely so hermetics can shorten it",
}

// TestHermeticTestsDeclareTheProductBoundsTheyShorten closes the hole 🎯T93
// walked through in 🎯T33's guard.
//
// 🎯T33 put TestHermeticTestsHaveNoWallClockDeadline in codex/ to stop a
// hermetic test creating its own clock — context.WithTimeout, time.After
// and friends, written in the test. It cannot see the clock that decided
// TestHermeticCodexThreadStartTimesOut, because that one lives in the
// product: codex_session.go bounds the app-server handshake, and the test
// merely assigns it 200ms. Nothing in the test source looks like a
// deadline, and the verdict still came from one.
//
// So the signature this guard looks for is the assignment itself: a
// hermetic test writing a duration into a package-level var arms a clock
// against its own assertion, whoever declared the var. That cannot be
// banned — shortening a production bound is how a timeout gets tested at
// all — so it is made declared instead. Each such var is listed above with
// the reason it is safe to shorten, and the next one added fails this test
// until somebody writes that reason down.
//
// What the guard decides mechanically is that the list and the code agree.
// Whether a reason is true is review's job; what this stops is the hazard
// being silent, which is the form it took in 🎯T93.
func TestHermeticTestsDeclareTheProductBoundsTheyShorten(t *testing.T) {
	fset := gotoken.NewFileSet()
	bounds, productFiles := packageDurationVars(t, fset)
	shortened, scanned := boundsShortenedByHermeticTests(t, fset, bounds)

	// A guard that scanned nothing would pass forever.
	if productFiles == 0 || scanned == 0 {
		t.Fatalf("guard is not looking at anything: %d product files, %d hermetic test files",
			productFiles, scanned)
	}

	for name, where := range shortened {
		if shortenableBounds[name] == "" {
			t.Errorf("%s: hermetic test writes product bound %s, which is not in "+
				"shortenableBounds (🎯T93). A test that arms a product clock against its "+
				"own assertion must say which single step that bound speaks for and what "+
				"stops another step expiring first.", where, name)
		}
	}

	// The registry describes the code, it does not wish at it.
	for _, name := range sortedKeys(shortenableBounds) {
		switch {
		case !bounds[name]:
			t.Errorf("shortenableBounds names %s, which is no longer a package-level duration var "+
				"— drop the entry or fix the name (🎯T93)", name)
		case shortened[name] == "":
			t.Errorf("shortenableBounds names %s, which no hermetic test shortens any more "+
				"— drop the entry (🎯T93)", name)
		}
	}
}

// isDurationSpec reports whether a package-level var spec declares a
// time.Duration, either by type or by a value built from time's units.
func isDurationSpec(vs *goast.ValueSpec) bool {
	if sel, ok := vs.Type.(*goast.SelectorExpr); ok {
		if pkg, ok := sel.X.(*goast.Ident); ok && pkg.Name == "time" && sel.Sel.Name == "Duration" {
			return true
		}
	}
	units := map[string]bool{
		"Nanosecond": true, "Microsecond": true, "Millisecond": true,
		"Second": true, "Minute": true, "Hour": true,
	}
	for _, v := range vs.Values {
		found := false
		goast.Inspect(v, func(n goast.Node) bool {
			sel, ok := n.(*goast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*goast.Ident); ok && pkg.Name == "time" && units[sel.Sel.Name] {
				found = true
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// codexHandshakeEarlierSteps names the client methods whose handshake RPC
// runs before the one TestHermeticCodexThreadStartTimesOut asserts on.
//
// This is the app-server protocol's order, not a guess: Start calls
// initialize and only then openThread, and a reply to the second cannot
// arrive before the first has been answered. So whatever bounds initialize
// is the wait that gets to expire first, and a test that shortens THAT one
// is arming the step it is not testing.
var codexHandshakeEarlierSteps = []string{"initialize"}

// TestShortenedBoundDoesNotGuardAnEarlierHandshakeStep is 🎯T93's oracle,
// and it decides on the wiring rather than on a duration.
//
// The defect was never that a timeout was too short. It was that one var
// bounded two steps, so shortening it to test the later one also armed the
// earlier one, and whichever expired first wrote the verdict. That is a
// property of which identifier each requestHandshake call is handed — it is
// in the source, and it does not need a clock to read.
//
// Which matters, because a clock cannot decide this family. The obvious
// experiment is to make initialize slow and watch the single-knob code fail,
// but a test that only reds on a slow host is another instance of the bug it
// is guarding: cl-t33 ran the pre-fix code five times at load 44 and it
// passed every time. This guard reds on an idle laptop and a thrashing one
// alike, because merging the two bounds back into one knob is visible in the
// AST whatever the host is doing.
//
// Scope: the earlier step's bound must not be one a hermetic test writes.
// Whether the later step's bound is shortened is fine and is the point —
// that is the bound the test is entitled to arm.
func TestShortenedBoundDoesNotGuardAnEarlierHandshakeStep(t *testing.T) {
	fset := gotoken.NewFileSet()
	bounds, _ := packageDurationVars(t, fset)
	shortened, _ := boundsShortenedByHermeticTests(t, fset, bounds)

	file, err := goparser.ParseFile(fset, "codex_session.go", nil, 0)
	if err != nil {
		t.Fatalf("parse codex_session.go: %v", err)
	}

	// Method name -> the bound identifier its requestHandshake call is given.
	guarding := map[string]string{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*goast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		goast.Inspect(fn.Body, func(n goast.Node) bool {
			call, ok := n.(*goast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*goast.SelectorExpr)
			if !ok || sel.Sel.Name != "requestHandshake" {
				return true
			}
			id, ok := call.Args[len(call.Args)-1].(*goast.Ident)
			if !ok {
				t.Errorf("%s: requestHandshake is given a timeout that is not a named bound — "+
					"this guard reads the identifier, so an inline duration hides the wiring (🎯T93)",
					fset.Position(call.Pos()))
				return true
			}
			guarding[fn.Name.Name] = id.Name
			return true
		})
	}

	for _, step := range codexHandshakeEarlierSteps {
		bound, found := guarding[step]
		if !found {
			t.Fatalf("no requestHandshake call found in %s — this guard has lost the step it "+
				"watches, and would pass forever (🎯T93)", step)
		}
		if where := shortened[bound]; where != "" {
			t.Errorf("%s bounds the handshake step before the one under test with %s, and %s "+
				"shortens %s (🎯T93). The earlier wait then expires first and the assertion "+
				"reports a step it was not testing — on an idle host it passes, on a loaded "+
				"one it fails, and nothing in either run says which happened. Give the two "+
				"steps separate bounds and shorten only the later one.",
				step, bound, where, bound)
		}
	}
}

// packageDurationVars returns the package-level duration vars the product
// declares in this directory, and how many product files it read. Consts
// cannot be assigned, so a const bound is already out of a test's reach.
func packageDurationVars(t *testing.T, fset *gotoken.FileSet) (map[string]bool, int) {
	t.Helper()
	bounds := map[string]bool{}
	var productFiles int
	for _, name := range packageGoFiles(t) {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := goparser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		productFiles++
		for _, decl := range file.Decls {
			gen, ok := decl.(*goast.GenDecl)
			if !ok || gen.Tok != gotoken.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*goast.ValueSpec)
				if !ok || !isDurationSpec(vs) {
					continue
				}
				for _, id := range vs.Names {
					bounds[id.Name] = true
				}
			}
		}
	}
	return bounds, productFiles
}

// boundsShortenedByHermeticTests maps each of those bounds a hermetic test
// assigns to the file:line where it does so, and returns how many hermetic
// test files it read. Live tests are exempt for the same reason 🎯T33
// exempts them: they bound a real network, not a fixture.
func boundsShortenedByHermeticTests(t *testing.T, fset *gotoken.FileSet, bounds map[string]bool) (map[string]string, int) {
	t.Helper()
	shortened := map[string]string{}
	var scanned int
	for _, name := range packageGoFiles(t) {
		if !strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, "_live_test.go") {
			continue
		}
		file, err := goparser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		goast.Inspect(file, func(n goast.Node) bool {
			assign, ok := n.(*goast.AssignStmt)
			if !ok || assign.Tok != gotoken.ASSIGN {
				return true
			}
			for _, lhs := range assign.Lhs {
				id, ok := lhs.(*goast.Ident)
				if !ok || !bounds[id.Name] {
					continue
				}
				// A shortening test also restores the bound in its
				// cleanup; one report per var, not per assignment.
				if _, seen := shortened[id.Name]; seen {
					continue
				}
				shortened[id.Name] = name + ":" + strconv.Itoa(fset.Position(id.Pos()).Line)
			}
			return true
		})
	}
	return shortened, scanned
}

func packageGoFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if name := e.Name(); strings.HasSuffix(name, ".go") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
