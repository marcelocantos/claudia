// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, test } from "bun:test";
import { mkdtempSync, readFileSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { History } from "./maintain.ts";
import { ReceiptJournal, requestFactType } from "./receipts.ts";
import { SeatStore } from "./store.ts";

const dirs: string[] = [];
const meta = (request_id = "request-1") => ({ request_id, session_id: "session-1", attempt_id: `attempt-${request_id}` });
function fixture() {
  const root = mkdtempSync(join(tmpdir(), "claudia-receipts-"));
  dirs.push(root);
  const store = new SeatStore(root, "seat-1", "session-1");
  const history = new History(store);
  return { root, store, history, journal: new ReceiptJournal(history) };
}
function reopen(store: SeatStore) {
  const history = new History(store);
  history.restore();
  return { history, journal: new ReceiptJournal(history) };
}
afterEach(() => {
  for (const dir of dirs.splice(0)) rmSync(dir, { recursive: true, force: true });
});

describe("durable request receipts", () => {
  test("acceptance is on disk before acknowledgement; pending input recovers with its identity", () => {
    const { store, history, journal } = fixture();
    const accepted = journal.accept("same text", meta(), "steer");
    expect(accepted.fresh).toBe(true);
    expect(accepted.receipt).toMatchObject({ ...meta(), seat: "seat-1", state: "accepted", turn_ids: [] });
    expect(accepted.message).toMatchObject({ role: "user", content: "same text", claudiaRequest: { request_id: "request-1", attempt_id: "attempt-request-1" } });
    const before = readFileSync(store.path, "utf8");
    expect(JSON.parse(before.trim().split("\n").at(-1)!).customType).toBe(requestFactType);
    const restored = reopen(store);
    expect(restored.journal.pending()).toEqual([{ message: accepted.message, delivery: "steer", receipt: accepted.receipt }]);
    expect(restored.journal.query("request-1")).toEqual(accepted.receipt);
    expect(readFileSync(store.path, "utf8")).toBe(before); // Construction and queries do not append.
    expect(history.rebuild()).toEqual([]); // Receipt payloads do not become model instructions.
  });

  test("ack retries and escalation do not enqueue or rewrite an existing obligation", () => {
    const { store, journal } = fixture();
    const accepted = journal.accept("payload", meta(), "prompt");
    const before = readFileSync(store.path, "utf8");
    const retry = reopen(store).journal.accept("payload", { ...meta(), attempt_id: "another-attempt" }, "steer");
    expect(retry).toEqual({ ...accepted, fresh: false });
    expect(readFileSync(store.path, "utf8")).toBe(before);
    expect(() => journal.accept("other payload", meta(), "prompt")).toThrow("conflicting payload");
    expect(() => journal.accept("payload", { ...meta(), session_id: "other-session" }, "prompt")).toThrow("identity mismatch");
  });

  test("identical text with different request IDs remains distinct through History", () => {
    const { store, history, journal } = fixture();
    const a = journal.accept("identical", meta("a"), "prompt");
    const b = journal.accept("identical", meta("b"), "steer");
    // Equal timestamps cannot collapse the messages: identity is part of them.
    b.message.timestamp = a.message.timestamp;
    history.persist([a.message, b.message]);
    const restored = reopen(store);
    expect(restored.journal.pending().map(p => p.receipt.request_id)).toEqual(["a", "b"]);
    expect(restored.history.rebuild()).toEqual([a.message, b.message]);
  });

  test("only started requests settle, and terminal receipts survive reopening", () => {
    const { store, journal } = fixture();
    journal.accept("first", meta("a"), "prompt");
    journal.accept("queued", meta("b"), "steer");
    journal.finish(["a", "b"], "completed");
    expect(journal.query("a").state).toBe("accepted");
    journal.start(["a", "a"], "turn-1");
    const before = readFileSync(store.path, "utf8");
    journal.start(["a"], "turn-1");
    expect(readFileSync(store.path, "utf8")).toBe(before);
    journal.start(["a"], "turn-2");
    journal.finish(["a", "b"], "completed");
    const settled = journal.query("a");
    expect(settled).toMatchObject({ state: "completed", turn_ids: ["turn-1", "turn-2"] });
    expect(journal.query("b").state).toBe("accepted");
    journal.finish(["a"], "failed", "must not overwrite terminal state");
    journal.start(["a"], "turn-3");
    expect(journal.query("a")).toEqual(settled);
    expect(reopen(store).journal.query("a")).toEqual(settled);
  });

  test.each(["cancelled", "failed"] as const)("%s settles started inputs without settling the waiting queue", state => {
    const { store, journal } = fixture();
    journal.accept("running", meta("a"), "prompt");
    journal.accept("queued", meta("b"), "steer");
    journal.start(["a"], "turn-1");
    journal.finish(["a", "b"], state, "provider stopped");
    const restored = reopen(store).journal;
    expect(restored.query("a")).toMatchObject({ state, reason: "provider stopped" });
    expect(restored.pending().map(p => p.receipt.request_id)).toEqual(["b"]);
  });

  test("a started request restores as unknown and is never automatically replayed", () => {
    const { store, journal } = fixture();
    journal.accept("may have external effects", meta(), "prompt");
    journal.start(["request-1"], "turn-1");
    const before = readFileSync(store.path, "utf8");
    const restored = reopen(store).journal;
    expect(restored.query("request-1")).toMatchObject({ state: "unknown", reason: "interrupted", turn_ids: ["turn-1"] });
    expect(restored.pending()).toEqual([]);
    expect(restored.accept("may have external effects", meta(), "steer")).toMatchObject({ fresh: false, receipt: { state: "unknown" } });
    expect(() => restored.start(["request-1"], "turn-2")).toThrow("cannot be replayed");
    restored.finish(["request-1"], "completed");
    expect(readFileSync(store.path, "utf8")).toBe(before);
  });

  test("unknown queries are explicit and legacy sessions without stores cannot accept identified requests", () => {
    const { store, journal } = fixture();
    expect(journal.query("unseen")).toMatchObject({ request_id: "unseen", state: "unknown", reason: "not_found", session_id: "session-1" });
    expect(journal.query("unseen").accepted_at).toBeUndefined();
    const legacy = new ReceiptJournal(new History());
    expect(() => legacy.assertHealthy()).not.toThrow();
    expect(legacy.pending()).toEqual([]);
    expect(legacy.query("unseen")).toMatchObject({ state: "unknown", reason: "no_store" });
    expect(() => legacy.accept("payload", meta(), "prompt")).toThrow("require a durable");
    expect(() => legacy.start(["unseen"], "turn")).toThrow("require a durable");
    expect(store.load()).toBeNull();
  });

  test("receipts, pending rows and tagged messages are defensive copies", () => {
    const { journal } = fixture();
    const accepted = journal.accept("original", meta(), "prompt");
    accepted.receipt.turn_ids.push("forged");
    accepted.message.claudiaRequest.request_id = "forged";
    accepted.message.content = "forged";
    const pending = journal.pending();
    pending[0].receipt.state = "completed";
    pending[0].message.content = "forged";
    journal.query("request-1").turn_ids.push("forged");
    expect(journal.pending()[0]).toMatchObject({ message: { content: "original", claudiaRequest: { request_id: "request-1" } }, receipt: { state: "accepted", turn_ids: [] } });
  });

  test("invalid identities and unknown starts fail before any write", () => {
    const { journal, store } = fixture();
    expect(() => journal.accept("payload", { ...meta(), request_id: " " }, "prompt")).toThrow("invalid identified");
    expect(() => journal.accept("payload", { ...meta(), attempt_id: "" }, "prompt")).toThrow("invalid identified");
    expect(() => journal.start(["unseen"], "turn-1")).toThrow("was not accepted");
    expect(store.load()).toBeNull();
    journal.accept("payload", meta(), "prompt");
    const before = readFileSync(store.path, "utf8");
    expect(() => journal.start(["request-1", "unseen"], "turn-1")).toThrow("was not accepted");
    expect(readFileSync(store.path, "utf8")).toBe(before);
  });

  test.each(["accept", "start", "finish"] as const)("a real persistence failure during %s poisons every later dispatch", operation => {
    const { store, journal } = fixture();
    journal.accept("payload", meta(), "prompt");
    if (operation === "finish") journal.start(["request-1"], "turn-1");
    const seatDir = dirname(store.path);
    const saved = `${seatDir}.saved`;
    renameSync(seatDir, saved);
    writeFileSync(seatDir, "a file prevents journal append");
    const fail = () => {
      if (operation === "accept") journal.accept("another", meta("other"), "prompt");
      else if (operation === "start") journal.start(["request-1"], "turn-1");
      else journal.finish(["request-1"], "completed");
    };
    expect(fail).toThrow("persistence failed");
    rmSync(seatDir);
    renameSync(saved, seatDir);
    const before = readFileSync(store.path, "utf8");
    expect(journal.query("request-1")).toMatchObject({ state: "unknown", reason: "persistence_failed" });
    expect(journal.query("other")).toMatchObject({ state: "unknown", reason: "persistence_failed" });
    expect(() => journal.assertHealthy()).toThrow("dispatch refused");
    expect(() => journal.pending()).toThrow("dispatch refused");
    expect(() => journal.accept("payload", meta(), "prompt")).toThrow("dispatch refused");
    expect(() => journal.start([], "turn-2")).toThrow("dispatch refused");
    expect(() => journal.finish([], "failed")).toThrow("dispatch refused");
    expect(readFileSync(store.path, "utf8")).toBe(before);
  });

  test.each([
    ["foreign seat", (f: any) => { f.receipt.seat = "another-seat"; }],
    ["foreign session", (f: any) => { f.receipt.session_id = "another-session"; }],
    ["missing request", (f: any) => { delete f.receipt.request_id; }],
    ["missing attempt", (f: any) => { delete f.receipt.attempt_id; }],
    ["bad timestamp", (f: any) => { f.receipt.accepted_at = "not-a-date"; }],
    ["invalid delivery", (f: any) => { f.delivery = "invented"; }],
    ["missing payload", (f: any) => { delete f.text; }],
    ["acceptance with a turn", (f: any) => { f.receipt.turn_ids = ["turn"]; }],
    ["start without acceptance", (f: any) => { f.receipt.state = "started"; f.receipt.turn_ids = ["turn"]; }],
  ])("rejects %s in a stored fact", (_name, mutate) => {
    const { store, journal } = fixture();
    journal.accept("payload", meta(), "prompt");
    const lines = readFileSync(store.path, "utf8").trim().split("\n").map(line => JSON.parse(line));
    (mutate as (f: any) => void)(lines[1].data);
    const corrupt = lines.map(line => JSON.stringify(line) + "\n").join("");
    writeFileSync(store.path, corrupt);
    expect(() => reopen(store)).toThrow();
    expect(readFileSync(store.path, "utf8")).toBe(corrupt);
  });

  test("rejects a later fact rewriting the immutable payload or regressing a terminal receipt", () => {
    for (const change of ["payload", "terminal"]) {
      const { store, journal } = fixture();
      journal.accept("payload", meta(), "prompt");
      journal.start(["request-1"], "turn-1");
      journal.finish(["request-1"], "completed");
      const lines = readFileSync(store.path, "utf8").trim().split("\n").map(line => JSON.parse(line));
      if (change === "payload") lines[2].data.text = "rewritten";
      else {
        const regression = structuredClone(lines.at(-1));
        regression.id = crypto.randomUUID();
        regression.data.receipt.state = "started";
        lines.push(regression);
      }
      writeFileSync(store.path, lines.map(line => JSON.stringify(line) + "\n").join(""));
      expect(() => reopen(store)).toThrow("corrupt request transition");
    }
  });
});
