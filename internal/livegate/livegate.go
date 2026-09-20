// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Package livegate reads this module's live tests out of its own source and
// checks that the repo's live gate can actually reach every one of them.
//
// AGENTS.md's "Live tests" section is written as a hard gate — "Run every
// backend whose wire you touched… You are the gate" — and hands the reader a
// per-backend table of the tests that must be included. An agent that follows
// that section exactly consults the table, does not find the surface it
// touched, and correctly concludes that no live gate binds it. That is how a
// live test that exists still never runs: on 2026-09-21 the module defined
// thirty-eight live tests and `make live`'s -run expression named twenty-two,
// including none at all for the plan-usage wire, which had a live test and no
// row (T100).
//
// So the two documents that tell an agent what to run — the Makefile and
// AGENTS.md — are checked against the source, never against each other. Each
// live test carries two obligations:
//
//   - make-live: `make live` reaches it, by name AND by package. A test named
//     in the -run expression but sitting in a package the recipe does not
//     list is unreachable, which is how the Codex and Grok sub-package live
//     tests hid.
//   - agents-md: AGENTS.md names it, so an agent that touched that surface
//     finds a row instead of a silence.
//
// A test may be released from either obligation by an entry in
// live-gate-exclusions.json that states a reason. That is the honest answer
// for a live test nobody can afford to run on every gate; a list of ten
// unwired tests nobody decided about is not.
//
// # What counts as a live test
//
// Not the name. `TestT30LiveReproSendSucceeds` replays two committed frames
// and spends nothing; `TestT30LargePayloadSubmitsOnRealPath` spends a real
// turn and has no "Live" in it. A name scan is wrong in both directions, so
// the census is by BEHAVIOUR: a live test is a top-level test function that
// reads a live-gate environment variable (see LiveGateName).
//
// Reading that from source is not free — the variable may be a constant, a
// constant in another package, a field of a table-driven case, or the
// parameter of a helper the test body is a one-line call to:
//
//	func TestT96MeasureSilenceGrok(t *testing.T) { t96measure(t, ProviderGrok, "CLAUDIA_GROK_LIVE") }
//
// Every form this module uses is resolved — calls into package-local helpers
// are followed, binding their parameters to the caller's arguments — and a
// form that is not resolvable is reported as undecidable rather than dropped.
// A census that silently under-counts is the same defect one layer down.
//
// A test — or a helper a test calls — that reads an environment variable the
// census cannot resolve, and is nonetheless hermetic, says so where the
// reader is:
//
//	//livegate:hermetic reports what leaked; it un-skips nothing
//
// It is one line, because gofmt moves a directive to the end of a doc comment
// and would strand a second line of it above. That is a claim the check holds
// to account: a test carrying the directive
// that does resolve a live gate is a contradiction and fails, and a directive
// without a reason fails too. On a helper it means "nothing live in here",
// and the census does not descend past it.
package livegate

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/marcelocantos/claudia/internal/gowalk"
)

// Obligations a live test carries. An exclusion waives them by name.
const (
	ObligationMakeLive = "make-live"
	ObligationAgentsMD = "agents-md"
)

// Obligations is every obligation, in report order.
var Obligations = []string{ObligationMakeLive, ObligationAgentsMD}

// LiveGateName reports whether an environment variable name is a live gate:
// a CLAUDIA_ variable with LIVE as one of its underscore-separated words.
//
// The rule is a shape rather than a list on purpose. A list would have to be
// kept in step with the tests, and the whole failure this package exists to
// catch is a list that drifted from the source. CLAUDIA_LIVE,
// CLAUDIA_GROK_LIVE, CLAUDIA_MCP_OAUTH_LIVE and CLAUDIA_LIVE_SEND all match;
// the helper handshakes (CLAUDIA_CRASH_TEST_HELPER) do not.
func LiveGateName(s string) bool {
	if !strings.HasPrefix(s, "CLAUDIA_") {
		return false
	}
	for _, word := range strings.Split(s, "_") {
		if word == "LIVE" {
			return true
		}
	}
	return false
}

// A Test is one top-level test function that skips unless a live gate is set.
type Test struct {
	Name  string   // the Go test function name
	Pkg   string   // module-relative directory; "." for the root package
	File  string   // module-relative path
	Line  int      // line of the func declaration
	Gates []string // sorted live-gate variables it reads
}

