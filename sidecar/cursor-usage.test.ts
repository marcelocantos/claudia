// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { afterEach, expect, spyOn, test } from "bun:test";
import http2 from "node:http2";
import { EventEmitter } from "node:events";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import { History } from "./maintain.ts";
import { HistoricalUsageReader } from "./usage-reader.ts";
import { SeatStore } from "./store.ts";
import { measuredStream, UsageJournal } from "./usage.ts";

const raw = JSON.parse(readFileSync(join(import.meta.dir, "testdata/provider-usage/cursor-frames.json"), "utf8"));
const context = { messages: [{ role: "user" as const, content: "Fixture question", timestamp: 1 }] };
const refs = () => ({ turn_id: "turn-A", requests: [{ request_id: "logical-A", attempt_id: "delivery-A" }] });
const roots: string[] = [];
let restore: (() => void) | undefined;
afterEach(() => { restore?.(); restore = undefined; for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true }); });

class Request extends EventEmitter {
  closed = false;
  writes = 0;
  constructor(readonly headers: any, readonly respond: (request: Request) => void) { super(); }
  write(bytes: Uint8Array) {
    expect(bytes[0]).toBe(0);
    if (++this.writes === 1) queueMicrotask(() => this.respond(this));
    return true;
  }
  close() { if (!this.closed) { this.closed = true; this.emit("end"); } }
  frame(name: string) {
    const payload = Buffer.from(raw[name], "hex"); const frame = Buffer.alloc(5 + payload.length);
    frame.writeUInt32BE(payload.length, 1); payload.copy(frame, 5); this.emit("data", frame);
  }
  fail(message: string) { this.emit("error", new Error(message)); }
  finish() { this.frame("turnEnded"); this.emit("end"); }
}
function transport(respond: (request: Request, index: number) => void) {
  const requests: Request[] = [];
  const mock = spyOn(http2, "connect").mockImplementation((url: any) => {
    expect(String(url)).toBe("https://fixture.invalid");
    const session = new EventEmitter() as any;
    session.request = (headers: any) => {
      expect(headers[":path"]).toBe("/agent.v1.AgentService/Run");
      const request = new Request(headers, r => respond(r, requests.indexOf(r))); requests.push(request); return request;
    };
    session.close = () => {};
    return session;
  });
  restore = () => mock.mockRestore(); return requests;
}
function fixture(id = "claude-4-sonnet") {
  const root = mkdtempSync(join(tmpdir(), "claudia-cursor-usage-")); roots.push(root);
  const store = new SeatStore(root, "seat", "generation"); const history = new History(store); history.restore();
  const errors: string[] = []; const journal = new UsageJournal(history, () => {}, error => errors.push(error));
  const model = { ...getBundledModel("cursor", id)!, baseUrl: "https://fixture.invalid" };
  return { model, store, journal, errors, options: { apiKey: "fixture", sessionId: crypto.randomUUID(), streamFirstEventTimeoutMs: 1000, streamIdleTimeoutMs: 1000 } };
}
async function run(f: ReturnType<typeof fixture>, options = {}) {
  return measuredStream(() => f.journal, refs)(f.model, context, { ...f.options, ...options }).result();
}

test("Cursor H2 raw deltas and checkpoint stay separate from billed counters and survive restart", async () => {
  const requests = transport(r => { r.frame("context23"); r.frame("delta3"); r.frame("delta4"); r.finish(); });
  const f = fixture(); const result = await run(f);
  expect(result.stopReason).toBe("stop"); expect(requests).toHaveLength(1); expect(f.errors).toEqual([]);
  const facts = f.journal.page().facts; expect(facts).toHaveLength(2);
  expect(facts[1]).toMatchObject({ state: "completed", coverage: "provider_attempt", counters: {}, context_tokens: 23, protocol_token_delta_total: 7, requests: refs().requests });
  expect(facts[1].provider_request_id).toBe(requests[0].headers["x-request-id"]);
  expect(facts[1].provider_attempt_id).toBe(facts[1].call_id);
  expect(facts[1].provider_request_id).not.toBe(facts[1].call_id);
  const restored = new History(f.store); restored.restore();
  expect(new UsageJournal(restored).page()).toEqual(f.journal.page());
  const reader = new HistoricalUsageReader();
  const first = await reader.page(f.store, 0, 1);
  const final = await reader.page(f.store, first.next, 1);
  expect([...first.facts, ...final.facts]).toEqual(facts);
  expect(final.facts[0]).toMatchObject({ counters: {}, context_tokens: 23, protocol_token_delta_total: 7 });
  expect(reader.stats().scans).toBe(1);
  expect(restored.rebuild()).toEqual([]);
});

