// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package codex

import (
	goast "go/ast"
	goparser "go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHermeticTestsHaveNoWallClockDeadline keeps 🎯T33 fixed.
//
// TestHermeticTaskRunSuccess used to bound its run with
// context.WithTimeout(..., 10*time.Second). That context reaches the child
// through Run's runCtx (task.go:149) and exec.CommandContext (task.go:175),
// so when the deadline expired os/exec killed the fake CLI. The stdout
// scanner (task.go:221) then saw EOF having read nothing, and the ExitError
// that Wait produced raced runCtx.Done() in the send at task.go:273 — so the
// channel could close with no events at all. The failure was
// `incomplete events init=false result=false got=[]codex.Event(nil)`: not a
// truncated stream, an empty one, and the constant rather than the product
// had decided the verdict. On a loaded machine 10s is not enough to fork
// /bin/sh and read a fixture, so a green suite was not citable evidence.
//
// The fix is to wait on the event the test needs — drain the channel until
// the process closes it — and let `go test -timeout` be the only clock. This
// guard stops a deadline being reintroduced into a hermetic test, where a
// slow machine could again outvote the assertion.
//
// Live tests (*_live_test.go) are exempt: they drive a real backend over the
// network, where a deadline bounds a genuinely unbounded wait rather than a
// local fixture read.
//
// time.Sleep is not flagged — it delays a test but cannot by itself fail one.
func TestHermeticTestsHaveNoWallClockDeadline(t *testing.T) {
	// Constructs whose expiry can turn a slow machine into a failed assertion.
	banned := map[string]string{
		"context.WithTimeout":  "deadline can expire before the fixture is read",
		"context.WithDeadline": "deadline can expire before the fixture is read",
		"time.After":           "fires against whatever the select is racing",
		"time.Tick":            "fires against whatever the select is racing",
		"time.NewTimer":        "fires against whatever the select is racing",
		"time.NewTicker":       "fires against whatever the select is racing",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := gotoken.NewFileSet()
	var scanned int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, "_live_test.go") {
			continue
		}
		scanned++
		file, err := goparser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		goast.Inspect(file, func(n goast.Node) bool {
			call, ok := n.(*goast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*goast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*goast.Ident)
			if !ok {
				return true
			}
			qualified := pkg.Name + "." + sel.Sel.Name
			if why, bad := banned[qualified]; bad {
				pos := fset.Position(call.Pos())
				t.Errorf("%s:%d: hermetic test uses %s — %s (🎯T33). "+
					"Wait on the event the test needs; let `go test -timeout` be the clock.",
					name, pos.Line, qualified, why)
			}
			return true
		})
	}

	// A guard that scanned nothing would pass forever.
	if scanned == 0 {
		t.Fatal("no hermetic test files scanned — guard is not looking at anything")
	}
}