// Pos renders the declaration site the way a compiler would.
func (t Test) Pos() string { return fmt.Sprintf("%s:%d", t.File, t.Line) }

// HermeticDirective is how a test whose environment reads are unresolvable
// declares that it is not a live test. The text after it is the reason, and
// is required.
const HermeticDirective = "//livegate:hermetic"

// An Undecidable is a test whose live gating could not be read from source:
// it calls os.Getenv with an argument this package cannot resolve to a string
// and names no live-gate literal anywhere in its body. It is a hard failure,
// not a skip — the test may or may not be live, and guessing which is how the
// census starts lying.
type Undecidable struct {
	Name string
	File string
	Line int
	Why  string // what stopped the census, and what to do about it
}

// Pos renders the declaration site the way a compiler would.
func (u Undecidable) Pos() string { return fmt.Sprintf("%s:%d", u.File, u.Line) }

// A Census is what Enumerate read out of a module's test sources.
type Census struct {
	Live        []Test        // sorted by name
	Undecidable []Undecidable // sorted by position
	Gates       []string      // every live-gate variable seen, sorted
	AllTests    []string      // every top-level test function, live or not, sorted
}

// Enumerate reads root's test sources and returns every live test in them.
//
// It walks the directories the go command itself would walk (gowalk.IgnoredDir),
// so the census covers exactly the files `go test ./...` compiles — a scratch
// file under _scratchpad/ is invisible here for the same reason it is
// invisible to vet (T99).
func Enumerate(root string) (Census, error) {
	fset := token.NewFileSet()

	// Constants first, across the whole module: a test may name its gate
	// through a constant in its own package (t30LiveEnv) or in another
	// (broker.NoBrokerEnv), and both have to resolve before any func is read.
	byDir := map[string]map[string]string{}          // dir -> const -> value
	byPkg := map[string]map[string]string{}          // package name -> const -> value
	files := map[string][]*ast.File{}                // dir -> parsed test files
	fileName := map[*ast.File]string{}               // parsed file -> module-relative path
	helpers := map[string]map[string]*ast.FuncDecl{} // dir -> func name -> decl
	hermetic := map[string]map[string]bool{}         // dir -> func name -> claimed hermetic
	var reasonless []Undecidable                     // directives that state no reason

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && gowalk.IgnoredDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		dir := filepath.Dir(path)
		pkg := strings.TrimSuffix(f.Name.Name, "_test")
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			if helpers[dir] == nil {
				helpers[dir] = map[string]*ast.FuncDecl{}
				hermetic[dir] = map[string]bool{}
			}
			helpers[dir][fn.Name.Name] = fn
			if claimed, reason := hermeticClaim(f, fn); claimed {
				hermetic[dir][fn.Name.Name] = true
				if strings.TrimSpace(reason) == "" {
					reasonless = append(reasonless, Undecidable{
						Name: fn.Name.Name, File: rel, Line: fset.Position(fn.Pos()).Line,
						Why: "declares " + HermeticDirective + " without stating why. " +
							"The reason is the whole value of the directive.",
					})
				}
			}
		}
		for name, val := range stringConsts(f) {
			if byDir[dir] == nil {
				byDir[dir] = map[string]string{}
			}
			byDir[dir][name] = val
			if byPkg[pkg] == nil {
				byPkg[pkg] = map[string]string{}
			}
			byPkg[pkg][name] = val
		}
		if strings.HasSuffix(path, "_test.go") {
			files[dir] = append(files[dir], f)
			fileName[f] = rel
		}
		return nil
	})
	if err != nil {
		return Census{}, err
	}

	var c Census
	gates := map[string]bool{}
	for dir, fs := range files {
		pkgDir, rerr := filepath.Rel(root, dir)
		if rerr != nil {
			return Census{}, rerr
		}
		pkgDir = filepath.ToSlash(pkgDir)
		for _, f := range fs {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || !isTestFunc(fn) {
					continue
				}
				c.AllTests = append(c.AllTests, fn.Name.Name)
				found, unresolved := gatesOf(fn, byDir[dir], byPkg, helpers[dir], hermetic[dir])
				line := fset.Position(fn.Pos()).Line
				claimed := hermetic[dir][fn.Name.Name]
				if claimed && len(found) > 0 {
					c.Undecidable = append(c.Undecidable, Undecidable{
						Name: fn.Name.Name, File: fileName[f], Line: line,
						Why: "declares " + HermeticDirective + " and yet reads " +
							strings.Join(found, "+") + ". One of the two is wrong.",
					})
					continue
				}
				if claimed {
					continue
				}
				if len(found) == 0 {
					if unresolved != "" {
						c.Undecidable = append(c.Undecidable, Undecidable{
							Name: fn.Name.Name, File: fileName[f], Line: line,
							Why: "os.Getenv(" + unresolved + ") does not resolve to a string and the " +
								"body names no live gate, so this census cannot tell whether the test " +
								"is live. Name the variable, put the literal in the function, or declare " +
								HermeticDirective + " with a reason.",
						})
					}
					continue
				}
				sort.Strings(found)
				for _, g := range found {
					gates[g] = true
				}
				c.Live = append(c.Live, Test{
					Name: fn.Name.Name, Pkg: pkgDir, File: fileName[f], Line: line, Gates: found,
				})
			}
		}
	}
	c.Undecidable = append(c.Undecidable, reasonless...)
	sort.Strings(c.AllTests)
	sort.Slice(c.Live, func(i, j int) bool { return c.Live[i].Name < c.Live[j].Name })
	sort.Slice(c.Undecidable, func(i, j int) bool { return c.Undecidable[i].Pos() < c.Undecidable[j].Pos() })
	for g := range gates {
		c.Gates = append(c.Gates, g)
	}
	sort.Strings(c.Gates)
	return c, nil
}

