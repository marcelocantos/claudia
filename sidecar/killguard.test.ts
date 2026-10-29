// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { SeatProcessOwnership, guardKill, type ProcessRow } from "./killguard.ts";
import { runShell } from "./execution.ts";
import { codingTools } from "./coding.ts";
import { cursorExecHandlers } from "./cursorexec.ts";

const rows: ProcessRow[] = [
  { pid: 410, ppid: 1, pgid: 410, started: "Thu Oct 29 10:00:00 2026", name: "/bin/bash", command: "bash -lc sleep 60" },
  { pid: 411, ppid: 410, pgid: 410, started: "Thu Oct 29 10:00:01 2026", name: "/bin/sleep", command: "sleep 60" },
  { pid: 520, ppid: 1, pgid: 520, started: "Thu Oct 29 10:00:00 2026", name: "/usr/bin/journey-suite", command: "journey-suite --run" },
];
const snapshot = async () => rows.map((r) => ({ ...r }));
const denied = (p: Promise<unknown>, fragment: string) => expect(p).rejects.toThrow(fragment);

describe("killguard pre-exec", () => {
  test("pre-aborted pkill never calls the shell runner or process snapshot (T235)", async () => {
    const abort = new AbortController();
    abort.abort();
    let snapshots = 0, launches = 0;
    const owner = new SeatProcessOwnership(async () => { snapshots++; return rows; });
    const runner: any = async () => { launches++; throw new Error("must not launch"); };
    const result = await runShell(() => ".", () => undefined,
      { command: "/usr/bin/pkill -f journey-suite", timeoutMs: 1000, maxBuffer: 1000, signal: abort.signal }, owner, runner);
    expect(result.error?.code).toBe("ABORT_ERR");
    expect(result.status).toBeNull();
    expect(snapshots).toBe(0);
    expect(launches).toBe(0);
  });
  test("refuses pattern forms with real snapshot candidates; never launches a shell", async () => {
    const owner = new SeatProcessOwnership(snapshot);
    let launches = 0;
    const launch: any = async () => { launches++; return { status: 0, stdout: "", stderr: "" }; };
    const dir = mkdtempSync(join(tmpdir(), "killguard-"));
    try {
      for (const command of ["pkill -f journey-suite", "/usr/bin/pkill journey-suite", "sudo pkill -9 journey-suite", "killall journey-suite", "kill $(pgrep -f journey-suite)", "kill `pgrep journey-suite`", "kill -TERM 520", "kill -9 -520", "kill $pid", "env -i pkill journey-suite", "sudo -n /usr/bin/pkill journey-suite", "bash -c 'killall journey-suite'"]) {
        await denied(runShell(() => dir, () => undefined, { command, timeoutMs: 1000, maxBuffer: 1000 }, owner, launch), "killguard:");
      }
      expect(launches).toBe(0);
      await denied(guardKill(dir, "pkill -f journey-suite", owner), "520 /usr/bin/journey-suite");
      await denied(guardKill(dir, "killall journey-suite", owner), "520 /usr/bin/journey-suite");
      await denied(guardKill(dir, "kill 520", owner), "foreign");
      await denied(guardKill(dir, "echo $(pkill -f journey-suite)", owner), "520 /usr/bin/journey-suite");
      await denied(guardKill(dir, "kill $(pgrep -f journey-suite)", owner), "520 /usr/bin/journey-suite");
      for (const command of ["echo `pkill -f journey-suite`", "printf '%s' `killall journey-suite`", "echo $(pkill -f journey-suite)"]) {
        await denied(runShell(() => dir, () => undefined,
          { command, timeoutMs: 1000, maxBuffer: 1000 }, owner, launch), "520 /usr/bin/journey-suite");
      }
      expect(launches).toBe(0);
      await guardKill(dir, "echo 'pkill is only prose'", owner);
    } finally { rmSync(dir, { recursive: true, force: true }); }
  });

  test("OS launch provenance permits own PID and group member, but not reuse, group targets or restart", async () => {
    const owner = new SeatProcessOwnership(snapshot);
    await owner.launched(410);
    await guardKill(".", "kill 410", owner);
    await guardKill(".", "kill 411", owner);
    const dir = mkdtempSync(join(tmpdir(), "killguard-own-"));
    try {
      let called = false;
      const fake: any = async (_bin: string, _argv: string[]) => { called = true; return { status: 0, stdout: "fake-no-signal", stderr: "" }; };
      const result = await runShell(() => dir, () => undefined, { command: "kill 411", timeoutMs: 1000, maxBuffer: 1000 }, owner, fake);
      expect(called).toBe(true);
      expect(result.stdout).toBe("fake-no-signal");
    } finally { rmSync(dir, { recursive: true, force: true }); }
    await denied(guardKill(".", "kill 520", owner), "foreign");
    await denied(guardKill(".", "kill -410", owner), "group");
    const later = new SeatProcessOwnership(async () => rows.map((r) => r.pid === 410 ? { ...r, started: "Fri Oct 30 10:00:00 2026" } : r));
    await later.launched(410);
    await denied(guardKill(".", "kill 411", later), "unowned");
    await denied(guardKill(".", "kill 410", new SeatProcessOwnership(snapshot)), "unowned");
    await denied(guardKill(".", "kill 410", new SeatProcessOwnership(async () => null)), "snapshot unavailable");
  });

  test("both production adapters reach the guard before execution", async () => {
    const dir = mkdtempSync(join(tmpdir(), "killguard-tools-"));
    try {
      const owner = new SeatProcessOwnership(snapshot);
      const bash = codingTools(() => dir, () => 100_000, () => undefined, owner).find((t) => t.name === "Bash")!;
      const cursor = cursorExecHandlers(() => dir, () => 100_000, () => undefined, owner) as any;
      const a = await bash.execute("id", { command: "pkill -f journey-suite" }) as any;
      expect(a.isError).toBe(true);
      expect(a.content[0].text).toContain("520 /usr/bin/journey-suite");
      const b = await cursor.shell({ command: "killall journey-suite" });
      expect(b.isError).toBe(true);
      expect(b.content[0].text).toContain("520 /usr/bin/journey-suite");
      const c = await cursor.piBash({ args: { command: "kill 520" } });
      expect(c.isError).toBe(true);
      expect(c.content[0].text).toContain("foreign");
    } finally { rmSync(dir, { recursive: true, force: true }); }
  });
});
