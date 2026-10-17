// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package claudia

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/internal/gowalk"
)

// consumerNamedLiteral is one Go string constant that names a Jevons
// default: the state directory, an environment variable, or the consumer
// MCP prefix. Comments are not literals. A test file is not this walk.
func consumerNamedLiteral(s string) bool {
	return strings.Contains(s, ".jevons") ||
		strings.Contains(s, "JEVONS_") ||
		strings.Contains(s, "jevonsmcp")
}

// consumerNamedLiterals lists string literals in one Go file that name a
// Jevons default. Comments are invisible to the parse, so a mention in a
// comment is not a hit.
func consumerNamedLiterals(filename string, src []byte) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			s = lit.Value
		}
		if consumerNamedLiteral(s) {
			pos := fset.Position(lit.Pos())
			out = append(out, fmt.Sprintf("%s:%d: %s", pos.Filename, pos.Line, s))
		}
		return true
	})
	sort.Strings(out)
	return out, nil
}

func TestConsumerNamedLiteralDecision(t *testing.T) {
	src := []byte(`package p

// comment keeps JEVONS_SPOOL_DIR, ~/.jevons, and "jevonsmcp"
const plain = "neutral"

const prefix = "jevonsmcp"
const env = "JEVONS_SPOOL_DIR"
const home = "~/.jevons/spool"
`)
	got, err := consumerNamedLiterals("src.go", src)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"src.go:6: jevonsmcp",
		"src.go:7: JEVONS_SPOOL_DIR",
		"src.go:8: ~/.jevons/spool",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("literals =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestNoConsumerNamedLiteralInNonTestSource is the 🎯T160 tree walk:
// non-test Go under ./... (gowalk.IgnoredDir) has no .jevons, JEVONS_, or
// jevonsmcp string literal. _test.go files and comments may still name the
// consumer, because those are fixtures and explanations, not defaults.
func TestNoConsumerNamedLiteralInNonTestSource(t *testing.T) {
	root := moduleRoot(t)
	var hits []string
	files := 0
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
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		rel, _ := filepath.Rel(root, path)
		found, err := consumerNamedLiterals(rel, src)
		if err != nil {
			return err
		}
		hits = append(hits, found...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatal("walked the module and read no non-test Go source")
	}
	if len(hits) > 0 {
		t.Fatalf("non-test Go still names a Jevons default (🎯T160):\n  %s", strings.Join(hits, "\n  "))
	}
}