// isTestFunc reports whether fn is a top-level test the go command would run.
// TestMain is the harness, not a test, and reads environment variables of its
// own (broker.NoBrokerEnv) that have nothing to do with live gating.
func isTestFunc(fn *ast.FuncDecl) bool {
	if fn.Recv != nil || fn.Name.Name == "TestMain" || !strings.HasPrefix(fn.Name.Name, "Test") {
		return false
	}
	if len(fn.Name.Name) > len("Test") {
		if r := fn.Name.Name[len("Test")]; r >= 'a' && r <= 'z' {
			return false // TestingHelper-style name; go test ignores it
		}
	}
	return fn.Body != nil
}

// stringConsts returns the file's top-level `const name = "value"` bindings.
func stringConsts(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if s, ok := stringLit(vs.Values[i]); ok {
					out[name.Name] = s
				}
			}
		}
	}
	return out
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// gatesOf returns the live gates fn reads, and — when some os.Getenv argument
// could not be resolved and nothing else in the body settled the question —
// that argument as written.
//
// The fallback for an unresolved argument is the live-gate string literals in
// fn's own body, which is what a table-driven live test looks like:
//
//	cases := []struct{ gate string; provider Provider }{{"CLAUDIA_LIVE", …}}
//	… if os.Getenv(tc.gate) == "" { t.Skipf(…) }
//
// tc.gate is not resolvable without type information, but the literals two
// lines up say exactly which gates the test spends. Resolving that by hand
// would be guessing; reading it out of the same function is not.
func gatesOf(fn *ast.FuncDecl, dirConsts map[string]string, byPkg map[string]map[string]string, pkgFuncs map[string]*ast.FuncDecl, hermetic map[string]bool) (gates []string, unresolved string) {
	seen := map[string]bool{}
	visit(fn, nil, dirConsts, byPkg, pkgFuncs, hermetic, map[string]bool{fn.Name.Name: true}, 0, seen, &unresolved)
	if unresolved != "" && len(seen) == 0 {
		ast.Inspect(fn, func(n ast.Node) bool {
			if e, ok := n.(ast.Expr); ok {
				if v, ok := stringLit(e); ok && LiveGateName(v) {
					seen[v] = true
				}
			}
			return true
		})
		if len(seen) > 0 {
			unresolved = ""
		}
	}
	for g := range seen {
		gates = append(gates, g)
	}
	return gates, unresolved
}

// callDepth bounds how far a helper chain is followed. Three is past every
// shape this module uses and stops a mutually recursive pair cold; the
// visited set already does, but a bound that does not depend on being right
// about that is cheaper than being right about it.
const callDepth = 3

