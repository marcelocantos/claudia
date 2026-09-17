// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
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
)

// The library-first boundary (🎯T75.1). The daemon is a separate package, so
// it cannot reach claudia's unexported identifiers at all; what remains is
// importing claudia's other internal packages and rebuilding capability
// here instead of in the library. The daemon may import the library and the
// wire it serves, nothing else from this module.

const module = "github.com/marcelocantos/claudia"

var allowedModuleImports = map[string]bool{
	module:                      true, // the library, by its exported API
	module + "/internal/broker": true, // the wire the daemon serves
}

// disallowedImports is the check's whole decision: every import from this
// module, other than the allowed ones, in the files given (file → imports).
func disallowedImports(files map[string][]string) []string {
	var out []string
	for file, imports := range files {
		for _, path := range imports {
			if (path == module || strings.HasPrefix(path, module+"/")) && !allowedModuleImports[path] {
				out = append(out, file+": "+path)
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestDisallowedImportsDecision(t *testing.T) {
	got := disallowedImports(map[string][]string{
		"ok.go":   {"context", module, module + "/internal/broker", "github.com/google/uuid"},
		"bad.go":  {module + "/internal/tmuxagent", module + "/grok"},
		"near.go": {module + "x/other"}, // a different module that shares the prefix
	})
	want := []string{"bad.go: " + module + "/grok", "bad.go: " + module + "/internal/tmuxagent"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("disallowedImports = %q, want %q", got, want)
	}
}

// TestDaemonImportsOnlyTheLibraryAndTheWire applies the decision to this
// package's non-test files, and checks the library does not import the
// daemon back (Go would refuse the cycle, but a root file importing it
// through a helper package would not be a cycle).
func TestDaemonImportsOnlyTheLibraryAndTheWire(t *testing.T) {
	files := map[string][]string{}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			files[name] = append(files[name], path)
		}
	}
	if len(files) == 0 {
		t.Fatal("found no daemon source files to check")
	}
	if bad := disallowedImports(files); len(bad) > 0 {
		t.Fatalf("the daemon imports module packages other than the library and the wire:\n  %s", strings.Join(bad, "\n  "))
	}

	daemonPath := module + "/daemon"
	err = filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "daemon", "cmd", "scratchpad", "testdata", ".git", "bin":
				if path != ".." {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == daemonPath {
				t.Errorf("%s imports the daemon; the library must not depend on its server", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
