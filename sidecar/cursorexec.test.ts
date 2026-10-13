import { describe, expect, test } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { cursorExecHandlers, readSelector } from "./cursorexec.ts";

function seat() {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "cursorexec-")));
  return { dir, h: cursorExecHandlers(() => dir, () => 100_000) as any };
}
const text = (r: any) => r.content.map((c: any) => c.text).join("");

describe("cursor exec handlers", () => {
  test("every Cursor tool frame has a handler (none answers Tool not available)", () => {
    const { h } = seat();
    for (const k of ["read", "ls", "grep", "write", "delete", "shell", "shellStream", "piRead", "piBash", "piEdit", "piWrite", "piGrep", "piFind", "piLs"]) {
      expect(typeof h[k]).toBe("function");
    }
  });

  test("write then read round-trips inside the workdir", async () => {
    const { dir, h } = seat();
    const w = await h.write({ path: "sub/a.txt", fileText: "one\ntwo\nthree\n", toolCallId: "w1" });
    expect(w.isError).toBe(false);
    expect(fs.readFileSync(path.join(dir, "sub/a.txt"), "utf8")).toBe("one\ntwo\nthree\n");
    const r = await h.read({ path: path.join(dir, "sub/a.txt"), toolCallId: "r1" });
    expect(r.isError).toBe(false);
    expect(text(r)).toBe("one\ntwo\nthree\n");
    const ranged = await h.piRead({ toolCallId: "r2", args: { path: "sub/a.txt", offset: 2, limit: 1 } });
    expect(text(ranged)).toBe("two");
    const sel = await h.read({ path: "sub/a.txt:raw:2+2", toolCallId: "r3" });
    expect(text(sel)).toBe("two\nthree");
  });

  test("paths outside the workdir are refused", async () => {
    const { h } = seat();
    const r = await h.read({ path: "/etc/hosts", toolCallId: "x" });
    expect(r.isError).toBe(true);
    expect(text(r)).toContain("escapes working directory");
    const w = await h.piWrite({ toolCallId: "y", args: { path: "../escape.txt", content: "no" } });
    expect(w.isError).toBe(true);
    const s = await h.shell({ command: "pwd", workingDirectory: "/tmp", toolCallId: "z" });
    expect(s.isError).toBe(true);
  });

  test("shell runs in the workdir and reports failure", async () => {
    const { dir, h } = seat();
    const ok = await h.shell({ command: "pwd && echo hi > made.txt", toolCallId: "s1" });
    expect(ok.isError).toBe(false);
    expect(text(ok)).toContain(dir);
    expect(fs.existsSync(path.join(dir, "made.txt"))).toBe(true);
    const bad = await h.piBash({ toolCallId: "s2", args: { command: "exit 3" } });
    expect(bad.isError).toBe(true);
    expect(text(bad)).toContain("exit 3");
  });

  test("piEdit replaces text and ls/find/grep see the tree", async () => {
    const { dir, h } = seat();
    fs.writeFileSync(path.join(dir, "e.go"), "package x // OLD\n");
    const e = await h.piEdit({ toolCallId: "e1", args: { path: "e.go", edits: [{ oldText: "OLD", newText: "NEW" }] } });
    expect(e.isError).toBe(false);
    expect(fs.readFileSync(path.join(dir, "e.go"), "utf8")).toBe("package x // NEW\n");
    expect(text(await h.piLs({ toolCallId: "l", args: {} }))).toContain("e.go");
    expect(text(await h.piFind({ toolCallId: "f", args: { pattern: "*.go" } }))).toContain("e.go");
    expect(text(await h.piGrep({ toolCallId: "g", args: { pattern: "NEW" } }))).toContain("e.go");
  });

  test("readSelector strips OMP selectors", () => {
    expect(readSelector("a.txt")).toEqual({ file: "a.txt", start: undefined, count: undefined, end: undefined });
    expect(readSelector("a.txt:raw")).toMatchObject({ file: "a.txt" });
    expect(readSelector("a.txt:raw:3+2")).toMatchObject({ file: "a.txt", start: 3, count: 2 });
    expect(readSelector("a.txt:3-5:raw")).toMatchObject({ file: "a.txt", start: 3, end: 5 });
  });
});
