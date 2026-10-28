// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// An owner abort, then a new send, through the real sidecar process and its
// socket (jevons J3-cancel-and-send, 2026-10-19). Every answer the host reads
// as part of a turn (text, turn_end, a turn's error) names that turn: the Go
// pump refuses one without turn_id, and the refusal reached the owner as a
// failed turn while the aborted turn's own end never arrived.

import { afterEach, beforeEach, expect, setDefaultTimeout, test } from "bun:test";
import { ABORT_SEMANTIC_MS, FIXTURE_CLEANUP_MS, FIXTURE_STARTUP_MS, Host, launchAbortFixture, type Ev } from "./testdata/abort-fixture";

// Setup has its own bounded lifetime. Each semantic body keeps the original
// five-second limit; cold vendor imports no longer spend that allowance.
setDefaultTimeout(ABORT_SEMANTIC_MS);
let fixture: ReturnType<typeof launchAbortFixture>;
let host: Host;
beforeEach(async () => {
  fixture = launchAbortFixture();
  host = await fixture.ready;
}, FIXTURE_STARTUP_MS + FIXTURE_CLEANUP_MS);
afterEach(async () => { await fixture?.close(); }, FIXTURE_CLEANUP_MS);

async function loadSeat(host: Host, dir: string, seat: string, session: string) {
  host.send({ op: "load", seat, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: dir, session_id: session });
  const ready = await host.next("ready", e => e.type === "ready" || e.type === "error");
  expect(ready.type).toBe("ready");
}

function prompt(seat: string, session: string, request: string, turn: string, text: string) {
  return { op: "prompt", seat, session_id: session, request_id: request, rpc_id: `rpc-${request}`, attempt_id: `att-${request}`, turn_id: turn, text };
}

const isAnswer = (e: Ev) => (e.type === "text" || e.type === "turn_end" || e.type === "error") && !e.rpc_id;
const settled = (request: string) => (e: Ev) =>
  e.type === "receipt" && e.request_id === request && ["completed", "cancelled", "failed"].includes(e.receipt?.state);

// The host's contract (omp_agent.go publishOMPAnswer): an answer names its turn.
function expectEveryAnswerNamesItsTurn(host: Host) {
  const anonymous = host.events.filter(e => isAnswer(e) && !(typeof e.turn_id === "string" && e.turn_id.trim()));
  expect(anonymous.map(e => ({ type: e.type, text: e.text }))).toEqual([]);
}

async function abortThenSend(batched: boolean) {
  const { dir } = fixture;
  await loadSeat(host, dir, "owner", "s1");

  host.send(prompt("owner", "s1", "r1", "T1", "HOLD the first owner message"));
  const started = await host.next("r1 started", e => e.type === "receipt" && e.request_id === "r1" && e.receipt?.state === "started");
  expect(started.receipt.turn_ids).toEqual(["T1"]);

  const second = prompt("owner", "s1", "r2", "T2", "second owner message");
  if (batched) host.send({ op: "abort", seat: "owner" }, second);
  else host.send({ op: "abort", seat: "owner" });

  // The aborted turn ends, as itself, cancelled.
  const aborted = await host.next("T1 turn_end", e => e.type === "turn_end" && !e.rpc_id);
  expect(aborted.turn_id).toBe("T1");
  expect(aborted.error).toBeUndefined();
  // How it ended is typed; its text is the reply, and an aborted turn has none.
  expect(aborted.stop_reason).toBe("aborted");
  expect(aborted.text).toBeUndefined();
  const digest = await host.next("T1 digest", e => e.type === "turn" && e.turn_id === "T1");
  expect(digest.stop).toBe("abort");
  const cancelled = await host.next("r1 settled", settled("r1"));
  expect(cancelled.receipt).toMatchObject({ state: "cancelled", cause: "cancellation", turn_ids: ["T1"] });

  if (!batched) host.send(second);

  // The next send gets its own turn and its reply.
  const r2 = await host.next("r2 settled", settled("r2"));
  expect(r2.receipt.state).toBe("completed");
  expect(r2.receipt.turn_ids).toEqual(["T2"]);
  const end = await host.next("T2 turn_end", e => e.type === "turn_end" && e.turn_id === "T2");
  expect(end.error).toBeUndefined();
  const reply = host.events.filter(e => e.type === "text" && e.turn_id === "T2").map(e => e.text).join("");
  expect(reply).toBe("reply: second owner message");
  // Nothing of the aborted turn answered after it ended, and no turn but
  // these two was told to the host.
  const turns = new Set(host.events.filter(isAnswer).map(e => e.turn_id));
  expect([...turns].sort()).toEqual(["T1", "T2"]);
  expect(host.events.filter(e => e.type === "turn_end" && e.turn_id === "T1").length).toBe(1);
  expectEveryAnswerNamesItsTurn(host);
  host.close();
}

test("owner abort then send: the aborted turn ends cancelled as itself, the next send answers as its own turn", async () => {
  await abortThenSend(false);
});

test("abort and send in one write: the send queued behind the unwinding turn answers under its own turn", async () => {
  await abortThenSend(true);
});

test("abort with no turn open is acknowledged, not answered as an anonymous turn_end", async () => {
  const { dir } = fixture;
  await loadSeat(host, dir, "idle", "s1");
  host.send({ op: "abort", seat: "idle" });
  const ack = await host.next("abort ack", e => e.type === "aborted" || e.type === "turn_end" || e.type === "error");
  expect(ack.type).toBe("aborted");
  expect(ack.turn_id).toBeUndefined();
  // The seat still takes a turn afterwards.
  host.send(prompt("idle", "s1", "r1", "T1", "after an idle abort"));
  const r1 = await host.next("r1 settled", settled("r1"));
  expect(r1.receipt.state).toBe("completed");
  expectEveryAnswerNamesItsTurn(host);
  host.close();
});