// visit walks one function body looking for live-gate reads, following calls
// into package-local helpers with their parameters bound to the caller's
// arguments. bindings maps a parameter name in THIS function to the string
// the caller passed.
func visit(fn *ast.FuncDecl, bindings map[string]string, dirConsts map[string]string,
	byPkg map[string]map[string]string, pkgFuncs map[string]*ast.FuncDecl, hermetic map[string]bool,
	onStack map[string]bool, depth int, seen map[string]bool, unresolved *string) {

	resolve := func(e ast.Expr) (string, bool) {
		if id, ok := e.(*ast.Ident); ok {
			if v, ok := bindings[id.Name]; ok {
				return v, true
			}
		}
		return resolveEnvArg(e, dirConsts, byPkg)
	}

	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Getenv" && len(call.Args) == 1 {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "os" {
				name, ok := resolve(call.Args[0])
				if !ok {
					if *unresolved == "" {
						*unresolved = exprString(call.Args[0])
					}
				} else if LiveGateName(name) {
					seen[name] = true
				}
				return true
			}
		}
		callee, ok := call.Fun.(*ast.Ident)
		if !ok || depth >= callDepth || onStack[callee.Name] || hermetic[callee.Name] {
			return true
		}
		target := pkgFuncs[callee.Name]
		if target == nil || target.Type.Params == nil {
			return true
		}
		inner := map[string]string{}
		i := 0
		for _, field := range target.Type.Params.List {
			for _, name := range field.Names {
				if i < len(call.Args) {
					if v, ok := resolve(call.Args[i]); ok {
						inner[name.Name] = v
					}
				}
				i++
			}
		}
		onStack[callee.Name] = true
		visit(target, inner, dirConsts, byPkg, pkgFuncs, hermetic, onStack, depth+1, seen, unresolved)
		delete(onStack, callee.Name)
		return true
	})
}

func resolveEnvArg(e ast.Expr, dirConsts map[string]string, byPkg map[string]map[string]string) (string, bool) {
	switch a := e.(type) {
	case *ast.BasicLit:
		return stringLit(a)
	case *ast.Ident:
		v, ok := dirConsts[a.Name]
		return v, ok
	case *ast.SelectorExpr:
		pkg, ok := a.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		v, ok := byPkg[pkg.Name][a.Sel.Name]
		return v, ok
	}
	return "", false
}

// hermeticClaim reports whether fn carries the //livegate:hermetic directive,
// in its doc comment or anywhere inside its body, and the reason it states.
func hermeticClaim(f *ast.File, fn *ast.FuncDecl) (bool, string) {
	for _, cg := range f.Comments {
		if cg.End() < fn.Pos() && (fn.Doc == nil || cg != fn.Doc) {
			continue
		}
		if cg.Pos() > fn.End() {
			continue
		}
		for _, c := range cg.List {
			if rest, ok := strings.CutPrefix(c.Text, HermeticDirective); ok {
				return true, strings.TrimSpace(rest)
			}
		}
	}
	return false, ""
}

func exprString(e ast.Expr) string {
	switch a := e.(type) {
	case *ast.Ident:
		return a.Name
	case *ast.SelectorExpr:
		return exprString(a.X) + "." + a.Sel.Name
	case *ast.BasicLit:
		return a.Value
	}
	return fmt.Sprintf("%T", e)
}

// A LiveTarget is `make live` as the Makefile actually spells it: the -run
// expression and the package patterns, read from the recipe rather than
// restated here.
type LiveTarget struct {
	Run  string
	Pkgs []string
}

var (
	liveRecipe = regexp.MustCompile(`(?m)^live:\n((?:\t.*\n)+)`)
	runFlag    = regexp.MustCompile(`-run\s+'([^']*)'`)
)

// ParseLiveTarget reads the `live:` recipe out of a Makefile.
func ParseLiveTarget(makefile []byte) (LiveTarget, error) {
	m := liveRecipe.FindSubmatch(makefile)
	if m == nil {
		return LiveTarget{}, fmt.Errorf("no `live:` recipe in the Makefile")
	}
	recipe := string(m[1])
	run := runFlag.FindStringSubmatch(recipe)
	if run == nil {
		return LiveTarget{}, fmt.Errorf("the `live:` recipe has no -run '…' expression")
	}
	rest := recipe[strings.Index(recipe, run[0])+len(run[0]):]
	var pkgs []string
	for _, f := range strings.Fields(rest) {
		if strings.HasPrefix(f, "-") {
			continue
		}
		pkgs = append(pkgs, f)
	}
	if len(pkgs) == 0 {
		return LiveTarget{}, fmt.Errorf("the `live:` recipe names no packages")
	}
	return LiveTarget{Run: run[1], Pkgs: pkgs}, nil
}

