// 🎯T219 / 🎯jevons/T1216: seat-owned commands must tear down the whole
// process group (no disowned grandchildren after timeout or seat stop).

import { describe, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { runCommand } from "./process.ts";
import { SeatCommandScope } from "./seat-command-scope.ts";

function processAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

function grandchildOrphanScript(marker: string): string {
  return `/bin/sleep 120 & echo $! > ${marker}; exec /bin/sleep 120`;
}

describe("seat-owned command process groups (🎯T219)", () => {
  test("tool timeout kills background grandchild (T1216 orphan class)", async () => {
    const dir = mkdtempSync(join(tmpdir(), "t219-timeout-"));
    const marker = join(dir, "grand.pid");
    try {
      const r = await runCommand("/bin/bash", ["-c", grandchildOrphanScript(marker)], {
        timeoutMs: 400,
        maxBuffer: 10_000,
      });
      expect(r.error?.code).toBe("ETIMEDOUT");
      await new Promise((r) => setTimeout(r, 800));
      const grand = Number.parseInt(readFileSync(marker, "utf8").trim(), 10);
      expect(Number.isFinite(grand)).toBe(true);
      expect(processAlive(grand)).toBe(false);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  test("maxBuffer abort kills background grandchild", async () => {
    const dir = mkdtempSync(join(tmpdir(), "t219-buf-"));
    const marker = join(dir, "grand.pid");
    const script = `/bin/sleep 120 & echo $! > ${marker}; exec yes`;
    try {
      const r = await runCommand("/bin/bash", ["-c", script], {
        timeoutMs: 20_000,
        maxBuffer: 500,
      });
      expect(r.error?.code).toBe("ENOBUFS");
      await new Promise((r) => setTimeout(r, 800));
      const grand = Number.parseInt(readFileSync(marker, "utf8").trim(), 10);
      expect(processAlive(grand)).toBe(false);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  test("SeatCommandScope.killAll ends native shell (stop/reap path)", async () => {
    const dir = mkdtempSync(join(tmpdir(), "t219-stop-"));
    const marker = join(dir, "grand.pid");
    const scope = new SeatCommandScope();
    const started = runCommand("/bin/bash", ["-c", grandchildOrphanScript(marker)], {
      timeoutMs: 60_000,
      maxBuffer: 10_000,
      commandScope: scope,
    });
    await new Promise((r) => setTimeout(r, 300));
    scope.killAll();
    await started;
    await new Promise((r) => setTimeout(r, 800));
    const grand = Number.parseInt(readFileSync(marker, "utf8").trim(), 10);
    expect(processAlive(grand)).toBe(false);
  });

  test("AbortSignal on runCommand tears down the group", async () => {
    const dir = mkdtempSync(join(tmpdir(), "t219-abort-"));
    const marker = join(dir, "grand.pid");
    const ctl = new AbortController();
    try {
      const p = runCommand("/bin/bash", ["-c", grandchildOrphanScript(marker)], {
        timeoutMs: 60_000,
        maxBuffer: 10_000,
        signal: ctl.signal,
      });
      await new Promise((r) => setTimeout(r, 300));
      ctl.abort();
      const r = await p;
      expect(r.error?.code).toBe("ABORT_ERR");
      await new Promise((r) => setTimeout(r, 800));
      const grand = Number.parseInt(readFileSync(marker, "utf8").trim(), 10);
      expect(processAlive(grand)).toBe(false);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