test("an unidentified prompt aborted mid-turn ends under the turn_id the host gave it", async () => {
  const { dir } = fixture;
  await loadSeat(host, dir, "legacy", "s1");
  host.send({ op: "prompt", seat: "legacy", session_id: "s1", turn_id: "L1", text: "HOLD legacy" });
  await host.next("L1 accepted", e => e.type === "accepted" && e.turn_id === "L1");
  host.send({ op: "abort", seat: "legacy" });
  const end = await host.next("L1 turn_end", e => e.type === "turn_end");
  expect(end.turn_id).toBe("L1");
  host.send({ op: "prompt", seat: "legacy", session_id: "s1", turn_id: "L2", text: "legacy two" });
  const next = await host.next("L2 turn_end", e => e.type === "turn_end" && e.turn_id === "L2");
  expect(next.error).toBeUndefined();
  expect(host.events.filter(e => e.type === "text" && e.turn_id === "L2").map(e => e.text).join("")).toBe("reply: legacy two");
  expectEveryAnswerNamesItsTurn(host);
  host.close();
});

// A reload is a load of a seat the connection already loaded: the host's
// token refresh sends one with no request id and reads no reply, because its
// pump is already running. When it fails, the failure belongs to no turn and
// to no waiter. It used to go out as an `error` with no turn_id, which the
// pump refused as an identity-less answer and published as a failed turn,
// ending whichever wait was open (the same defect as the abort's turn_end).

test("a failed reload during a turn is a seat_error, and the turn is not ended by it", async () => {
  const { dir } = fixture;
  await loadSeat(host, dir, "reloaded", "s1");
  host.send(prompt("reloaded", "s1", "r1", "T1", "HOLD while the token reloads"));
  await host.next("r1 started", e => e.type === "receipt" && e.request_id === "r1" && e.receipt?.state === "started");

  // The reload carries no token, so the sidecar refuses it.
  host.send({ op: "load", seat: "reloaded", session_id: "s1", provider: "anthropic", model: "claude-sonnet-5" });
  const refused = await host.next("seat_error", e => e.type === "seat_error", 3000);
  expect(refused.text).toBe("load without an access token");
  expect(refused.turn_id).toBeUndefined();

  // The turn it interrupted is still running, and ends only as itself.
  host.send({ op: "abort", seat: "reloaded" });
  const end = await host.next("T1 turn_end", e => e.type === "turn_end" && !e.rpc_id);
  expect(end.turn_id).toBe("T1");
  expect(host.events.filter(e => e.type === "error")).toEqual([]);
  expectEveryAnswerNamesItsTurn(host);
  host.close();
});

test("a reload naming another session is a seat_error with its reason, not an anonymous error", async () => {
  const { dir } = fixture;
  await loadSeat(host, dir, "reloaded", "s1");
  host.send({ op: "load", seat: "reloaded", session_id: "other", provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: dir });
  const refused = await host.next("seat_error", e => e.type === "seat_error", 3000);
  expect(refused.reason).toBe("seat_identity_mismatch");
  // The seat is untouched: it takes a turn under its own session.
  host.send(prompt("reloaded", "s1", "r1", "T1", "after a refused reload"));
  const r1 = await host.next("r1 settled", settled("r1"));
  expect(r1.receipt.state).toBe("completed");
  expect(host.events.filter(e => e.type === "error")).toEqual([]);
  expectEveryAnswerNamesItsTurn(host);
  host.close();
});

test("the first load on a connection is still answered as the load's own reply", async () => {
  const { dir } = fixture;
  host.send({ op: "load", seat: "fresh", session_id: "s1", provider: "anthropic", model: "claude-sonnet-5", cwd: dir });
  const reply = await host.next("load reply", e => e.type === "ready" || e.type === "error" || e.type === "seat_error");
  expect(reply.type).toBe("error");
  expect(reply.text).toBe("load without an access token");
  host.close();
});


test("socket abort of an executing Bash tool kills its sleep child and settles the turn (🎯T235)", async () => {
  const { dir } = fixture;
  await loadSeat(host, dir, "bash-seat", "bash-session");
  const marker = `${dir}/sleep.pid`;
  host.send(prompt("bash-seat", "bash-session", "bash-r1", "bash-T1", `BASH_CANCEL ${marker}`));
  await host.next("Bash tool started", e => e.type === "tool_start", 3000);
  const { existsSync, readFileSync } = await import("node:fs");
  for (let i = 0; i < 100 && !existsSync(marker); i++) await Bun.sleep(10);
  expect(existsSync(marker)).toBe(true);
  const pid = Number(readFileSync(marker, "utf8").trim());
  const started = Date.now();
  try {
    await Bun.sleep(1000);
    host.send({ op: "abort", seat: "bash-seat" });
    const end = await host.next("cancelled turn", e => e.type === "turn_end" && e.turn_id === "bash-T1", 1800);
    expect(end.stop_reason).toBe("aborted");
    expect(Date.now() - started).toBeLessThan(3000);
    for (let i = 0; i < 50; i++) {
      try { process.kill(pid, 0); } catch { break; }
      await Bun.sleep(20);
    }
    expect(() => process.kill(pid, 0)).toThrow();
  } finally {
    try { process.kill(pid, "SIGKILL"); } catch {}
  }
});
