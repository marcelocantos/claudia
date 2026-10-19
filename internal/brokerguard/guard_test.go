// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package brokerguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	modulePath   = "github.com/marcelocantos/claudia"
	brokerClient = modulePath + "/internal/broker"
	noBrokerEnv  = "CLAUDIA_NO_BROKER"
)

func TestEveryBrokerReachingTestPackageIsGuarded(t *testing.T) {
	reaching := brokerReachingTestPackages(t)
	if len(reaching) == 0 {
		t.Fatal("no test package links the broker client; the census is broken, not clean")
	}
	var unguarded []string
	for _, dir := range reaching {
		if !guardedTestMain(t, dir) {
			rel, _ := filepath.Rel(moduleRoot(t), dir)
			unguarded = append(unguarded, "./"+filepath.ToSlash(rel))
		}
	}
	sort.Strings(unguarded)
	if len(unguarded) > 0 {
		t.Fatalf("test packages that link the broker client without a TestMain setting %s=1 before m.Run "+
			"(a plain `go test` there can hand fixture seats to the machine's live broker):\n  %s",
			noBrokerEnv, strings.Join(unguarded, "\n  "))
	}
	t.Logf("%d broker-reaching test packages, all guarded", len(reaching))
}

// TestGuardDetection pins what counts as the guard, so the census above
// cannot pass on a TestMain that only mentions the variable.
func TestGuardDetection(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      bool
	}{
		{"selector", `package p
import ("os"; "testing"; "github.com/marcelocantos/claudia/internal/broker")
func TestMain(m *testing.M) {
	if os.Getenv(broker.NoBrokerEnv) == "" { _ = os.Setenv(broker.NoBrokerEnv, "1") }
	os.Exit(m.Run())
}`, true},
		{"literal", `package p
import ("os"; "testing")
func TestMain(m *testing.M) { os.Setenv("CLAUDIA_NO_BROKER", "1"); os.Exit(m.Run()) }`, true},
		{"no TestMain", `package p
import ("os"; "testing")
func TestX(t *testing.T) { os.Setenv("CLAUDIA_NO_BROKER", "1") }`, false},
		{"reads only", `package p
import ("os"; "testing")
func TestMain(m *testing.M) { _ = os.Getenv("CLAUDIA_NO_BROKER"); os.Exit(m.Run()) }`, false},
		{"switched on", `package p
import ("os"; "testing")
func TestMain(m *testing.M) { os.Setenv("CLAUDIA_NO_BROKER", ""); os.Exit(m.Run()) }`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := guardedTestMain(t, dir); got != tc.want {
				t.Fatalf("guarded = %v, want %v", got, tc.want)
			}
		})
	}
}

// brokerReachingTestPackages returns the directory of every package whose
// test binary depends on the broker client, as the go command resolves it.
func brokerReachingTestPackages(t *testing.T) []string {
	t.Helper()
	root := moduleRoot(t)
	// With -test, each package that has tests also lists its generated
	// "p.test" main package, whose Deps are the whole test binary's.
	cmd := exec.Command("go", "list", "-e", "-test", "-f",
		`{{if and (eq .Name "main") (not .ForTest)}}{{.ImportPath}}{{range .Deps}} {{.}}{{end}}{{end}}`, "./...")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	dirs := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasSuffix(fields[0], ".test") {
			continue
		}
		pkg := strings.TrimSuffix(fields[0], ".test")
		if pkg == brokerClient {
			continue // the client's own tests dial their own sockets
		}
		for _, dep := range fields[1:] {
			if dep == brokerClient {
				dirs[pkg] = packageDir(t, root, pkg)
				break
			}
		}
	}
	var list []string
	for _, d := range dirs {
		list = append(list, d)
	}
	sort.Strings(list)
	return list
}

func packageDir(t *testing.T, root, pkg string) string {
	t.Helper()
	if pkg == modulePath {
		return root
	}
	if !strings.HasPrefix(pkg, modulePath+"/") {
		t.Fatalf("test package %s is outside the module", pkg)
	}
	return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(pkg, modulePath+"/")))
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("go", "env", "GOMOD")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	mod := strings.TrimSpace(string(out))
	if mod == "" || mod == os.DevNull {
		t.Fatal("not inside the claudia module")
	}
	return filepath.Dir(mod)
}

// guardedTestMain reports whether a _test.go file in dir declares a TestMain
// that calls os.Setenv(CLAUDIA_NO_BROKER, "1"), by the broker constant or by
// the literal name.
func guardedTestMain(t *testing.T, dir string) bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "TestMain" || fn.Body == nil {
				continue
			}
			found := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 || !isSelector(call.Fun, "os", "Setenv") {
					return true
				}
				if namesNoBroker(call.Args[0]) && isOn(call.Args[1]) {
					found = true
				}
				return !found
			})
			if found {
				return true
			}
		}
	}
	return false
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

func namesNoBroker(e ast.Expr) bool {
	if sel, ok := e.(*ast.SelectorExpr); ok {
		return sel.Sel.Name == "NoBrokerEnv"
	}
	if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		v, err := strconv.Unquote(lit.Value)
		return err == nil && v == noBrokerEnv
	}
	return false
}

// isOn is a literal the broker reads as switched off (anything but the
// negative spellings internal/broker recognises).
func isOn(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}
