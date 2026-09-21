// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package wallclockguard finds wall clocks a hermetic test writes for itself
// and that could decide its verdict on a loaded host (🎯T33, 🎯T97).
//
// The disease it guards against: a hermetic test bounds a wait with
// context.WithTimeout or races a select against time.After, and when the
// machine is busy the clock expires before the fixture answers. The test then
// fails, or worse passes, on the host's speed rather than the product's
// behaviour — and a green suite stops being citable evidence. The repair is
// to wait on the event the test needs and let `go test -timeout` be the only
// clock.
//
// Not every clock in a test decides a verdict, so a blanket ban is the wrong
// shape. A clock is defensible when a slow host cannot turn it into a wrong
// answer: it bounds a wait that must NOT return (slowness only strengthens the
// assertion), it is a failsafe on an event the test is already waiting on and
// only converts a hang into a failure, or it paces a fake peer. Such a clock
// stays, and says why where the reader is:
//
//	// 🎯T97 exemption: the 300ms bounds a wait that must not return, so a
//	// slow host can only strengthen the assertion.
//
// The marker is the one cl-t98 used first in wait_for_response_turn_test.go,
// and the text after it is the reason, which is required. It exempts a clock
// when it is found in any of three places:
//
//   - the doc comment of the function the clock is in;
//   - a comment ending on the line above the clock's statement, or on the
//     clock's own line;
//   - the doc comment of a const or var the clock's arguments name, so one
//     reason covers a failsafe constant wherever it is used.
//
// A marker that exempts nothing is reported, so a reason cannot outlive the
// clock it was written for.
//
// Examples with no output comment are out of scope too: go test compiles
// them and never runs them, so their clocks are documentation.
//
// Live tests are out of scope: they drive a real backend, where a deadline
// bounds a genuinely unbounded wait. That is decided by behaviour, not by
// name — a test is live when livegate's census says it reads a live gate —
// plus the old *_live_test.go filename rule, which covers files whose every
// test is live.
//
// # The half this cannot see
//
// A source scan of tests sees clocks a test WRITES. It cannot see a clock
// that lives in the product and that a test merely ARMS by shortening a
// package-level bound — 🎯T93's shape, where codex_session_test.go set a
// product timeout to 200ms and nothing in the test looked like a deadline.
// That half is TestHermeticTestsDeclareTheProductBoundsTheyShorten in the
// root package (wallclock_bound_guard_test.go), which makes every such
// assignment declared with a reason. The two guards are complementary and
// this package does not duplicate the other.
package wallclockguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/marcelocantos/claudia/internal/gowalk"
	"github.com/marcelocantos/claudia/internal/livegate"
)

// Marker introduces an in-source exemption. The reason follows it.
const Marker = "🎯T97 exemption"

// Banned maps each clock constructor to how its expiry can outvote an
// assertion. time.Sleep is absent on purpose: it delays a test but cannot by
// itself fail one.
var Banned = map[string]string{
	"context.WithTimeout":  "deadline can expire before the fixture answers",
	"context.WithDeadline": "deadline can expire before the fixture answers",
	"time.After":           "fires against whatever the select is racing",
	"time.Tick":            "fires against whatever the select is racing",
	"time.NewTimer":        "fires against whatever the select is racing",
	"time.NewTicker":       "fires against whatever the select is racing",
}

// A Clock is one banned call in a hermetic test.
type Clock struct {
	File   string // path relative to the scanned directory
	Line   int
	Call   string // e.g. "context.WithTimeout"
	Reason string // the exemption's reason; empty when unexempted
}

// Pos renders the call site the way a compiler would.
func (c Clock) Pos() string { return fmt.Sprintf("%s:%d", c.File, c.Line) }

// A Marker problem is an exemption that is not doing its job.
type MarkerProblem struct {
	File string
	Line int
	Why  string
}

// Pos renders the marker's site the way a compiler would.
func (m MarkerProblem) Pos() string { return fmt.Sprintf("%s:%d", m.File, m.Line) }

