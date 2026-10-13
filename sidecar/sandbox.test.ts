// 🎯T178: Codex sandbox settings mapped onto the sidecar's own tools.
import { describe, expect, test } from "bun:test";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { codingTools, type SandboxPolicy } from "./coding.ts";

function tmp(): string {
  return fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "sbx-")));
}

function tool(policy: SandboxPolicy | undefined, cwd: string, name: string) {
  const t = codingTools(() => cwd, () => 100_000, () => policy).find((x) => x.name === name);
  return t as unknown as { execute: (id: string, p: unknown) => Promise<{ content: { text: string }[]; isError?: boolean }> };
}

const text = (r: { content: { text: string }[] }) => r.content[0].text;

describe("sandbox policy", () => {
  test("read-only holds back Write and Bash", () => {
    const names = codingTools(() => "/tmp", () => 1000, () => ({ mode: "read-only" })).map((t) => t.name);
    expect(names.sort()).toEqual(["Glob", "Grep", "Read"]);
  });

  test("no policy keeps every tool", () => {
    const names = codingTools(() => "/tmp", () => 1000).map((t) => t.name);
    expect(names.sort()).toEqual(["Bash", "Glob", "Grep", "Read", "Write"]);
  });

  test("Write is confined to the workdir and refuses a symlink escape", async () => {
    const work = tmp();
    const outside = tmp();
    fs.symlinkSync(outside, path.join(work, "esc"));
    const w = tool({ mode: "workspace-write" }, work, "Write");
    expect(text(await w.execute("1", { path: "ok.txt", content: "x" }))).toContain("wrote");
    expect(fs.readFileSync(path.join(work, "ok.txt"), "utf8")).toBe("x");
    expect(text(await w.execute("2", { path: "esc/bad.txt", content: "x" }))).toContain("outside");
    expect(fs.existsSync(path.join(outside, "bad.txt"))).toBe(false);
  });

  test("Write keeps protected git dirs read-only unless granted", async () => {
    const work = tmp();
    fs.mkdirSync(path.join(work, ".git"));
    const git = path.join(work, ".git");
    const denied = tool({ mode: "workspace-write", protected: [git] }, work, "Write");
    expect(text(await denied.execute("1", { path: ".git/HEAD", content: "x" }))).toContain("protected");
    const granted = tool({ mode: "workspace-write", writable_roots: [git] }, work, "Write");
    expect(text(await granted.execute("2", { path: ".git/HEAD", content: "x" }))).toContain("wrote");
  });

  const darwin = process.platform === "darwin" && fs.existsSync("/usr/bin/sandbox-exec");
  test.if(darwin)("Bash writes are confined by seatbelt", async () => {
    const work = tmp();
    const outside = path.join(os.homedir(), `.sbx-outside-${process.pid}`);
    fs.mkdirSync(path.join(work, ".git"));
    const bash = tool({ mode: "workspace-write", protected: [path.join(work, ".git")] }, work, "Bash");
    await bash.execute("1", { command: "echo in > inside.txt" });
    expect(fs.readFileSync(path.join(work, "inside.txt"), "utf8")).toBe("in\n");
    const out = text(await bash.execute("2", { command: `echo out > ${outside}` }));
    expect(fs.existsSync(outside)).toBe(false);
    expect(out).toMatch(/not permitted|denied/i);
    await bash.execute("3", { command: "echo x > .git/HEAD" });
    expect(fs.existsSync(path.join(work, ".git", "HEAD"))).toBe(false);
    await bash.execute("4", { command: "echo t > $TMPDIR/sbx-tmp-ok-$$ && echo ok" });
  });

  test.if(darwin)("git commit works when the git dir is granted", async () => {
    const work = tmp();
    execFileSync("git", ["init", "-q"], { cwd: work });
    const bash = tool({ mode: "workspace-write", writable_roots: [path.join(work, ".git")] }, work, "Bash");
    const r = text(
      await bash.execute("1", {
        command: "git -c user.name=t -c user.email=t@x.invalid commit -q --allow-empty -m sbx && git log --oneline | wc -l",
      }),
    );
    expect(r.trim()).toBe("1");
  });
});
