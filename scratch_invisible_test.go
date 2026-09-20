package claudia

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/marcelocantos/claudia/internal/gowalk"
)

// T99. Every seat in this repo takes its evidence from `make gate`, so one
// agent's private scratch file must not be able to turn that gate RED for
// everybody else. On 2026-09-20 it did: a scratch copy of agent.go written to
// the scratch root declared `package claudia` a second time, `./...` compiled
// it, and a clean full-suite run died on
//
//	vet: scratchpad/agent-head.go:62:11: undefined: Provider
//
// The mechanism that prevents it is the leading underscore on _scratchpad/,
// which is a rule of the go command itself. .gitignore is not a mechanism here
// and must never be cited as one — the toolchain does not read it.
const scratchDir = "_scratchpad"

// conflictingScratch is that incident's shape, reproduced: a truncated copy of
// a library file, so it declares the library's package a second time and calls
// a symbol it did not copy. The trailing comment carries the storage tokens
// that provider_test.go's hygiene walks forbid in production source, so one
// file exercises the compile hazard and the walk hazard together.
const conflictingScratch = `package claudia

func scratchHead(p Provider) string { return scratchUndefinedSymbol(p) }

// scratch notes: rollout_path, .grok/sessions
`

// TestScratchFileCannotBreakTheGate drops that file at the scratch root of this
// very module and asserts the gate is blind to it — both halves of blind: the
// toolchain does not compile it, and the suite's own tree walks do not read it.
func TestScratchFileCannotBreakTheGate(t *testing.T) {
	root := moduleRoot(t)
	dropped := dropConflictingScratch(t, filepath.Join(root, scratchDir))

	out, err := runGoVet(t, root)
	if err != nil {
		t.Errorf("a scratch file under %s/ broke `go vet ./...` (%v):\n%s\n"+
			"the scratch directory is no longer invisible to the toolchain",
			scratchDir, err, out)
	}

	if seen := goFilesVisibleToWalks(t, root); seen[dropped] {
		t.Errorf("%s is read by a gowalk.IgnoredDir walk; a file the toolchain "+
			"never compiles must be a file the suite never reads", dropped)
	}
}

// TestVisibleScratchDirectoryStillBreaksTheGate is the teeth of the test above,
// which would otherwise pass just as well if the dropped file were harmless. It
// runs in a throwaway module rather than this one, because proving the hazard
// is real means creating it, and four other seats are running the gate here.
func TestVisibleScratchDirectoryStillBreaksTheGate(t *testing.T) {
	if gowalk.IgnoredDir("scratchpad") {
		t.Fatal(`gowalk.IgnoredDir("scratchpad") is true; the rule under test is not the "_" prefix`)
	}
	if !gowalk.IgnoredDir(scratchDir) {
		t.Fatalf("gowalk.IgnoredDir(%q) is false; the repo's scratch directory is not an ignored one", scratchDir)
	}

	probe := probeModule(t, goDirective(t, moduleRoot(t)))

	visible := dropConflictingScratchAs(t, filepath.Join(probe, "scratchpad"), "probe")
	out, err := runGoVet(t, probe)
	if err == nil {
		t.Fatalf("`go vet ./...` passed with a conflicting file at %s; "+
			"this test can no longer tell an invisible directory from a visible one:\n%s",
			visible, out)
	}
	if !strings.Contains(out, "scratchpad") {
		t.Errorf("vet failed for some other reason than the dropped file:\n%s", out)
	}

	// Same file, same content, underscore in front: green.
	if err := os.Remove(visible); err != nil {
		t.Fatal(err)
	}
	hidden := dropConflictingScratchAs(t, filepath.Join(probe, scratchDir), "probe")
	if out, err := runGoVet(t, probe); err != nil {
		t.Errorf("`go vet ./...` failed with the same file at %s (%v):\n%s", hidden, err, out)
	}
}

func runGoVet(t *testing.T, dir string) (string, error) {
	t.Helper()
	cmd := exec.Command("go", "vet", "./...")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// goFilesVisibleToWalks is what every hand-rolled walk in this suite does:
// every .go file outside a directory the go command ignores.
func goFilesVisibleToWalks(t *testing.T, root string) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
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
		if strings.HasSuffix(path, ".go") {
			seen[path] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 {
		t.Fatal("walked the module and found no .go files at all")
	}
	return seen
}

// dropConflictingScratch writes the incident's file into dir and removes it
// when the test ends. The name is unique because sibling seats run this suite
// against the same checkout at the same time.
func dropConflictingScratch(t *testing.T, dir string) string {
	t.Helper()
	return dropConflictingScratchAs(t, dir, "claudia")
}

func dropConflictingScratchAs(t *testing.T, dir, pkg string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(dir, "zz-t99-conflict-*.go")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(conflictingScratch, "package claudia", "package "+pkg, 1)
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

// probeModule builds a minimal module in a temp directory, outside this repo
// and outside any go.work above it.
func probeModule(t *testing.T, goVersion string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/probe\n\ngo "+goVersion+"\n")
	write("probe.go", "package probe\n\ntype Provider int\n\nfunc scratchUndefinedSymbol(Provider) string { return \"\" }\n")
	return dir
}

var goDirectiveRE = regexp.MustCompile(`(?m)^go (\d+\.\d+(\.\d+)?)$`)

// goDirective reuses this module's own go directive for the probe module, so
// the probe never asks for a toolchain this machine would have to download.
func goDirective(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	m := goDirectiveRE.FindSubmatch(data)
	if m == nil {
		t.Fatalf("no `go` directive in %s/go.mod", root)
	}
	return string(m[1])
}
