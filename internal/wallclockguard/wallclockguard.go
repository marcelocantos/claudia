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
// # Clocks assembled by hand (🎯T106)
//
// The same disease has a second spelling that calls none of the banned
// constructors: a deadline built from time.Now() and compared —
// `for time.Now().Before(deadline)`, `if time.Since(start) > d`. The guard
// follows the clock through a function's locals (`start := time.Now()`,
// `deadline := start.Add(d)`), through methods and arithmetic on it
// (`time.Since(start).Milliseconds()`), and through package helpers that
// return it (`func timedGap(d) time.Duration`), and reports any <, >, <=,
// >= or .Before/.After/.Compare with a clock-derived operand. A clock that
// is only logged or stored decides nothing and is not reported. The same
// marker answers for a defensible comparison: a lower bound load can only
// pass, a bracket taken before and after the call, a stretched attempt that
// is retried rather than scored.
//
// What that tracking cannot see, and is residue rather than a pass: a clock
// reaching a comparison through a struct field, a channel, a closure
// variable captured from another function, a method value, or a helper in
// another package; and a comparison against a clock the PRODUCT reads
// internally. Taint is by name and flow-insensitive, which errs toward
// reporting — a shadowed name inherits its outer taint.
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
	NowComparison:          "compares a time.Now()-derived value, so host speed picks the branch",
}

// NowComparison names the second shape the guard reports (🎯T106): a
// deadline or an elapsed time assembled by hand from time.Now() and then
// compared — `for time.Now().Before(deadline)`, `if time.Since(start) > d`.
// No banned constructor is called, and the host's speed still decides.
const NowComparison = "time.Now comparison"

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

	nowFuncs := nowReturningFuncs(files)

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
			derived := nowDerivedLocals(fn.Body, nowFuncs)
			isNow := func(e ast.Expr) bool { return nowDerived(e, derived, nowFuncs) }

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
				var clock Clock
				var args []ast.Expr
				switch x := n.(type) {
				case *ast.CallExpr:
					if qualified, ok := qualifiedName(x.Fun); ok && Banned[qualified] != "" && qualified != NowComparison {
						clock, args = Clock{Call: qualified}, x.Args
					} else if isNowComparison(x, isNow) {
						clock, args = Clock{Call: NowComparison}, x.Args
					} else {
						return true
					}
				case *ast.BinaryExpr:
					switch x.Op {
					case token.LSS, token.GTR, token.LEQ, token.GEQ:
					default:
						return true
					}
					if !isNow(x.X) && !isNow(x.Y) {
						return true
					}
					clock, args = Clock{Call: NowComparison}, []ast.Expr{x.X, x.Y}
				default:
					return true
				}
				clock.File, clock.Line = name, fset.Position(n.Pos()).Line

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
					for _, arg := range args {
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

// isNowComparison reports a .Before/.After/.Compare call with a
// time.Now()-derived receiver or argument.
func isNowComparison(call *ast.CallExpr, isNow func(ast.Expr) bool) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "Before", "After", "Compare":
	default:
		return false
	}
	if isNow(sel.X) {
		return true
	}
	for _, a := range call.Args {
		if isNow(a) {
			return true
		}
	}
	return false
}

// nowDerived reports whether e carries the wall clock: a call to time.Now,
// time.Since or time.Until, or to a package func that returns one; a
// derived local; a method on a derived value (deadline.Add, elapsed.Seconds);
// arithmetic on one; or a numeric conversion of one. Arguments to any other
// call do not taint its result — that call is somebody else's function, and
// guessing at what it returns is how a guard starts crying wolf.
func nowDerived(e ast.Expr, locals map[string]bool, funcs map[string]bool) bool {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return nowDerived(x.X, locals, funcs)
	case *ast.Ident:
		return locals[x.Name]
	case *ast.UnaryExpr:
		return nowDerived(x.X, locals, funcs)
	case *ast.BinaryExpr:
		switch x.Op {
		case token.ADD, token.SUB, token.MUL, token.QUO, token.REM:
			return nowDerived(x.X, locals, funcs) || nowDerived(x.Y, locals, funcs)
		}
		return false
	case *ast.CallExpr:
		switch fun := x.Fun.(type) {
		case *ast.SelectorExpr:
			if q, ok := qualifiedName(fun); ok {
				switch q {
				case "time.Now", "time.Since", "time.Until":
					return true
				case "time.Duration":
					return len(x.Args) == 1 && nowDerived(x.Args[0], locals, funcs)
				}
			}
			return nowDerived(fun.X, locals, funcs)
		case *ast.Ident:
			if funcs[fun.Name] {
				return true
			}
			if numericConversion[fun.Name] && len(x.Args) == 1 {
				return nowDerived(x.Args[0], locals, funcs)
			}
		}
	}
	return false
}

var numericConversion = map[string]bool{
	"int": true, "int64": true, "uint64": true, "float32": true, "float64": true,
}

// nowDerivedLocals is the set of names in body assigned a derived value,
// flow-insensitively and to a fixpoint, so `start := time.Now()` then
// `deadline := start.Add(d)` taints both. Names, not objects: a shadowed
// name inherits its outer taint, which errs toward reporting.
func nowDerivedLocals(body *ast.BlockStmt, funcs map[string]bool) map[string]bool {
	locals := map[string]bool{}
	for changed := true; changed; {
		changed = false
		mark := func(lhs ast.Expr) {
			if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" && !locals[id.Name] {
				locals[id.Name] = true
				changed = true
			}
		}
		ast.Inspect(body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				if len(x.Lhs) == len(x.Rhs) {
					for i, r := range x.Rhs {
						if nowDerived(r, locals, funcs) {
							mark(x.Lhs[i])
						}
					}
				}
			case *ast.ValueSpec:
				if len(x.Names) == len(x.Values) {
					for i, v := range x.Values {
						if nowDerived(v, locals, funcs) {
							mark(x.Names[i])
						}
					}
				}
			}
			return true
		})
	}
	return locals
}

// nowReturningFuncs is the set of package funcs (by name) that return a
// derived value — `func elapsedSince(start time.Time) time.Duration { return
// time.Since(start) }` — found to a fixpoint so a helper of a helper counts.
func nowReturningFuncs(files []*ast.File) map[string]bool {
	funcs := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, f := range files {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil || funcs[fn.Name.Name] {
					continue
				}
				locals := nowDerivedLocals(fn.Body, funcs)
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if _, lit := n.(*ast.FuncLit); lit {
						return false
					}
					ret, ok := n.(*ast.ReturnStmt)
					if !ok || funcs[fn.Name.Name] {
						return !funcs[fn.Name.Name]
					}
					for _, r := range ret.Results {
						if nowDerived(r, locals, funcs) {
							funcs[fn.Name.Name] = true
							changed = true
						}
					}
					return true
				})
			}
		}
	}
	return funcs
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
