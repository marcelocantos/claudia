// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// An owner abort, then a new send, through the real sidecar process and its
// socket (jevons J3-cancel-and-send, 2026-10-19). Every answer the host reads
// as part of a turn (text, turn_end, a turn's error) names that turn: the Go
// pump refuses one without turn_id, and the refusal reached the owner as a
// failed turn while the aborted turn's own end never arrived.

import { afterEach, expect, test } from "bun:test";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { connect, type Socket } from "node:net";

type Ev = Record<string, any>;

const dirs: string[] = [];
const procs: ReturnType<typeof Bun.spawn>[] = [];
afterEach(async () => {
  for (const p of procs.splice(0)) { p.kill(); await p.exited; }
  for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true });
});

// A host connection: one socket for the seat's whole life, as the Go pump has.
class Host {
  events: Ev[] = [];
  #buf = "";
  #waiters: { pred: (ev: Ev) => boolean; resolve: (ev: Ev) => void }[] = [];
  constructor(private socket: Socket) {
    socket.on("data", chunk => {
      this.#buf += chunk.toString("utf8");
      let nl: number;
      while ((nl = this.#buf.indexOf("\n")) >= 0) {
        const ev = JSON.parse(this.#buf.slice(0, nl));
        this.#buf = this.#buf.slice(nl + 1);
        this.events.push(ev);
        this.#waiters = this.#waiters.filter(w => (w.pred(ev) ? (w.resolve(ev), false) : true));
      }
    });
  }
  send(...lines: object[]) {
    this.socket.write(lines.map(l => JSON.stringify(l) + "\n").join(""));
  }
  // next resolves with the first event, already seen or still to come, that
  // matches pred.
  next(what: string, pred: (ev: Ev) => boolean, ms = 10_000): Promise<Ev> {
    const seen = this.events.find(pred);
    if (seen) return Promise.resolve(seen);
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`no ${what} within ${ms}ms; saw ${JSON.stringify(this.events.map(e => [e.type, e.turn_id, e.request_id]))}`)), ms);
      this.#waiters.push({ pred, resolve: ev => { clearTimeout(timer); resolve(ev); } });
    });
  }
  close() { this.socket.end(); }
}

async function startSidecar(): Promise<{ dir: string; sock: string }> {
  // Short path: a unix socket path is limited to ~104 bytes on macOS.
  const dir = mkdtempSync(join(tmpdir(), "abrt-"));
  dirs.push(dir);
  const sock = join(dir, "s.sock");
  const proc = Bun.spawn(["bun", join(import.meta.dir, "testdata", "abort-provider.ts"), sock], {
    stdout: "ignore", stderr: "inherit",
    env: { ...process.env, HOME: dir, CLAUDIA_OMP_SPOOL_DIR: join(dir, "spool") },
  });
  procs.push(proc);
  for (let i = 0; i < 200 && !existsSync(sock); i++) await Bun.sleep(25);
  expect(existsSync(join(dir, "mock-api-ready"))).toBe(true);
  return { dir, sock };
}

async function dial(sock: string): Promise<Host> {
  return new Promise((resolve, reject) => {
    const c = connect(sock, () => resolve(new Host(c)));
    c.on("error", reject);
  });
}

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
  const { dir, sock } = await startSidecar();
  const host = await dial(sock);
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
  const { dir, sock } = await startSidecar();
  const host = await dial(sock);
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
  const { dir, sock } = await startSidecar();
  const host = await dial(sock);
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
  const { dir, sock } = await startSidecar();
  const host = await dial(sock);
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
  const { dir, sock } = await startSidecar();
  const host = await dial(sock);
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
  const { dir, sock } = await startSidecar();
  const host = await dial(sock);
  host.send({ op: "load", seat: "fresh", session_id: "s1", provider: "anthropic", model: "claude-sonnet-5", cwd: dir });
  const reply = await host.next("load reply", e => e.type === "ready" || e.type === "error" || e.type === "seat_error");
  expect(reply.type).toBe("error");
  expect(reply.text).toBe("load without an access token");
  host.close();
});