// A Report is what Scan read out of one package directory.
type Report struct {
	Scanned     int             // hermetic test files read
	ScannedDirs int             // package directories with at least one of them
	Violations  []Clock         // unexempted clocks: each one fails the guard
	Exempted    []Clock         // clocks a marker answered for
	Markers     []MarkerProblem // markers with no reason, or that exempt nothing
}

// Scan reads the hermetic test files in dir, one package directory of the
// module rooted at moduleRoot, and reports every banned clock in them.
// Positions are relative to dir.
func Scan(moduleRoot, dir string) (Report, error) {
	census, err := livegate.Enumerate(moduleRoot)
	if err != nil {
		return Report{}, fmt.Errorf("live-test census: %w", err)
	}
	return scan(census, moduleRoot, dir, "")
}

// ScanModule is Scan over every package directory the go command would
// compile under moduleRoot (gowalk.IgnoredDir), with positions relative to
// moduleRoot, so no package can be the one the guard never looked at.
func ScanModule(moduleRoot string) (Report, error) {
	var rep Report
	census, err := livegate.Enumerate(moduleRoot)
	if err != nil {
		return rep, fmt.Errorf("live-test census: %w", err)
	}
	var dirs []string
	err = filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != moduleRoot && gowalk.IgnoredDir(d.Name()) {
				return filepath.SkipDir
			}
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return rep, err
	}
	for _, dir := range dirs {
		rel, err := filepath.Rel(moduleRoot, dir)
		if err != nil {
			return rep, err
		}
		prefix := ""
		if rel != "." {
			prefix = filepath.ToSlash(rel) + "/"
		}
		r, err := scan(census, moduleRoot, dir, prefix)
		if err != nil {
			return rep, err
		}
		rep.Scanned += r.Scanned
		if r.Scanned > 0 {
			rep.ScannedDirs++
		}
		rep.Violations = append(rep.Violations, r.Violations...)
		rep.Exempted = append(rep.Exempted, r.Exempted...)
		rep.Markers = append(rep.Markers, r.Markers...)
	}
	return rep, nil
}

