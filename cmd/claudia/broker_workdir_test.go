package main

import (
	"path/filepath"
	"testing"
)

func TestSeatDefinitionResolvesWorkDir(t *testing.T) {
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(real)
	for _, in := range []string{"", "."} {
		def, err := seatDefinition("s", "grok", in, "work", "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if def.WorkDir != real {
			t.Fatalf("seatDefinition workdir %q = %q, want %q", in, def.WorkDir, real)
		}
	}
}
