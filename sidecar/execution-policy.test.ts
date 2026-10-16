// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, test } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { createMockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";
import { codingTools, type SandboxPolicy } from "./coding.ts";
import { cursorExecHandlers } from "./cursorexec.ts";
import { createSeatAgent } from "./seat.ts";

registerMockApi();
const dirs: string[] = [];
function temp(home = false) {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(home ? os.homedir() : os.tmpdir(), ".claudia-policy-")));
  dirs.push(dir);
  return dir;
}
afterEach(() => { for (const dir of dirs.splice(0)) fs.rmSync(dir, { recursive: true, force: true }); });
const text = (r: any) => r.content.map((c: any) => c.text).join("");

function adapters(cwdOf: () => string, policyOf: () => SandboxPolicy | undefined) {
  // Retain these handles: removing a tool from a later list is not enforcement.
  const tools = codingTools(cwdOf, () => 100_000, policyOf);
  const call = (name: string, args: unknown) => tools.find((t) => t.name === name)!.execute("id", args);
  const cursor = cursorExecHandlers(cwdOf, () => 100_000, policyOf) as any;
  return [
    { name: "ordinary", write: (p: string, s: string) => call("Write", { path: p, content: s }), read: (p: string) => call("Read", { path: p }), shell: (s: string) => call("Bash", { command: s }) },
    { name: "cursor-native", write: (p: string, s: string) => cursor.write({ path: p, fileText: s }), read: (p: string) => cursor.read({ path: p }), shell: (s: string) => cursor.shell({ command: s }) },
    { name: "cursor-pi", write: (p: string, s: string) => cursor.piWrite({ args: { path: p, content: s } }), read: (p: string) => cursor.piRead({ args: { path: p } }), shell: (s: string) => cursor.piBash({ args: { command: s } }) },
  ];
}

