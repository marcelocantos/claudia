// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package omp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 🎯T143: a test binary never resolves the owner's real plan file.
func TestT143TestBinaryNeverGetsTheRealPlanFile(t *testing.T) {
	prev := testDataPath
	t.Cleanup(func() { testDataPath = prev })

	UseTestDataPath("")
	if p, err := DefaultDataPath(); err == nil || p != "" {
		t.Fatalf("with no test path: %q, %v; want an error and no path", p, err)
	}
	real := ""
	if dir, err := os.UserConfigDir(); err == nil {
		real = filepath.Join(dir, "claudia", dataFileName)
	}
	tmp := filepath.Join(t.TempDir(), dataFileName)
	UseTestDataPath(tmp)
	p, err := DefaultDataPath()
	if err != nil || p != tmp {
		t.Fatalf("with a test path: %q, %v", p, err)
	}
	if real != "" && strings.EqualFold(p, real) {
		t.Fatal("the test path is the owner's real plan file")
	}
}
