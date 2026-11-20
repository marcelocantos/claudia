import { describe, expect, test } from "bun:test";
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, realpathSync, rmSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { detectBranchCheckout, guardBranchCheckout } from "./branchguard.ts";
import { destructiveGitCommands, guardDestructiveGit } from "./gitdestructguard.ts";
import { runShell } from "./execution.ts";
import { SeatProcessOwnership } from "./killguard.ts";

function git(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8" }).trim();
}
function fixture() {
  const dir = realpathSync(mkdtempSync(join(tmpdir(), "gitdestructguard-")));
  git(dir, "init", "-q", "-b", "main");
  git(dir, "config", "user.email", "test@example.com");
  git(dir, "config", "user.name", "test");
  git(dir, "config", "commit.gpgsign", "false");
  writeFileSync(join(dir, "owner.txt"), "original\n");
  git(dir, "add", "."); git(dir, "commit", "-qm", "init");
  writeFileSync(join(dir, "owner.txt"), "owner change\n");
  writeFileSync(join(dir, "untracked.txt"), "owner new file\n");
  return dir;
}

describe("Git global options in both shared-clone guards (H8)", () => {
  const prefixes = [
    "git -c color.ui=never",
    "git -c 'core.pager=git stash' -c advice.detachedHead=false",
    "git -C '' -C sub -C .. -c color.ui=never",
    "git --git-dir .git --work-tree .",
    "git --git-dir=.git --work-tree=.",
    "git --git-dir .git -C repo --work-tree=.",
    "git --namespace seat",
    "git --namespace=seat",
    "git --config-env color.ui=SEAT_COLOR",
    "git --config-env=color.ui=SEAT_COLOR",
    "git --attr-source HEAD",
    "git --attr-source=HEAD",
    "git --shallow-file .git/shallow",
    "git --super-prefix dir/",
    "git --super-prefix=dir/",
    "git --exec-path=/usr/bin -p -P --literal-pathspecs",
  ];
  for (const prefix of prefixes) {
    test(prefix, () => {
      expect(detectBranchCheckout(`${prefix} checkout other`)?.ref).toBe("other");
      expect(destructiveGitCommands(`${prefix} stash`).map((h) => h.operation)).toEqual(["stash"]);
      expect(destructiveGitCommands(`${prefix} checkout -- owner.txt`)[0]?.paths).toEqual(["owner.txt"]);
    });
  }

  for (const prefix of ["git --", "git --exec-path", "git --version", "git -pP", "git -Csub", "git -ccolor.ui=never"]) {
    test(`${prefix} does not select a subcommand`, () => {
      expect(detectBranchCheckout(`${prefix} checkout other`)).toBeNull();
      expect(destructiveGitCommands(`${prefix} stash`)).toEqual([]);
    });
  }

  test("denies both operations before launch, including explicit repository paths", async () => {
    const dir = fixture();
    const elsewhere = realpathSync(mkdtempSync(join(tmpdir(), "git-options-")));
    try {
      git(dir, "branch", "other");
      mkdirSync(join(dir, "sub"));
      symlinkSync(join(dir, "sub"), join(elsewhere, "link"));
      const invocations = [
        { cwd: dir, prefix: "git -c color.ui=never" },
        { cwd: elsewhere, prefix: `git --git-dir .git -C "${dir}" -C sub -C .. --work-tree=.` },
        { cwd: elsewhere, prefix: `git --git-dir="${dir}/.git" --work-tree "${dir}"` },
        { cwd: elsewhere, prefix: "git -C link -C .. -c color.ui=never" },
      ];
      let launched = false;
      for (const { cwd, prefix } of invocations) {
        for (const [command, guard] of [["stash", "gitdestructguard:"], ["checkout other", "branchguard:"]]) {
          await expect(runShell(() => cwd, () => undefined,
            { command: `${prefix} ${command}`, timeoutMs: 1000, maxBuffer: 1000 },
            new SeatProcessOwnership(), async () => { launched = true; throw new Error("spawned"); }))
            .rejects.toThrow(guard);
        }
      }
      expect(launched).toBe(false);
    } finally {
      rmSync(elsewhere, { recursive: true, force: true });
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe("T240 shared clone destructive git pre-exec", () => {
  const commands = [
    "git stash", "git stash push", "git stash save", "git stash drop", "git stash clear", "git stash pop", "git stash apply",
    "git reset --hard", "git reset --merge", "git reset --keep",
    "git checkout HEAD -- owner.txt", "git checkout -- owner.txt", "git checkout owner.txt",
    "git restore owner.txt", "git restore --worktree owner.txt", "git restore --source HEAD owner.txt", "git checkout-index -f -- owner.txt", "git clean -fd", "git clean -fx",
  ];
  for (const command of commands) test(`refuses ${command} without launching shell`, async () => {
    const dir = fixture();
    let launched = false;
    try {
      await expect(runShell(() => dir, () => undefined, { command, timeoutMs: 1000, maxBuffer: 1000 },
        new SeatProcessOwnership(), async () => { launched = true; throw new Error("spawned"); })).rejects.toThrow("gitdestructguard:");
      expect(launched).toBe(false);
      expect(git(dir, "status", "--short")).toContain("owner.txt");
      expect(git(dir, "status", "--short")).toContain("untracked.txt");
      const message = await guardDestructiveGit(dir, command).catch((e: Error) => e.message);
      expect(message).toContain(command.includes("clean") ? "untracked.txt" : "owner.txt");
    } finally { rmSync(dir, { recursive: true, force: true }); }
  });
  test("clean -fx names ignored paths too", async () => {
    const dir = fixture();
    try {
      writeFileSync(join(dir, ".gitignore"), "ignored.txt\n");
      writeFileSync(join(dir, "ignored.txt"), "owner ignored\n");
      const message = await guardDestructiveGit(dir, "git clean -fx").catch((e: Error) => e.message);
      expect(message).toContain("ignored.txt");
      expect(message).toContain("untracked.txt");
    } finally { rmSync(dir, { recursive: true, force: true }); }
  });
  test("cd sub && cd .. evolves cwd and denies stash before shell launch", async () => {
    const dir = fixture(); mkdirSync(join(dir, "sub"));
    let launched = false;
    try {
      const command = "cd sub && cd .. && git stash";
      const message = await runShell(() => dir, () => undefined, { command, timeoutMs: 1000, maxBuffer: 1000 },
        new SeatProcessOwnership(), async () => { launched = true; throw new Error("spawned"); }).catch((e: Error) => e.message);
      expect(message).toContain("gitdestructguard:");
      expect(message).toContain("owner.txt");
      expect(launched).toBe(false);
      await expect(guardDestructiveGit(dir, "cd sub && cd .. && git -C sub/.. stash push")).rejects.toThrow("owner.txt");
    } finally { rmSync(dir, { recursive: true, force: true }); }
  });
  test("absolute cd and -C reset prior relative cwd before pre-exec admission", async () => {
    const dir = fixture();
    mkdirSync(join(dir, "sub"));
    let launched = false;
    try {
      for (const command of [`cd sub && cd ${dir} && git stash`,
        `cd sub && cd ${join(dir, "sub")} && cd .. && git stash`,
        `cd sub && git -C ${dir} stash`]) {
        expect(destructiveGitCommands(command)[0].dir).toBe(dir);
        const message = await runShell(() => dir, () => undefined, { command, timeoutMs: 1000, maxBuffer: 1000 },
          new SeatProcessOwnership(), async () => { launched = true; throw new Error("spawned"); }).catch((e: Error) => e.message);
        expect(message).toContain("gitdestructguard:");
        expect(message).toContain("owner.txt");
        expect(launched).toBe(false);
      }
    } finally { rmSync(dir, { recursive: true, force: true }); }
  });
  test("clean excludes are operands, not pathspecs; names only removed paths", async () => {
    const dir = fixture();
    let launched = false;
    try {
      writeFileSync(join(dir, "keep.txt"), "owner keep\n");
      writeFileSync(join(dir, ".gitignore"), "ignored.txt\n");
      writeFileSync(join(dir, "ignored.txt"), "owner ignored\n");
      for (const option of ["-e keep.txt", "--exclude=keep.txt", "--exclude keep.txt"]) {
        const command = `git clean -fx ${option}`;
        const message = await runShell(() => dir, () => undefined, { command, timeoutMs: 1000, maxBuffer: 1000 },
          new SeatProcessOwnership(), async () => { launched = true; throw new Error("spawned"); }).catch((e: Error) => e.message);
        expect(message).toContain("gitdestructguard:");
        expect(message).toContain("untracked.txt");
        expect(message).toContain("ignored.txt");
        expect(message).not.toContain('"keep.txt"');
        expect(launched).toBe(false);
      }
    } finally { rmSync(dir, { recursive: true, force: true }); }
  });
  test("-C and cd segments resolve shared clone from another cwd", async () => {
    const dir = fixture(), elsewhere = realpathSync(mkdtempSync(join(tmpdir(), "gitdestruct-else-")));
    try {
      for (const command of [`git -C ${dir} reset --hard`, `cd ${dir} && git stash push`, `echo safe; git -C ${dir} clean -f`]) {
        await expect(guardDestructiveGit(elsewhere, command)).rejects.toThrow(dir);
      }
    } finally { rmSync(dir, { recursive: true, force: true }); rmSync(elsewhere, { recursive: true, force: true }); }
  });
  test("linked worktree is allowed; non-destructive shared operations remain allowed", async () => {
    const dir = fixture(), parent = realpathSync(mkdtempSync(join(tmpdir(), "gitdestruct-wt-")));
    try {
      const wt = join(parent, "wt"); git(dir, "worktree", "add", "-qb", "seat", wt);
      writeFileSync(join(wt, "owner.txt"), "seat edit\n");
      await expect(guardDestructiveGit(wt, "git reset --hard")).resolves.toBeUndefined();
      const explicit = `git --git-dir="${git(wt, "rev-parse", "--absolute-git-dir")}" --work-tree="${wt}"`;
      await expect(guardDestructiveGit(dir, `${explicit} stash`)).resolves.toBeUndefined();
      await expect(guardBranchCheckout(dir, `${explicit} checkout -b elsewhere`)).resolves.toBeNull();
      for (const command of ["git status", "git stash list", "git -c color.ui=never stash show", "git restore --staged owner.txt", "echo 'git clean -f'"]) {
        await expect(guardDestructiveGit(dir, command)).resolves.toBeUndefined();
      }
    } finally { rmSync(parent, { recursive: true, force: true }); rmSync(dir, { recursive: true, force: true }); }
  });
});
