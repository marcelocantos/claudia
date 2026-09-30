// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"bufio"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/internal/gowalk"
)

// 🎯T13. Claudia is a library that any program embeds; Jevons is one of its
// consumers, not its owner. The dependency arrow points one way: jevons
// requires claudia, and claudia never requires jevons. The owner's concern
// (2026-08-03) was that Jevons would pollute Claudia until it was "just a
// Jevons backend", and the cheapest place that happens is an import — one
// `github.com/marcelocantos/jevons/...` line and every non-Jevons consumer
// now builds the whole fleet daemon to embed an agent.
//
// This file pins the invariant three ways, all hermetic:
//
//   - go.mod and go.sum name no jevons module, directly or transitively;
//   - no .go file the toolchain compiles imports a jevons package
//     (tests included: a test import would put jevons in go.mod too);
//   - the README's lead paragraph defines claudia without naming Jevons,
//     so the docs do not present it as "the Jevons harness".
//
// A local go.work that `use`s a jevons checkout is not a requirement and is
// not checked; it is untracked and is how the consumer is developed beside
// the library.
const jevonsModule = "github.com/marcelocantos/jevons"

// isJevonsImport is the check's whole decision for one import path: the
// jevons module itself or any package under it. A different module that
// merely shares the prefix (jevonsx) is not a match.
func isJevonsImport(path string) bool {
	return path == jevonsModule || strings.HasPrefix(path, jevonsModule+"/")
}

// jevonsImports lists every jevons import in the files given (file →
// imports), sorted, so a failure names each offending line.
func jevonsImports(files map[string][]string) []string {
	var out []string
	for file, imports := range files {
		for _, path := range imports {
			if isJevonsImport(path) {
				out = append(out, file+": "+path)
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestJevonsImportsDecision(t *testing.T) {
	got := jevonsImports(map[string][]string{
		"ok.go":   {"context", "github.com/marcelocantos/claudia/internal/broker", "github.com/google/uuid"},
		"bad.go":  {jevonsModule + "/internal/fleet", jevonsModule},
		"near.go": {jevonsModule + "x/other", "github.com/marcelocantos/jevonsmcp"}, // share the prefix, are not jevons
	})
	want := []string{"bad.go: " + jevonsModule, "bad.go: " + jevonsModule + "/internal/fleet"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("jevonsImports = %q, want %q", got, want)
	}
}

// TestNoClaudiaModuleRequiresJevons is the go.mod / go.sum half: the module
// graph names no jevons module. go.sum covers the transitive case — a
// dependency that itself required jevons would leave its hash there.
func TestNoClaudiaModuleRequiresJevons(t *testing.T) {
	root := moduleRoot(t)
	for _, name := range []string{"go.mod", "go.sum"} {
		f, err := os.Open(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		lineNo := 0
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			lineNo++
			if strings.Contains(sc.Text(), jevonsModule) {
				t.Errorf("%s:%d names %s; claudia is the library jevons consumes, never the other way round:\n  %s",
					name, lineNo, jevonsModule, sc.Text())
			}
		}
		_ = f.Close()
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
	}
}

// TestNoClaudiaPackageImportsJevons is the import-graph half: every .go
// file `./...` compiles, in every package of this module, tests included.
// gowalk.IgnoredDir keeps the walk to exactly those files (T99).
func TestNoClaudiaPackageImportsJevons(t *testing.T) {
	root := moduleRoot(t)
	files := map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		imports := []string{}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			imports = append(imports, p)
		}
		files[rel] = imports
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A walk that read nothing would pass silently.
	if len(files) == 0 {
		t.Fatal("walked the module and read no Go source files")
	}
	if bad := jevonsImports(files); len(bad) > 0 {
		t.Fatalf("claudia imports jevons; Jevons-specific wiring belongs in the jevons repo (🎯T13):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// readmeLead returns the first paragraph after the top-level heading:
// the sentence that says what claudia is.
func readmeLead(readme string) string {
	lines := strings.Split(readme, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "# ") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}
	var para []string
	for _, line := range lines[start:] {
		if strings.TrimSpace(line) == "" {
			if len(para) > 0 {
				break
			}
			continue
		}
		para = append(para, line)
	}
	return strings.Join(para, " ")
}

func TestReadmeLeadDecision(t *testing.T) {
	got := readmeLead("# claudia\n\nGo library for embedding\nagents.\n\nclaudia wraps Jevons.\n")
	if want := "Go library for embedding agents."; got != want {
		t.Fatalf("readmeLead = %q, want %q", got, want)
	}
	if got := readmeLead("no heading\n"); got != "" {
		t.Fatalf("readmeLead without a heading = %q, want empty", got)
	}
}

// TestReadmeDefinesClaudiaWithoutJevons is the docs half of 🎯T13: the
// README's lead paragraph is claudia's definition of itself, and it names
// no consumer. Jevons may appear further down as a labeled example.
func TestReadmeDefinesClaudiaWithoutJevons(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(moduleRoot(t), "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	lead := readmeLead(string(b))
	if lead == "" {
		t.Fatal("README.md has no lead paragraph under its top-level heading")
	}
	if strings.Contains(strings.ToLower(lead), "jevons") {
		t.Fatalf("README's lead paragraph defines claudia in terms of Jevons; it is a library for any program (🎯T13):\n  %s", lead)
	}
}
