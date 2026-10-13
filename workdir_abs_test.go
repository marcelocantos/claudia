package claudia

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveClientWorkDir(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(real, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(real, "link")
	if err := os.Symlink(filepath.Join(real, "sub"), link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(real)
	for in, want := range map[string]string{
		"":     real,
		".":    real,
		"sub":  filepath.Join(real, "sub"),
		"link": filepath.Join(real, "sub"),
		"nope": filepath.Join(real, "nope"), // missing: Abs, unresolved
	} {
		got, err := ResolveClientWorkDir(in)
		if err != nil || got != want {
			t.Errorf("ResolveClientWorkDir(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