describe("one seat policy through every tool adapter", () => {
  test("file grants, protected roots and symlink escapes have identical effects", async () => {
    const dir = temp(), outside = temp(), granted = temp();
    fs.mkdirSync(path.join(dir, ".git"));
    fs.writeFileSync(path.join(dir, ".git", "HEAD"), "original");
    fs.writeFileSync(path.join(outside, "secret"), "OUTSIDE-CANARY");
    fs.symlinkSync(outside, path.join(dir, "escape"));
    fs.symlinkSync(path.join(outside, "not-created"), path.join(dir, "dangling"));
    fs.symlinkSync(path.join(dir, ".git"), path.join(dir, "git-alias"));
    const policy: SandboxPolicy = { mode: "workspace-write", writable_roots: [granted], protected: [path.join(dir, ".git")] };
    for (const a of adapters(() => dir, () => policy)) {
      await a.write(`${a.name}.txt`, "inside");
      expect(fs.readFileSync(path.join(dir, `${a.name}.txt`), "utf8")).toBe("inside");
      await a.write(path.join(granted, a.name), "granted");
      expect(text(await a.read(path.join(granted, a.name)))).toBe("granted");
      expect(text(await a.write("escape/new/denied", "no"))).toContain("outside");
      expect(text(await a.write("dangling", "no"))).toContain("outside");
      expect(fs.existsSync(path.join(outside, "not-created"))).toBe(false);
      expect(text(await a.read("escape/secret"))).not.toContain("OUTSIDE-CANARY");
      expect(text(await a.write("git-alias/HEAD", "no"))).toContain("protected");
      expect(fs.readFileSync(path.join(dir, ".git", "HEAD"), "utf8")).toBe("original");
      expect(fs.existsSync(path.join(outside, "new"))).toBe(false);
    }
  });

  test("Cursor deletion unlinks an allowed symlink without deleting its target", async () => {
    const dir = temp(), outside = temp();
    fs.mkdirSync(path.join(dir, ".git"));
    const cursor = cursorExecHandlers(() => dir, () => 100_000,
      () => ({ mode: "workspace-write", protected: [path.join(dir, ".git")] })) as any;
    for (const target of [path.join(dir, "target"), path.join(outside, "target"), path.join(dir, ".git", "HEAD")]) {
      fs.writeFileSync(target, "keep");
      fs.symlinkSync(target, path.join(dir, "link"));
      expect((await cursor.delete({ path: "link" })).isError).toBe(false);
      expect(fs.readFileSync(target, "utf8")).toBe("keep");
      expect(fs.existsSync(path.join(dir, "link"))).toBe(false);
    }
    fs.symlinkSync(path.join(outside, "absent"), path.join(dir, "dangling"));
    expect((await cursor.delete({ path: "dangling" })).isError).toBe(false);
    expect(() => fs.lstatSync(path.join(dir, "dangling"))).toThrow();
    // The final entry may be unlinked; an escaped or protected parent may not.
    fs.symlinkSync(outside, path.join(dir, "escape"));
    fs.symlinkSync(path.join(dir, ".git"), path.join(dir, "git-alias"));
    expect((await cursor.delete({ path: "escape/target" })).isError).toBe(true);
    expect((await cursor.delete({ path: "git-alias/HEAD" })).isError).toBe(true);
    expect(fs.readFileSync(path.join(outside, "target"), "utf8")).toBe("keep");
    expect(fs.readFileSync(path.join(dir, ".git", "HEAD"), "utf8")).toBe("keep");
  });

  test("retained handles obey read-only after a policy change", async () => {
    let dir = temp();
    let policy: SandboxPolicy | undefined;
    const entries = adapters(() => dir, () => policy);
    dir = temp(); // Rebind without reconstructing the Cursor handlers.
    policy = { mode: "read-only" };
    for (const a of entries) {
      expect(text(await a.write("denied.txt", "no"))).toContain("read-only");
      expect(text(await a.shell("echo no > denied-shell.txt"))).toContain("read-only");
      expect(fs.existsSync(path.join(dir, "denied.txt"))).toBe(false);
      expect(fs.existsSync(path.join(dir, "denied-shell.txt"))).toBe(false);
    }
  });

  test("shell admission rechecks authority after asynchronous branch inspection", async () => {
    const dir = temp(), rebound = temp();
    let cwd = dir;
    let policy: SandboxPolicy | undefined;
    for (const a of adapters(() => cwd, () => policy)) {
      policy = undefined;
      const pending = a.shell("echo no > late-write");
      policy = { mode: "read-only" };
      expect(text(await pending)).toContain("read-only");
      expect(fs.existsSync(path.join(dir, "late-write"))).toBe(false);
      policy = undefined;
      const beforeRebind = a.shell("echo no > stale-directory");
      cwd = rebound;
      expect(text(await beforeRebind)).toContain("working directory changed");
      expect(fs.existsSync(path.join(dir, "stale-directory"))).toBe(false);
      expect(fs.existsSync(path.join(rebound, "stale-directory"))).toBe(false);
      cwd = dir;
    }
    policy = { mode: "workspace-write", writable_roots: [rebound] };
    const cursor = cursorExecHandlers(() => cwd, () => 100_000, () => policy) as any;
    const beforeRevoke = cursor.shell({ command: "echo no > revoked", workingDirectory: rebound });
    policy = { mode: "workspace-write" };
    expect(text(await beforeRevoke)).toContain("outside seat roots");
    expect(fs.existsSync(path.join(rebound, "revoked"))).toBe(false);
  });

  test("Cursor edit, delete and streaming shell cannot bypass read-only", async () => {
    const dir = temp();
    fs.writeFileSync(path.join(dir, "keep"), "original");
    const cursor = cursorExecHandlers(() => dir, () => 100_000, () => ({ mode: "read-only" })) as any;
    for (const result of [
      await cursor.delete({ path: "keep" }),
      await cursor.piEdit({ args: { path: "keep", edits: [{ oldText: "original", newText: "no" }] } }),
      await cursor.shellStream({ command: "echo no > keep" }),
    ]) expect(text(result)).toContain("read-only");
    expect(fs.readFileSync(path.join(dir, "keep"), "utf8")).toBe("original");
  });

  test("Cursor directory and search entry points refuse symlink escapes", async () => {
    const dir = temp(), outside = temp();
    fs.writeFileSync(path.join(outside, "canary"), "OUTSIDE-CANARY");
    fs.symlinkSync(outside, path.join(dir, "escape"));
    const cursor = cursorExecHandlers(() => dir, () => 100_000) as any;
    for (const result of [
      await cursor.ls({ path: "escape" }),
      await cursor.piLs({ args: { path: "escape" } }),
      await cursor.grep({ path: "escape", pattern: "CANARY" }),
      await cursor.piGrep({ args: { path: "escape", pattern: "CANARY" } }),
      await cursor.piFind({ args: { path: "escape", pattern: "*" } }),
    ]) { expect(result.isError).toBe(true); expect(text(result)).toContain("escapes"); }
  });

  test("ambient ripgrep configuration cannot enable recursive symlink traversal", async () => {
    const dir = temp(), outside = temp();
    fs.writeFileSync(path.join(outside, "secret"), "OUTSIDE-CANARY");
    fs.symlinkSync(outside, path.join(dir, "escape"));
    const config = path.join(outside, "ripgrep-config");
    fs.writeFileSync(config, "--follow\n");
    const before = process.env.RIPGREP_CONFIG_PATH;
    process.env.RIPGREP_CONFIG_PATH = config;
    try {
      const tool = codingTools(() => dir).find((t) => t.name === "Grep")!;
      const cursor = cursorExecHandlers(() => dir, () => 100_000) as any;
      for (const result of [
        await tool.execute("id", { pattern: "CANARY" }),
        await cursor.grep({ pattern: "CANARY", path: "." }),
        await cursor.piGrep({ args: { pattern: "CANARY", path: "." } }),
      ]) expect(text(result)).not.toContain("OUTSIDE-CANARY");
    } finally {
      if (before === undefined) delete process.env.RIPGREP_CONFIG_PATH;
      else process.env.RIPGREP_CONFIG_PATH = before;
    }
  });

  test.if(process.platform === "darwin")("shell adapters enforce protected and outside writes using the seat grant", async () => {
    const dir = temp(), outside = temp(true);
    fs.mkdirSync(path.join(dir, ".git"));
    const policy: SandboxPolicy = { mode: "workspace-write", protected: [path.join(dir, ".git")] };
    for (const a of adapters(() => dir, () => policy)) {
      await a.shell(`echo yes > ${a.name}.txt`);
      expect(fs.readFileSync(path.join(dir, `${a.name}.txt`), "utf8")).toBe("yes\n");
      await a.shell(`echo no > '${outside}/denied'`);
      await a.shell("echo no > .git/HEAD");
      expect(fs.existsSync(path.join(outside, "denied"))).toBe(false);
      expect(fs.existsSync(path.join(dir, ".git", "HEAD"))).toBe(false);
    }
    const cursor = cursorExecHandlers(() => dir, () => 100_000, () => policy) as any;
    expect((await cursor.shell({ command: "pwd", workingDirectory: outside })).isError).toBe(true);
    fs.mkdirSync(path.join(dir, "sub"));
    // The execution cwd is not a new sandbox authority: the rest of the
    // original seat grant remains writable, and protected roots remain denied.
    await cursor.shellStream({ command: "echo yes > ../from-sub", workingDirectory: "sub" });
    expect(fs.readFileSync(path.join(dir, "from-sub"), "utf8")).toBe("yes\n");
  });

  test("the real seat wires dynamic policy into Cursor callbacks across a provider switch", async () => {
    const dir = temp();
    const observed: string[] = [];
    const mock = (provider: string) => createMockModel({ provider, id: provider, handler: async (_context, options) => {
      const handlers = (options as any).cursorExecHandlers;
      expect(handlers).toBeDefined();
      observed.push(text(await handlers.write({ path: "denied", fileText: "no" })));
      observed.push(text(await handlers.piBash({ args: { command: "echo no > denied-shell" } })));
      return { content: ["checked"] };
    } });
    const from = mock("anthropic"), to = mock("cursor");
    const seat = createSeatAgent({ provider: "anthropic", model: "anthropic", token: "test", cwd: dir,
      emit: () => {}, callTool: async () => "", modelOverride: from as never,
      resolveModelOverride: () => to as never });
    seat.setSandbox({ mode: "read-only" });
    await seat.prompt("check before switch");
    await seat.switchModel("cursor", "cursor");
    seat.rebind(() => {}, async () => "");
    await seat.prompt("check after switch");
    expect(observed).toHaveLength(4);
    expect(observed.every((s) => s.includes("read-only"))).toBe(true);
    expect(fs.existsSync(path.join(dir, "denied"))).toBe(false);
    expect(fs.existsSync(path.join(dir, "denied-shell"))).toBe(false);
  });
});
