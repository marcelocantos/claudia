// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { afterEach, expect, jest, test } from "bun:test";
import { once } from "node:events";
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { ABORT_SEMANTIC_MS, FIXTURE_CLEANUP_MS, FIXTURE_STARTUP_MS, launchAbortFixture } from "./testdata/abort-fixture";

const controlled = join(import.meta.dir, "testdata", "abort-startup-controlled.ts");
const fixtures: ReturnType<typeof launchAbortFixture>[] = [];
afterEach(async () => {
  jest.useRealTimers();
  for (const fixture of fixtures.splice(0)) await fixture.close();
}, FIXTURE_CLEANUP_MS);

function launch(mode: string, signal?: AbortSignal) {
  const fixture = launchAbortFixture({ entrypoint: controlled, args: [mode], signal });
  fixtures.push(fixture);
  return fixture;
}

test("startup waits for readiness and an actual connection, not process spawn", async () => {
  const fixture = launch("held");
  let ready = false;
  const pending = fixture.ready.then(host => { ready = true; return host; });
  const [chunk] = await once(fixture.child.stderr, "data");
  expect(chunk.toString()).toContain("setup-held");
  expect(ready).toBe(false);
  fixture.child.stdin.write("release\n");
  const host = await pending;
  expect(ready).toBe(true);
  expect(fixture.child.exitCode).toBeNull();
  host.close();
  await fixture.close();
  expect(existsSync(fixture.dir)).toBe(false);
  expect(fixture.child.signalCode).toBe("SIGKILL");
});

test("early process exit reports its status and reaps startup resources", async () => {
  const fixture = launch("exit");
  await expect(fixture.ready).rejects.toThrow("exit=17");
  expect(fixture.child.exitCode).toBe(17);
  expect(existsSync(fixture.dir)).toBe(false);
});

test("cancelling a held startup reaps its child and removes only its directory", async () => {
  const controller = new AbortController();
  const fixture = launch("held", controller.signal);
  const result = fixture.ready.catch(error => error);
  await once(fixture.child.stderr, "data");
  controller.abort(new Error("controlled setup cancellation"));
  expect((await result).message).toBe("controlled setup cancellation");
  expect(fixture.child.signalCode).toBe("SIGKILL");
  expect(existsSync(fixture.dir)).toBe(false);
});

test("the setup watchdog cancels a child that never announces readiness", async () => {
  jest.useFakeTimers();
  const fixture = launch("held");
  const result = fixture.ready.catch(error => error);
  await once(fixture.child.stderr, "data");
  jest.advanceTimersByTime(FIXTURE_STARTUP_MS);
  // Child exit is real process evidence; no fake timer stands in for reaping.
  expect((await result).message).toContain("fixture startup exceeded");
  expect(fixture.child.signalCode).toBe("SIGKILL");
  expect(existsSync(fixture.dir)).toBe(false);
});

test("a disconnected host rejects pending semantic waits", async () => {
  const fixture = launch("ready");
  const host = await fixture.ready;
  const pending = host.next("unanswered event", ev => ev.type === "never").catch(error => error);
  host.send({ close: true });
  expect((await pending).message).toContain("connection closed");
  await expect(host.next("later event", () => false)).rejects.toThrow("connection closed");
});

test("Bun setup can exceed the semantic budget, while a hung body still fails and cleans up", async () => {
  const dir = mkdtempSync(join(tmpdir(), "abrt-budget-"));
  const helper = join(import.meta.dir, "testdata", "abort-fixture.ts");
  const marker = join(dir, "disposed");
  // This deliberate delay crosses the real runner's five-second body budget.
  // No elapsed-time comparison determines the verdict: nested Bun must pass
  // the setup specimen and fail the hung semantic body, with cleanup evidence.
  const setupDelay = ABORT_SEMANTIC_MS + 1000;
  const run = async (name: string, source: string) => {
    const path = join(dir, `${name}.test.ts`);
    writeFileSync(path, source);
    const child = Bun.spawn([process.execPath, "test", path], { stdout: "pipe", stderr: "pipe" });
    try {
      const [code, stdout, stderr] = await Promise.all([child.exited, new Response(child.stdout).text(), new Response(child.stderr).text()]);
      return { code, output: stdout + stderr };
    } finally { child.kill(); await child.exited; }
  };
  try {
    const setup = await run("setup", `
      import { beforeEach, expect, test } from "bun:test";
      beforeEach(async () => { await Bun.sleep(${setupDelay}); }, ${FIXTURE_STARTUP_MS});
      test("setup is separate", () => { expect(true).toBe(true); }, ${ABORT_SEMANTIC_MS});
    `);
    expect(setup.code).toBe(0);
    const hung = await run("hung", `
      import { afterEach, beforeEach, test } from "bun:test";
      import { existsSync, writeFileSync } from "node:fs";
      import { launchAbortFixture } from ${JSON.stringify(helper)};
      let fixture;
      beforeEach(async () => {
        fixture = launchAbortFixture({ entrypoint: ${JSON.stringify(controlled)}, args: ["ready"] });
        await fixture.ready;
      }, ${FIXTURE_STARTUP_MS + FIXTURE_CLEANUP_MS});
      afterEach(async () => {
        await fixture.close();
        writeFileSync(${JSON.stringify(marker)}, JSON.stringify({ dirGone: !existsSync(fixture.dir), signal: fixture.child.signalCode }));
      }, ${FIXTURE_CLEANUP_MS});
      test("hung semantic body", async () => { await new Promise(() => {}); }, ${ABORT_SEMANTIC_MS});
    `);
    expect(hung.code).not.toBe(0);
    expect(hung.output).toContain(`timed out after ${ABORT_SEMANTIC_MS}ms`);
    const disposed = JSON.parse(readFileSync(marker, "utf8"));
    expect(disposed.dirGone).toBe(true);
    // Bun may terminate the owned child first when the body times out; our
    // teardown still awaits its exit before removing the fixture directory.
    expect(["SIGTERM", "SIGKILL"]).toContain(disposed.signal);
  } finally { rmSync(dir, { recursive: true, force: true }); }
}, FIXTURE_STARTUP_MS + FIXTURE_CLEANUP_MS);
