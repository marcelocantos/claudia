// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// C4: a dropped seat must not run prompts queued behind its open turn, and a
// drop followed at once by load must leave one writer of the session store.
import { afterEach, expect, setDefaultTimeout, test } from "bun:test";
import { join } from "node:path";
import { FIXTURE_CLEANUP_MS, launchAbortFixture } from "./testdata/abort-fixture";

setDefaultTimeout(90_000);
const fixtures: ReturnType<typeof launchAbortFixture>[] = [];
afterEach(async () => {
  for (const f of fixtures.splice(0)) await f.close();
}, FIXTURE_CLEANUP_MS * 2);

test("drop of a busy seat does not start its queued prompt", async () => {
  const f = launchAbortFixture({ entrypoint: join(import.meta.dir, "testdata/abort-provider.ts") });
  fixtures.push(f);
  const host = await f.ready;
  const seat = "drop-queued";
  const session = "s1";
  host.send({ op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: f.dir, session_id: session });
  expect((await host.next("ready", (e) => e.type === "ready" || e.type === "error")).type).toBe("ready");
  const prompt = (request: string, turn: string, text: string) =>
    ({ op: "prompt", seat, session_id: session, request_id: request, rpc_id: `rpc-${request}`, attempt_id: `att-${request}`, turn_id: turn, text });
  host.send(prompt("r1", "T1", "HOLD the first turn open"));
  await host.next("r1 started", (e) => e.type === "receipt" && e.request_id === "r1" && e.receipt?.state === "started");
  host.send(prompt("r2", "T2", "queued behind the held turn"));
  await host.next("r2 accepted", (e) => e.type === "accepted" && e.request_id === "r2");

  host.send({ op: "drop", seat, session_id: session });
  await host.next("dropped", (e) => e.type === "dropped" && e.seat === seat);
  const droppedAt = host.events.findIndex((e) => e.type === "dropped");
  await Bun.sleep(3000);

  const after = host.events.slice(droppedAt + 1).filter((e) => e.seat === seat && e.type !== "turn");
  expect(after.map((e) => [e.type, e.turn_id, e.request_id, e.text ?? e.receipt?.state])).toEqual([]);

  host.send({ op: "abort", seat });
  const refused = await host.next("abort refused", (e) => e.type === "seat_error" && e.seat === seat);
  expect(refused.text).toBe("seat is not loaded");
});

test("drop and reload leaves one writer on the session store", async () => {
  const f = launchAbortFixture({ entrypoint: join(import.meta.dir, "testdata/abort-provider.ts") });
  fixtures.push(f);
  const host = await f.ready;
  const seat = "drop-reload";
  const session = "s1";
  const load = { op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: f.dir, session_id: session };
  host.send(load);
  expect((await host.next("ready", (e) => e.type === "ready" || e.type === "error")).type).toBe("ready");
  const prompt = (request: string, turn: string, text: string) =>
    ({ op: "prompt", seat, session_id: session, request_id: request, rpc_id: `rpc-${request}`, attempt_id: `att-${request}`, turn_id: turn, text });
  host.send(prompt("r1", "T1", "HOLD the first turn open"));
  await host.next("r1 started", (e) => e.type === "receipt" && e.request_id === "r1" && e.receipt?.state === "started");
  host.send(prompt("r2", "T2", "queued behind the held turn"));
  await host.next("r2 accepted", (e) => e.type === "accepted" && e.request_id === "r2");
  host.send({ op: "drop", seat }, load);
  await host.next("reloaded", (e) => e.type === "ready" && e.how === "launched" && host.events.some((d) => d.type === "dropped"));
  await Bun.sleep(3000);
  host.send({ op: "abort", seat });
  const live = await host.next("snapshot", (e) => e.type === "aborted" && e.seat === seat);
  const liveUsers = (live.snapshot.messages as { role: string; content: unknown }[])
    .filter((m) => m.role === "user").map((m) => JSON.stringify(m.content));
  const { readFileSync } = await import("node:fs");
  const disk = readFileSync(join(f.dir, "omp-seats", seat, `${session}.jsonl`), "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l));
  const diskUsers = disk.filter((e) => e.type === "message" && e.message.role === "user").map((e) => JSON.stringify(e.message.content));
  expect(diskUsers).toEqual(liveUsers);
});

test("drop and reload runs a queued prompt once", async () => {
  const { existsSync, readFileSync, mkdirSync, writeFileSync } = await import("node:fs");
  const f = launchAbortFixture({ entrypoint: join(import.meta.dir, "testdata/abort-provider.ts") });
  fixtures.push(f);
  const host = await f.ready;
  const seat = "drop-twice";
  const session = "s1";
  const load = { op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: f.dir, session_id: session };
  host.send(load);
  expect((await host.next("ready", (e) => e.type === "ready" || e.type === "error")).type).toBe("ready");
  const prompt = (request: string, turn: string, text: string) =>
    ({ op: "prompt", seat, session_id: session, request_id: request, rpc_id: `rpc-${request}`, attempt_id: `att-${request}`, turn_id: turn, text });
  const marker = join(f.dir, "bash.pid");
  host.send(prompt("r1", "T1", `BASH_CANCEL ${marker}`));
  while (!existsSync(marker)) await Bun.sleep(50);
  host.send(prompt("r2", "T2", "Reply with exactly: ONCE-ONLY"));
  await host.next("r2 accepted", (e) => e.type === "accepted" && e.request_id === "r2");
  host.send({ op: "drop", seat }, load);
  await host.next("reloaded", (e) => e.type === "ready" && e.how === "launched" && host.events.some((d) => d.type === "dropped"));
  await Bun.sleep(3000);
  host.send(prompt("r3", "T3", "Reply with exactly: AFTER-RELOAD"));
  await host.next("r3 answered", (e) => e.type === "text" && String(e.text).includes("AFTER-RELOAD"), 20_000).catch(() => undefined);
  await Bun.sleep(1000);
  // The dropped seat must not run the queue (that run would be turn T2,
  // which it still had in memory). The reloaded seat runs the accepted
  // receipt once. The host turn id is not in the receipt, so the run takes
  // the turn that drains the queue.
  const answers = host.events.filter((e) => e.type === "text" && String(e.text).includes("ONCE-ONLY"))
    .map((e) => [e.request_id, e.text]);
  const store = readFileSync(join(f.dir, "omp-seats", seat, `${session}.jsonl`), "utf8");

  const g = launchAbortFixture({ entrypoint: join(import.meta.dir, "testdata/abort-provider.ts") });
  fixtures.push(g);
  mkdirSync(join(g.dir, "omp-seats", seat), { recursive: true });
  writeFileSync(join(g.dir, "omp-seats", seat, `${session}.jsonl`), store);
  const h2 = await g.ready;
  h2.send({ ...load, cwd: g.dir });
  const reload = await h2.next("restart load", (e) => e.type === "ready" || e.type === "error");
  expect({ answers, restartLoad: reload.type === "ready" ? "ready" : `error: ${reload.text}` })
    .toEqual({ answers: [["r2", "ONCE-ONLY"]], restartLoad: "ready" });
});
