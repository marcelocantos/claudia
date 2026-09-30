// 🎯T161: the sidecar is one process serving every seat. A seat's Bash,
// Glob or Grep must not stop the others' loads, adopts and prompts while it
// runs — a synchronous child did, for up to 60 s per command.

import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { codingTools, runCommand } from "./coding.ts";

function tool(dir: string, name: string) {
  const t = codingTools(() => dir, () => 100_000).find((x) => x.name === name);
  if (!t) throw new Error(`no ${name} tool`);
  return t;
}

function text(result: unknown): string {
  return (result as { content: { text: string }[] }).content[0].text;
}

describe("coding tools do not block the sidecar (🎯T161)", () => {
  test("the event loop runs while a Bash command does", async () => {
    const dir = mkdtempSync(join(tmpdir(), "coding-"));
    try {
      // A timer due long before the command ends. With a synchronous child
      // it cannot fire until the command has finished.
      let firedWhileRunning = false;
      let running = true;
      const timer = setTimeout(() => {
        firedWhileRunning = running;
      }, 20);
      const result = await tool(dir, "Bash").execute("c1", { command: "sleep 1; echo done" });
      running = false;
      clearTimeout(timer);
      expect(text(result)).toBe("done\n");
      expect(firedWhileRunning).toBe(true);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  test("Bash still reports a failing command's status and output", async () => {
    const dir = mkdtempSync(join(tmpdir(), "coding-"));
    try {
      const result = await tool(dir, "Bash").execute("c2", { command: "echo out; echo err >&2; exit 3" });
      expect(text(result)).toBe("exit 3\nout\nerr\n");
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  test("a command past its bound is killed and reported", async () => {
    const started = Date.now();
    const r = await runCommand("/bin/bash", ["-c", "sleep 30"], { timeoutMs: 200, maxBuffer: 1000 });
    expect(r.status).toBeNull();
    expect(r.error?.code).toBe("ETIMEDOUT");
    expect(Date.now() - started).toBeLessThan(20_000);
  });

  test("output past maxBuffer is cut off and reported", async () => {
    const r = await runCommand("/bin/bash", ["-c", "yes | head -c 100000"], { timeoutMs: 20_000, maxBuffer: 1000 });
    expect(r.error?.code).toBe("ENOBUFS");
    expect(r.stdout.length).toBeLessThanOrEqual(1000);
  });

  test("a missing binary is ENOENT, which Grep's fallback reads", async () => {
    const r = await runCommand("claudia-no-such-binary", [], { timeoutMs: 1000, maxBuffer: 1000 });
    expect(r.error?.code).toBe("ENOENT");
  });
});
