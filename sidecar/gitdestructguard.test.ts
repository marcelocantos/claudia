import { describe, expect, test } from "bun:test";
import { execFileSync } from "node:child_process";
import { mkdtempSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { guardDestructiveGit } from "./gitdestructguard.ts";
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
      for (const command of ["git status", "git stash list", "git stash show", "git restore --staged owner.txt", "echo 'git clean -f'"]) {
        await expect(guardDestructiveGit(dir, command)).resolves.toBeUndefined();
      }
    } finally { rmSync(parent, { recursive: true, force: true }); rmSync(dir, { recursive: true, force: true }); }
  });
});