test("Cursor omitted protobuf scalars remain unknown while explicit zero survives", async () => {
  transport(r => { r.frame("contextAbsent"); r.frame("deltaAbsent"); r.finish(); });
  const absent = fixture(); await run(absent);
  expect(absent.journal.page().facts[1].context_tokens).toBeUndefined();
  expect(absent.journal.page().facts[1].protocol_token_delta_total).toBeUndefined();
  restore?.();
  transport(r => { r.frame("contextZero"); r.frame("deltaZero"); r.finish(); });
  const zero = fixture(); await run(zero);
  expect(zero.journal.page().facts[1]).toMatchObject({ counters: {}, context_tokens: 0, protocol_token_delta_total: 0 });
});

test("Cursor partial failed request retains protocol observations without claiming consumption", async () => {
  transport(r => { r.frame("delta3"); r.fail("fixture disconnected"); });
  const f = fixture(); expect((await run(f)).stopReason).toBe("error");
  expect(f.journal.page().facts[1]).toMatchObject({ state: "failed", counters: {}, protocol_token_delta_total: 3 });
});

test("Cursor cancellation keeps deltas already received", async () => {
  const abort = new AbortController(); transport(r => { r.frame("delta4"); abort.abort(); });
  const f = fixture(); expect((await run(f, { signal: abort.signal })).stopReason).toBe("aborted");
  expect(f.journal.page().facts[1]).toMatchObject({ state: "cancelled", counters: {}, protocol_token_delta_total: 4 });
});

test("Cursor equal wire deltas are observations, not deduplicable generated-token identities", async () => {
  transport(r => { r.frame("delta3"); r.frame("delta3"); r.finish(); });
  const f = fixture(); await run(f);
  expect(f.journal.page().facts[1]).toMatchObject({ counters: {}, protocol_token_delta_total: 6 });
});

test("Cursor disconnected invocation followed by reconnect keeps separate attempt and invocation identities", async () => {
  const requests = transport((r, index) => { r.frame("delta3"); if (index === 0) r.fail("fixture disconnected"); else r.finish(); });
  const f = fixture(); await run(f); await run(f);
  expect(requests).toHaveLength(2);
  const finals = f.journal.page().facts.filter(f => f.state !== "started");
  expect(finals.map(f => f.state)).toEqual(["failed", "completed"]);
  expect(new Set(finals.map(f => f.call_id)).size).toBe(2);
  expect(new Set(finals.map(f => f.invocation_id)).size).toBe(2);
  expect(finals.map(f => f.protocol_token_delta_total)).toEqual([3, 3]);
});

test("Cursor discovered model retry retains failed Run without conflating invocation or delivery", async () => {
  const requests = transport((r, index) => {
    if (index === 0) { r.emit("trailers", { "grpc-status": "5", "grpc-message": "model not found" }); r.emit("end"); }
    else { r.frame("delta4"); r.finish(); }
  });
  const f = fixture("gpt-5.5");
  expect((await run(f, { reasoning: "high" })).stopReason).toBe("stop"); expect(requests).toHaveLength(2);
  const finals = f.journal.page().facts.filter(f => f.state !== "started");
  expect(finals.map(f => f.state)).toEqual(["failed", "completed"]);
  expect(finals[0].protocol_token_delta_total).toBeUndefined();
  expect(finals[1].protocol_token_delta_total).toBe(4);
  expect(new Set(finals.map(f => f.invocation_id)).size).toBe(1);
  expect(new Set(finals.map(f => f.provider_attempt_id)).size).toBe(2);
  expect(new Set(finals.map(f => f.provider_request_id)).size).toBe(2);
  expect(finals.every(f => f.requests[0].attempt_id === "delivery-A")).toBe(true);
});

test("Cursor local connection setup failure does not invent a Run dispatch", async () => {
  const mock = spyOn(http2, "connect").mockImplementation(() => { throw new Error("fixture setup failed"); });
  restore = () => mock.mockRestore();
  const f = fixture(); expect((await run(f)).stopReason).toBe("error");
  expect(f.journal.page().facts).toEqual([]);
});

test("Cursor invalid raw delta poisons accounting without retrying or interrupting ordinary frames", async () => {
  const requests = transport(r => { r.frame("deltaNegative"); r.frame("delta4"); r.finish(); });
  const f = fixture(); expect((await run(f)).stopReason).toBe("stop");
  expect(requests).toHaveLength(1); expect(f.errors).toHaveLength(1);
  expect(() => f.journal.page()).toThrow("invalid protocol delta total");
});