// scan is Scan against a census already taken, naming files prefix+name.
func scan(census livegate.Census, moduleRoot, dir, prefix string) (Report, error) {
	var rep Report
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return rep, err
	}
	absRoot, err := filepath.Abs(moduleRoot)
	if err != nil {
		return rep, err
	}
	live := map[string]bool{} // "file.go:Name" for live tests in dir
	for _, t := range census.Live {
		if filepath.Join(absRoot, filepath.FromSlash(t.Pkg)) == absDir {
			live[filepath.Base(t.File)+":"+t.Name] = true
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return rep, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	names := map[*ast.File]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, "_live_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return rep, err
		}
		files = append(files, f)
		names[f] = prefix + name
	}
	rep.Scanned = len(files)

	// Every marker in the scanned files, keyed by its comment group, so the
	// ones that exempt nothing can be reported at the end.
	type marker struct {
		file   string
		line   int
		reason string
		used   bool
	}
	markers := map[*ast.CommentGroup]*marker{}
	for _, f := range files {
		for _, cg := range f.Comments {
			reason, ok := markerReason(cg.Text())
			if !ok {
				continue
			}
			m := &marker{file: names[f], line: fset.Position(cg.Pos()).Line, reason: reason}
			markers[cg] = m
			if reason == "" {
				rep.Markers = append(rep.Markers, MarkerProblem{m.file, m.line,
					"exemption states no reason — say why this clock cannot outvote the assertion"})
			}
		}
	}
	valid := func(cg *ast.CommentGroup) *marker {
		if m := markers[cg]; m != nil && m.reason != "" {
			return m
		}
		return nil
	}

	// Package-level consts and vars whose declaration carries a marker. A
	// clock that names one is answered for by that declaration.
	declared := map[string]*marker{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				m := valid(vs.Doc)
				if m == nil {
					m = valid(gen.Doc)
				}
				if m == nil {
					continue
				}
				for _, id := range vs.Names {
					declared[id.Name] = m
				}
			}
		}
	}

	for _, f := range files {
		name := names[f]
		// A comment ending on line L answers for a statement starting on
		// L+1, or for a clock on line L itself.
		endingOn := map[int]*marker{}
		for _, cg := range f.Comments {
			if m := valid(cg); m != nil {
				endingOn[fset.Position(cg.End()).Line] = m
			}
		}

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if fn.Recv == nil && live[filepath.Base(name)+":"+fn.Name.Name] {
				continue
			}
			if fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Example") && !runsAsTest(f, fn) {
				continue
			}
			fnMarker := valid(fn.Doc)

			// The statement each node sits in, innermost first, so a comment
			// above a multi-line statement answers for a clock inside it.
			var stmts []ast.Stmt
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if n == nil {
					stmts = stmts[:len(stmts)-1]
					return true
				}
				s, isStmt := n.(ast.Stmt)
				if isStmt {
					stmts = append(stmts, s)
				} else {
					stmts = append(stmts, nil)
				}
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				qualified, ok := qualifiedName(call.Fun)
				if !ok || Banned[qualified] == "" {
					return true
				}
				clock := Clock{File: name, Line: fset.Position(call.Pos()).Line, Call: qualified}

				var by *marker
				if m := endingOn[clock.Line]; m != nil {
					by = m
				}
				for i := len(stmts) - 1; by == nil && i >= 0; i-- {
					if stmts[i] != nil {
						by = endingOn[fset.Position(stmts[i].Pos()).Line-1]
					}
				}
				if by == nil {
					for _, arg := range call.Args {
						ast.Inspect(arg, func(a ast.Node) bool {
							if id, ok := a.(*ast.Ident); ok && by == nil {
								by = declared[id.Name]
							}
							return by == nil
						})
					}
				}
				if by == nil {
					by = fnMarker
				}

				if by == nil {
					rep.Violations = append(rep.Violations, clock)
				} else {
					by.used = true
					clock.Reason = by.reason
					rep.Exempted = append(rep.Exempted, clock)
				}
				return true
			})
		}
	}

	for _, m := range markers {
		if m.reason != "" && !m.used {
			rep.Markers = append(rep.Markers, MarkerProblem{m.file, m.line,
				"exemption answers for no clock — delete it, or move it to the clock it was written for"})
		}
	}

	sortClocks(rep.Violations)
	sortClocks(rep.Exempted)
	sort.Slice(rep.Markers, func(i, j int) bool {
		a, b := rep.Markers[i], rep.Markers[j]
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Line < b.Line
	})
	return rep, nil
}

// outputComment is how go test recognises an example it will run.
var outputComment = regexp.MustCompile(`(?i)^[[:space:]]*(unordered )?output:`)

// runsAsTest reports whether an Example function carries an output comment.
// One that does not is compiled by go test but never executed, so no clock
// in it can decide a verdict.
func runsAsTest(f *ast.File, fn *ast.FuncDecl) bool {
	for _, cg := range f.Comments {
		if cg.Pos() >= fn.Body.Lbrace && cg.End() <= fn.Body.Rbrace && outputComment.MatchString(cg.Text()) {
			return true
		}
	}
	return false
}

// markerReason reports whether text carries the exemption marker at the
// start of one of its lines, and the reason that follows it with leading
// punctuation and space trimmed. Prose that merely names the marker mid-line
// is not an exemption.
func markerReason(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		after, found := strings.CutPrefix(strings.TrimSpace(line), Marker)
		if !found {
			continue
		}
		rest := strings.Join(append([]string{after}, lines[i+1:]...), " ")
		rest = strings.TrimLeftFunc(rest, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsPunct(r)
		})
		return strings.Join(strings.Fields(rest), " "), true
	}
	return "", false
}

// qualifiedName renders pkg.Func for a package-qualified call.
func qualifiedName(fun ast.Expr) (string, bool) {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return pkg.Name + "." + sel.Sel.Name, true
}

func sortClocks(cs []Clock) {
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].File != cs[j].File {
			return cs[i].File < cs[j].File
		}
		return cs[i].Line < cs[j].Line
	})
}