// Alternatives splits the -run expression into its top-level alternatives.
// The expression this Makefile uses is a flat `A|B|C`; anything with grouping
// is returned whole, so the dead-name check reads it as one pattern rather
// than inventing sub-patterns that go test would never form.
func (lt LiveTarget) Alternatives() []string {
	if strings.ContainsAny(lt.Run, "()") {
		return []string{lt.Run}
	}
	return strings.Split(lt.Run, "|")
}

// Reaches reports whether `make live` would run t: whether the -run
// expression matches the name the way go test matches it (unanchored, RE2),
// and whether t's package is one the recipe lists.
func (lt LiveTarget) Reaches(t Test) (name, pkg bool, err error) {
	re, err := regexp.Compile(lt.Run)
	if err != nil {
		return false, false, fmt.Errorf("the `live:` -run expression does not compile: %w", err)
	}
	for _, p := range lt.Pkgs {
		if matchPkgPattern(p, t.Pkg) {
			pkg = true
			break
		}
	}
	return re.MatchString(t.Name), pkg, nil
}

// matchPkgPattern compares a go-command package pattern against a
// module-relative directory. Only the forms this Makefile uses are honoured —
// ".", "./dir/", and a "./..." wildcard — and anything else is refused by
// returning false, so an unrecognised pattern reads as unreachable rather
// than as blanket coverage.
func matchPkgPattern(pattern, dir string) bool {
	p := strings.TrimSuffix(strings.TrimPrefix(pattern, "./"), "/")
	switch {
	case p == "" || p == ".":
		return dir == "."
	case p == "...":
		return true
	case strings.HasSuffix(p, "/..."):
		base := strings.TrimSuffix(p, "/...")
		return dir == base || strings.HasPrefix(dir, base+"/")
	default:
		return dir == p
	}
}

// An Exclusion releases one test from one or more obligations, with a reason.
type Exclusion struct {
	Test   string   `json:"test"`
	Waives []string `json:"waives"`
	Reason string   `json:"reason"`
}

// Exclusions is live-gate-exclusions.json.
type Exclusions struct {
	Comment    string      `json:"//,omitempty"`
	Exclusions []Exclusion `json:"exclusions"`
}

