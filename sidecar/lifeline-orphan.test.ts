// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// When the sidecar exits because its lifeline closed (🎯T166), the shell
// commands its seats were running must not keep running with nobody attached.
import { afterEach, expect, setDefaultTimeout, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { FIXTURE_CLEANUP_MS, launchAbortFixture } from "./testdata/abort-fixture";

setDefaultTimeout(90_000);
const fixtures: ReturnType<typeof launchAbortFixture>[] = [];
const leftovers: number[] = [];
afterEach(async () => {
  for (const pid of leftovers.splice(0)) { try { process.kill(pid, "SIGKILL"); } catch { /* gone */ } }
  for (const f of fixtures.splice(0)) await f.close();
}, FIXTURE_CLEANUP_MS * 2);

function alive(pid: number): boolean {
  try { process.kill(pid, 0); return true; } catch { return false; }
}

test("a lifeline exit does not orphan a running Bash command", async () => {
  const f = launchAbortFixture({ entrypoint: join(import.meta.dir, "testdata/abort-provider.ts"), args: ["--lifeline=stdin"] });
  fixtures.push(f);
  const host = await f.ready;
  const seat = "lifeline";
  host.send({ op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: f.dir, session_id: "s1" });
  expect((await host.next("ready", (e) => e.type === "ready" || e.type === "error")).type).toBe("ready");
  const marker = join(f.dir, "sleep.pid");
  // The fixture model answers this with Bash: `sleep 60 & echo $! > marker; wait`.
  host.send({ op: "prompt", seat, session_id: "s1", turn_id: "T1", text: `BASH_CANCEL ${marker}` });
  const deadline = Date.now() + 30_000;
  while (!existsSync(marker) || !readFileSync(marker, "utf8").trim()) {
    if (Date.now() > deadline) throw new Error("Bash never started");
    await Bun.sleep(50);
  }
  const sleepPid = Number(readFileSync(marker, "utf8").trim());
  leftovers.push(sleepPid);
  expect(alive(sleepPid)).toBe(true);

  // The process that started the sidecar goes away: its lifeline closes.
  f.child.stdin.end();
  const status = await f.exited;
  expect(status).toBe("exit=0 signal=none");
  await Bun.sleep(500);
  // The sidecar is gone; its seat's command must be gone too.
  expect({ sidecar: status, sleepAlive: alive(sleepPid) }).toEqual({ sidecar: "exit=0 signal=none", sleepAlive: false });
});

test("a SIGTERM does not orphan a running Bash command", async () => {
  const f = launchAbortFixture({ entrypoint: join(import.meta.dir, "testdata/abort-provider.ts"), args: ["--lifeline=stdin"] });
  fixtures.push(f);
  const host = await f.ready;
  const seat = "lifeline-term";
  host.send({ op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: f.dir, session_id: "s1" });
  expect((await host.next("ready", (e) => e.type === "ready" || e.type === "error")).type).toBe("ready");
  const marker = join(f.dir, "sleep.pid");
  host.send({ op: "prompt", seat, session_id: "s1", turn_id: "T1", text: `BASH_CANCEL ${marker}` });
  const deadline = Date.now() + 30_000;
  while (!existsSync(marker) || !readFileSync(marker, "utf8").trim()) {
    if (Date.now() > deadline) throw new Error("Bash never started");
    await Bun.sleep(50);
  }
  const sleepPid = Number(readFileSync(marker, "utf8").trim());
  leftovers.push(sleepPid);
  expect(alive(sleepPid)).toBe(true);

  f.child.kill("SIGTERM");
  const status = await f.exited;
  expect(status).toBe("exit=null signal=SIGTERM");
  await Bun.sleep(500);
  expect(alive(sleepPid)).toBe(false);
});
