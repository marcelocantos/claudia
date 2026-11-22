// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Analysis reproduction, 2026-11-22 (turn-delivery-protocol report, defect
// P1). Not part of the sidecar suite: run_repros.sh copies it into a scratch
// copy of sidecar/ and runs it there against the real server.ts and the
// mock-model fixture. No provider, broker or installed service is involved.
//
// Release Stop is daemon handleRelease -> Agent.Quiesce -> ompControl quiesce
// -> {op: "abort", stop_turn: true} -> seat.stopTurn(), which empties the
// agent's queues. A prompt the host was told was accepted, queued behind the
// stopped turn, is removed from memory but its durable receipt is left as
// "accepted". The assertions are the desired state; they fail on c33e60e.
import { afterEach, expect, setDefaultTimeout, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { FIXTURE_CLEANUP_MS, launchAbortFixture, type Ev } from "./testdata/abort-fixture";

setDefaultTimeout(150_000);
const fixtures: ReturnType<typeof launchAbortFixture>[] = [];
afterEach(async () => {
  for (const f of fixtures.splice(0)) await f.close();
}, FIXTURE_CLEANUP_MS * 4);

const seat = "stop-discard";
const session = "s1";
const prompt = (request: string, turn: string, text: string) =>
  ({ op: "prompt", seat, session_id: session, request_id: request, rpc_id: `rpc-${request}-${turn}`, attempt_id: `att-${request}`, turn_id: turn, text });

test("a prompt discarded by Release Stop is settled, not left accepted", async () => {
  const f = launchAbortFixture();
  fixtures.push(f);
  const host = await f.ready;
  const load = { op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: f.dir, session_id: session };
  host.send(load);
  expect((await host.next("ready", (e) => e.type === "ready" || e.type === "error")).type).toBe("ready");

  host.send(prompt("r1", "T1", "HOLD the first turn open"));
  await host.next("r1 started", (e) => e.type === "receipt" && e.request_id === "r1" && e.receipt?.state === "started");
  host.send(prompt("r2", "T2", "Reply with exactly: DISCARDED-BY-STOP"));
  const acc = await host.next("r2 accepted", (e) => e.type === "accepted" && e.request_id === "r2");

  // Release Stop, as the daemon sends it.
  host.send({ op: "abort", seat, stop_turn: true });
  await host.next("T1 aborted", (e) => e.type === "turn_end" && e.turn_id === "T1" && e.stop_reason === "aborted");
  await Bun.sleep(2000);
  const ranAfterStop = host.events.some((e) => e.type === "text" && String(e.text).includes("DISCARDED-BY-STOP"));

  host.send({ op: "receipt", seat, session_id: session, request_id: "r2", rpc_id: "q-after-stop" });
  const afterStop = await host.next("receipt", (e) => e.type === "receipt" && e.rpc_id === "q-after-stop");

  // The host retries the same request id (ErrReceiptUncertain says to query
  // or retry by the original id).
  host.send(prompt("r2", "T3", "Reply with exactly: DISCARDED-BY-STOP"));
  const retryAck = await host.next("retry ack", (e: Ev) => e.request_id === "r2" && e.rpc_id === "rpc-r2-T3");
  const ranOnRetry = await host.next("retry runs", (e) => e.type === "text" && String(e.text).includes("DISCARDED-BY-STOP"), 8000)
    .then(() => true, () => false);

  // The next sidecar process loads the same store.
  const store = readFileSync(join(f.dir, "omp-seats", seat, `${session}.jsonl`), "utf8");
  const g = launchAbortFixture();
  fixtures.push(g);
  mkdirSync(join(g.dir, "omp-seats", seat), { recursive: true });
  writeFileSync(join(g.dir, "omp-seats", seat, `${session}.jsonl`), store);
  const h2 = await g.ready;
  h2.send({ ...load, cwd: g.dir });
  expect((await h2.next("ready after restart", (e) => e.type === "ready" || e.type === "error")).type).toBe("ready");
  h2.send(prompt("r3", "T4", "Reply with exactly: AFTER-RESTART"));
  await h2.next("r3 answered", (e) => e.type === "text" && String(e.text).includes("AFTER-RESTART"), 20_000).catch(() => undefined);
  await Bun.sleep(1000);
  const ranAfterRestart = h2.events.some((e) => e.type === "text" && String(e.text).includes("DISCARDED-BY-STOP"));
  const startedAfterRestart = h2.events.some((e) => e.type === "receipt" && e.request_id === "r2" && e.receipt?.state === "started");

  const observed = {
    acceptedBeforeStop: acc.receipt?.state,
    ranAfterStop,
    receiptAfterStop: afterStop.receipt?.state,
    retryAck: [retryAck.type, retryAck.receipt?.state],
    ranOnRetry,
    ranAfterRestart,
    startedAfterRestart,
  };
  console.log("ANALYSIS-P1 " + JSON.stringify(observed));
  // Desired: the stop discards r2 durably, so nothing ever runs it again.
  expect({ ranAfterStop, receiptAfterStop: afterStop.receipt?.state, ranAfterRestart })
    .toEqual({ ranAfterStop: false, receiptAfterStop: "cancelled", ranAfterRestart: false });
});
