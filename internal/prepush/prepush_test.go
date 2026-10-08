// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package prepush

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const zero = "0000000000000000000000000000000000000000"

// hook is the repo's pre-push script, run against a throwaway repository.
func hook(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	p, err := filepath.Abs(filepath.Join(filepath.Dir(file), "..", "..", "scripts", "hooks", "pre-push"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type repo struct {
	t    *testing.T
	dir  string
	good string // a commit that builds and vets
	bad  string // a commit that does not build
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module prepushprobe\n\ngo 1.21\n")
	write("main.go", "package main\n\nfunc main() {}\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "good")
	good := git(t, dir, "rev-parse", "HEAD")
	write("broken.go", "package main\n\nfunc broken( {\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "bad")
	bad := git(t, dir, "rev-parse", "HEAD")
	return &repo{t: t, dir: dir, good: good, bad: bad}
}

// attest writes the attestation make gate would leave for sha's tree.
func (r *repo) attest(sha string) {
	tree := git(r.t, r.dir, "rev-parse", sha+"^{tree}")
	body := "tree=" + tree + "\ndirty=0\nat=2026-10-08T22:50:00+11:00\n"
	if err := os.WriteFile(filepath.Join(r.dir, ".git", "gate-attestation"), []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// push runs the hook with git's stdin lines and reports success and output.
func (r *repo) push(lines ...string) (bool, string) {
	r.t.Helper()
	cmd := exec.Command("sh", hook(r.t), "origin", "git@example.invalid:x.git")
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

func line(localRef, sha, remoteRef string) string {
	return localRef + " " + sha + " " + remoteRef + " " + zero
}

func TestBackupRefSkipsBuildAndAttestation(t *testing.T) {
	r := newRepo(t)
	// No attestation exists, and the tip does not even build.
	ok, out := r.push(line("refs/heads/wip", r.bad, "refs/backup/colossus/wip"))
	if !ok {
		t.Fatalf("backup push refused:\n%s", out)
	}
	if strings.Contains(out, "build + vet") && !strings.Contains(out, "skipped") {
		t.Fatalf("backup push ran build + vet:\n%s", out)
	}
	ok, out = r.push(
		line("refs/heads/a", r.bad, "refs/backup/colossus/a"),
		line("(delete)", zero, "refs/backup/colossus/gone"),
	)
	if !ok {
		t.Fatalf("backup push with a backup delete refused:\n%s", out)
	}
}

func TestNormalRefStillNeedsAttestation(t *testing.T) {
	r := newRepo(t)
	ok, out := r.push(line("refs/heads/master", r.good, "refs/heads/master"))
	if ok || !strings.Contains(out, "no gate attestation") {
		t.Fatalf("push to refs/heads/master without attestation: ok=%v\n%s", ok, out)
	}
	r.attest(r.good)
	if ok, out := r.push(line("refs/heads/master", r.good, "refs/heads/master")); !ok {
		t.Fatalf("attested push refused:\n%s", out)
	}
}

// A normal ref still runs build + vet. (They build the working tree, and
// as before a failure there does not stop the hook: under sh's set -e a
// failing command inside an && list is not fatal. The attestation is what
// refuses an unverified tree. That is unchanged here on purpose.)
func TestNormalRefStillRunsBuildAndVet(t *testing.T) {
	r := newRepo(t)
	git(t, r.dir, "checkout", "-q", r.good)
	r.attest(r.good)
	ok, out := r.push(line("refs/heads/master", r.good, "refs/heads/master"))
	if !ok || !strings.Contains(out, "✓ build + vet") {
		t.Fatalf("normal push: ok=%v, want build + vet to run:\n%s", ok, out)
	}
}

// A mixed push checks its non-backup refs as strictly as a push of them alone.
func TestMixedPushChecksNonBackupRefs(t *testing.T) {
	r := newRepo(t)
	backup := line("refs/heads/wip", r.good, "refs/backup/colossus/wip")
	normal := line("refs/heads/master", r.good, "refs/heads/master")
	if ok, out := r.push(backup, normal); ok || !strings.Contains(out, "no gate attestation") {
		t.Fatalf("mixed push without attestation: ok=%v\n%s", ok, out)
	}
	// Attest some other tree: the normal ref's tree must still match.
	r.attest(r.bad)
	if ok, out := r.push(normal, backup); ok || !strings.Contains(out, "build") && !strings.Contains(out, "attested tree") {
		t.Fatalf("mixed push with a stale attestation: ok=%v\n%s", ok, out)
	}
	r.attest(r.good)
	if ok, out := r.push(backup, normal); !ok {
		t.Fatalf("mixed push with a current attestation refused:\n%s", out)
	}
}

// A delete of a normal ref keeps the old rule: it is not exempt, though
// it pushes no tree to compare.
func TestNormalRefDeleteIsNotExempt(t *testing.T) {
	r := newRepo(t)
	ok, out := r.push(line("(delete)", zero, "refs/heads/old"))
	if ok || !strings.Contains(out, "no gate attestation") {
		t.Fatalf("normal-ref delete without attestation: ok=%v\n%s", ok, out)
	}
	r.attest(r.good)
	if ok, out := r.push(line("(delete)", zero, "refs/heads/old")); !ok {
		t.Fatalf("normal-ref delete with attestation refused:\n%s", out)
	}
}

// A ref merely containing "backup" is not under refs/backup/.
func TestLookalikeRefIsNotExempt(t *testing.T) {
	r := newRepo(t)
	for _, remote := range []string{"refs/heads/backup/x", "refs/backups/x", "refs/heads/refs/backup/x"} {
		if ok, out := r.push(line("refs/heads/x", r.good, remote)); ok {
			t.Fatalf("push to %s skipped the checks:\n%s", remote, out)
		}
	}
}