// LoadExclusions reads the exclusions file. A missing file is an empty list:
// the check is meaningful before anyone has excluded anything.
func LoadExclusions(path string) (Exclusions, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Exclusions{}, nil
	}
	if err != nil {
		return Exclusions{}, err
	}
	var x Exclusions
	if err := json.Unmarshal(b, &x); err != nil {
		return Exclusions{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return x, nil
}

// A Violation is one thing wrong, phrased so the reader knows what to do.
type Violation struct {
	Where string // test name, gate variable, or exclusion entry
	Want  string
}

func (v Violation) String() string { return v.Where + ": " + v.Want }

// Check is the whole verdict, and takes its inputs rather than reading them,
// so the teeth proof can drive it with a census containing a test nobody
// wired and watch it go red.
//
// registry is testctlenv.LiveGates(): the variables claudia strips from a
// spawned agent's environment. A live gate missing from it is the T20 leak —
// a fleet agent inherits it ambiently and silently runs a different suite.
func Check(c Census, lt LiveTarget, agentsMD string, x Exclusions, registry []string) []Violation {
	var vs []Violation

	for _, u := range c.Undecidable {
		vs = append(vs, Violation{u.Name, "at " + u.Pos() + " " + u.Why})
	}

	waived := map[string]map[string]string{} // test -> obligation -> reason
	used := map[string]map[string]bool{}
	known := map[string]bool{}
	for _, t := range c.Live {
		known[t.Name] = true
	}
	for _, e := range x.Exclusions {
		if strings.TrimSpace(e.Reason) == "" {
			vs = append(vs, Violation{"exclusion " + e.Test, "states no reason. An exclusion without one is an unwired live test with paperwork."})
			continue
		}
		if !known[e.Test] {
			vs = append(vs, Violation{"exclusion " + e.Test, "excludes a test this module has no live test for. Delete the entry, or restore the test."})
			continue
		}
		if len(e.Waives) == 0 {
			vs = append(vs, Violation{"exclusion " + e.Test, "waives nothing. Name " + strings.Join(Obligations, " and/or ") + "."})
			continue
		}
		if waived[e.Test] == nil {
			waived[e.Test] = map[string]string{}
			used[e.Test] = map[string]bool{}
		}
		for _, o := range e.Waives {
			if o != ObligationMakeLive && o != ObligationAgentsMD {
				vs = append(vs, Violation{"exclusion " + e.Test, "waives unknown obligation " + strconv.Quote(o) + "."})
				continue
			}
			waived[e.Test][o] = e.Reason
		}
	}

	for _, t := range c.Live {
		nameOK, pkgOK, err := lt.Reaches(t)
		if err != nil {
			vs = append(vs, Violation{"Makefile", err.Error()})
			return vs
		}
		reach := nameOK && pkgOK
		if reason, ok := waived[t.Name][ObligationMakeLive]; ok {
			used[t.Name][ObligationMakeLive] = true
			if reach {
				vs = append(vs, Violation{t.Name, "is excluded from `make live` (" + reason +
					") but `make live` reaches it anyway. Drop the exclusion."})
			}
		} else if !reach {
			why := "its name is not in `make live`'s -run expression"
			if nameOK && !pkgOK {
				why = "`make live` names it but does not list its package " + strconv.Quote(t.Pkg) +
					", so -run never sees it"
			}
			vs = append(vs, Violation{t.Name, "at " + t.Pos() + " spends " + strings.Join(t.Gates, "+") +
				" and " + why + ". Wire it into the `live:` recipe, or exclude it with a reason in " +
				ExclusionsFile + "."})
		}

		named := namedIn(agentsMD, t.Name)
		if reason, ok := waived[t.Name][ObligationAgentsMD]; ok {
			used[t.Name][ObligationAgentsMD] = true
			if named {
				vs = append(vs, Violation{t.Name, "is excluded from AGENTS.md (" + reason +
					") but AGENTS.md names it anyway. Drop the exclusion."})
			}
		} else if !named {
			vs = append(vs, Violation{t.Name, "at " + t.Pos() + " is not named in AGENTS.md's live-test table. " +
				"An agent that changes this surface reads that table and finds no row, so no gate binds it. " +
				"Add it, or exclude it with a reason in " + ExclusionsFile + "."})
		}
	}

	for _, e := range x.Exclusions {
		for _, o := range e.Waives {
			if known[e.Test] && !used[e.Test][o] && (o == ObligationMakeLive || o == ObligationAgentsMD) {
				vs = append(vs, Violation{"exclusion " + e.Test, "waives " + o + " twice."})
			}
		}
	}

	// A -run alternative that matches no test at all is the same decay read
	// from the other end: a renamed or deleted test leaves a name in the
	// recipe that reads like coverage and runs nothing.
	for _, alt := range lt.Alternatives() {
		re, err := regexp.Compile(alt)
		if err != nil {
			vs = append(vs, Violation{"Makefile", "the `live:` -run alternative " + strconv.Quote(alt) + " does not compile: " + err.Error()})
			continue
		}
		matched := false
		for _, name := range c.AllTests {
			if re.MatchString(name) {
				matched = true
				break
			}
		}
		if !matched {
			vs = append(vs, Violation{"Makefile", "the `live:` -run expression names " + strconv.Quote(alt) +
				", which matches no test in this module. It reads like coverage and runs nothing."})
		}
	}

	inRegistry := map[string]bool{}
	for _, g := range registry {
		inRegistry[g] = true
	}
	for _, g := range c.Gates {
		if !inRegistry[g] {
			vs = append(vs, Violation{g, "is a live gate this module's tests read, but internal/testctlenv.LiveGates() " +
				"does not list it, so claudia does not strip it from a spawned agent's environment. " +
				"A fleet agent inherits it and runs a different suite (T20)."})
		}
	}

	sort.Slice(vs, func(i, j int) bool { return vs[i].Where < vs[j].Where })
	return vs
}

// ExclusionsFile is the module-root file that records deliberate exclusions.
const ExclusionsFile = "live-gate-exclusions.json"

func namedIn(doc, name string) bool {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
	return re.MatchString(doc)
}

// ModuleRoot walks up from dir to the directory holding go.mod.
func ModuleRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(abs, "go.mod")); err == nil {
			return abs, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		abs = parent
	}
}
