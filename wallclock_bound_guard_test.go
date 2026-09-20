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
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := gotoken.NewFileSet()

	// Package-level duration vars declared by the product. Consts cannot be
	// assigned, so a const bound is already out of a test's reach.
	bounds := map[string]bool{}
	var productFiles int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
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

	// Hermetic tests that write to one. Live tests are exempt for the same
	// reason 🎯T33 exempts them: they bound a real network, not a fixture.
	shortened := map[string]string{}
	var scanned int
	for _, e := range entries {
		name := e.Name()
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
				where := name + ":" + strconv.Itoa(fset.Position(id.Pos()).Line)
				if _, seen := shortened[id.Name]; seen {
					// A shortening test also restores the bound in its
					// cleanup; one report per var, not per assignment.
					continue
				}
				shortened[id.Name] = where
				if shortenableBounds[id.Name] == "" {
					t.Errorf("%s: hermetic test writes product bound %s, which is not in "+
						"shortenableBounds (🎯T93). A test that arms a product clock against its "+
						"own assertion must say which single step that bound speaks for and what "+
						"stops another step expiring first.", where, id.Name)
				}
			}
			return true
		})
	}

	// A guard that scanned nothing would pass forever.
	if productFiles == 0 || scanned == 0 {
		t.Fatalf("guard is not looking at anything: %d product files, %d hermetic test files",
			productFiles, scanned)
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
