// 🎯T164: a sidecar seat (provider=anthropic) is refused a branch checkout
// of the shared clone, as jevons 🎯T955's Claude Code hook refuses it for a
// provider=claude seat. These tests drive the sidecar's own pre-exec path —
// the Bash tool's execute — not a hook file this process never reads.

import { describe, expect, test } from "bun:test";
import { execFileSync } from "node:child_process";
import { mkdtempSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { decideBranchCheckout, detectBranchCheckout, guardBranchCheckout } from "./branchguard.ts";
import { codingTools } from "./coding.ts";

function git(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
}

// sharedClone is a throwaway repo on branch `steer` with a `master` branch
// behind it — the 2026-09-30 incident's shape (jevons 🎯T955 fixture).
function sharedClone(): string {
  const dir = realpathSync(mkdtempSync(join(tmpdir(), "branchguard-")));
  git(dir, "init", "-q", "-b", "steer");
  git(dir, "config", "user.email", "test@example.com");
  git(dir, "config", "user.name", "test");
  git(dir, "config", "commit.gpgsign", "false");
  writeFileSync(join(dir, "README.md"), "steer\n");
  git(dir, "add", ".");
  git(dir, "commit", "-q", "-m", "steer base");
  git(dir, "branch", "master");
  return dir;
}

function bash(dir: string) {
  const t = codingTools(() => dir, () => 100_000).find((x) => x.name === "Bash");
  if (!t) throw new Error("no Bash tool");
  return (command: string) => t.execute("c", { command }) as Promise<{ content: { text: string }[]; isError?: boolean }>;
}

describe("detectBranchCheckout", () => {
  const cases: [string, string, ReturnType<typeof detectBranchCheckout>][] = [
    ["bare checkout of a branch", "git checkout master", { ref: "master", dir: "", definite: false }],
    ["switch is always definite", "git switch master", { ref: "master", dir: "", definite: true }],
    ["checkout -b creates and switches", "git checkout -b feature/x", { ref: "feature/x", dir: "", definite: true }],
    ["switch -c creates and switches", "git switch -c feature/x", { ref: "feature/x", dir: "", definite: true }],
    ["path restore with --", "git checkout -- some/file.go", null],
    ["ref-and-path restore with --", "git checkout HEAD -- some/file.go", null],
    ["bare checkout with no args", "git checkout", null],
    ["restore is not a switch", "git restore some/file.go", null],
    ["unrelated command", "go test ./...", null],
    ["git status is read-only", "git status", null],
    ["second command in a chain", "make test && git checkout master", { ref: "master", dir: "", definite: false }],
    ["quoted separator does not split", "echo 'a; git checkout master'", null],
    ["heredoc body is data", "cat <<EOF\ngit checkout master\nEOF", null],
    ["-C names the repository", "git -C ../shared checkout master", { ref: "master", dir: "../shared", definite: false }],
    ["cd moves later segments", "cd ../shared && git switch master", { ref: "master", dir: "../shared", definite: true }],
    ["env prefix is stripped", "GIT_DIR=.git env git switch master", { ref: "master", dir: "", definite: true }],
  ];
  for (const [name, command, want] of cases) {
    test(name, () => {
      expect(detectBranchCheckout(command)).toEqual(want);
    });
  }
});

describe("decideBranchCheckout", () => {
  test("the same branch is allowed", () => {
    expect(decideBranchCheckout({ currentBranch: "steer", targetRef: "steer", repo: "/r" }).verdict).toBe("allow");
  });
  test("a different branch is refused, naming both and the clone", () => {
    const d = decideBranchCheckout({ currentBranch: "steer", targetRef: "master", repo: "/shared/clone" });
    expect(d.verdict).toBe("deny");
    for (const want of ["steer", "master", "/shared/clone", "shared clone", "T164"]) {
      expect(d.message).toContain(want);
    }
  });
});

describe("the Bash tool's pre-exec guard (🎯T164)", () => {
  test("a fixture seat cannot switch the shared clone's branch, and recovers", async () => {
    const dir = sharedClone();
    try {
      const run = bash(dir);

      // The incident's command. Refused before the shell starts: HEAD still
      // reads steer, and the refusal names the clone and both branches.
      const refused = await run("git checkout master && git merge --ff-only jv-t946-resume2");
      expect(refused.isError).toBe(true);
      for (const want of ["steer", "master", dir, "T164"]) {
        expect(refused.content[0].text).toContain(want);
      }
      expect(git(dir, "symbolic-ref", "--quiet", "--short", "HEAD")).toBe("steer");

      // `git switch` and a create-and-switch are refused the same way.
      expect((await run("git switch master")).isError).toBe(true);
      expect((await run("git checkout -b scratch")).isError).toBe(true);
      expect(git(dir, "symbolic-ref", "--quiet", "--short", "HEAD")).toBe("steer");

      // Recoverable: ordinary git on the current branch still runs.
      const status = await run("git status --short");
      expect(status.isError).toBeUndefined();
      expect(status.content[0].text).toBe("(no output)");

      writeFileSync(join(dir, "README.md"), "steer again\n");
      const diff = await run("git diff --stat");
      expect(diff.isError).toBeUndefined();
      expect(diff.content[0].text).toContain("README.md");

      const commit = await run("git commit -q -am 'on steer' && git log --oneline -1");
      expect(commit.isError).toBeUndefined();
      expect(commit.content[0].text).toContain("on steer");
      expect(git(dir, "symbolic-ref", "--quiet", "--short", "HEAD")).toBe("steer");

      // Checking out the branch already checked out is not a switch.
      expect((await run("git checkout steer")).isError).toBeUndefined();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  test("a path restore in the shared clone is not a branch switch", async () => {
    const dir = sharedClone();
    try {
      const run = bash(dir);
      for (const command of ["git checkout .", "git checkout -- README.md", "git checkout HEAD -- README.md", "git checkout README.md"]) {
        const r = await run(command);
        expect(r.isError).toBeUndefined();
      }
      expect(git(dir, "symbolic-ref", "--quiet", "--short", "HEAD")).toBe("steer");
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  test("a seat may switch branches inside its own linked worktree", async () => {
    const base = sharedClone();
    const wtParent = realpathSync(mkdtempSync(join(tmpdir(), "branchguard-wt-")));
    const wt = join(wtParent, "wt");
    try {
      git(base, "branch", "jevons-worktree/seat-x");
      git(base, "worktree", "add", "-q", wt, "jevons-worktree/seat-x");
      const r = await bash(wt)("git checkout master");
      expect(r.isError).toBeUndefined();
      expect(git(wt, "symbolic-ref", "--quiet", "--short", "HEAD")).toBe("master");
      // The shared clone itself was untouched.
      expect(git(base, "symbolic-ref", "--quiet", "--short", "HEAD")).toBe("steer");
    } finally {
      rmSync(wtParent, { recursive: true, force: true });
      rmSync(base, { recursive: true, force: true });
    }
  });

  test("a switch addressed at the shared clone from elsewhere is still refused", async () => {
    const shared = sharedClone();
    const elsewhere = realpathSync(mkdtempSync(join(tmpdir(), "branchguard-else-")));
    try {
      const run = bash(elsewhere);
      const viaC = await run(`git -C ${shared} checkout master`);
      expect(viaC.isError).toBe(true);
      expect(viaC.content[0].text).toContain(shared);
      const viaCd = await run(`cd ${shared} && git switch master`);
      expect(viaCd.isError).toBe(true);
      expect(git(shared, "symbolic-ref", "--quiet", "--short", "HEAD")).toBe("steer");
    } finally {
      rmSync(elsewhere, { recursive: true, force: true });
      rmSync(shared, { recursive: true, force: true });
    }
  });

  test("outside a repository the guard has nothing to say", async () => {
    const dir = realpathSync(mkdtempSync(join(tmpdir(), "branchguard-norepo-")));
    try {
      expect(await guardBranchCheckout(dir, "git checkout master")).toBeNull();
      expect(await guardBranchCheckout(dir, "echo hello")).toBeNull();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
