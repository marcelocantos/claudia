// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { createMockModel, registerMockApi, type MockResponse } from "@oh-my-pi/pi-ai/providers/mock";
import type { AssistantMessage, Model } from "@oh-my-pi/pi-ai";
import { createSeatAgent, type SeatEvent } from "./seat.ts";
import { SeatStore } from "./store.ts";
import { History } from "./maintain.ts";
import { UsageJournal, usageFactType } from "./usage.ts";
import { Spool } from "./spool.ts";
import { readFileSync } from "node:fs";

registerMockApi();
const roots: string[] = [];
afterEach(() => { for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true }); });
function storage() {
  const root = mkdtempSync(join(tmpdir(), "claudia-usage-")); roots.push(root);
  return new SeatStore(root, "worker", "session");
}
const meta = (id: string) => ({ request_id: id, attempt_id: `attempt-${id}`, session_id: "session", turn_id: `turn-${id}` });
function fixture(store: SeatStore, handler: () => MockResponse = () => ({ content: ["ok"], usage: { input: 21, output: 4, cacheRead: 7 } })) {
  const events: SeatEvent[] = [];
  const model = createMockModel({ handler });
  const agent = createSeatAgent({ provider: "mock", model: "mock", token: "test", cwd: dirname(store.path), modelOverride: model, store, emit: e => events.push(e), callTool: async () => "" });
  return { agent, model, events };
}

test("real engine calls persist distinct facts with plural input references, not snapshots", async () => {
  const store = storage();
  const f = fixture(store, () => {
    if (f.model.calls.length === 1) { f.agent.steer("B", meta("B")); void f.agent.prompt("C", meta("C")); }
    return { content: ["ok"], usage: { input: 21, output: 4, cacheRead: 7 } };
  });
  await f.agent.prompt("A", meta("A"));
  const facts = f.agent.usage().facts;
  expect(facts.length).toBe(f.model.calls.length * 2);
  const finals = facts.filter(v => v.state === "completed");
  expect(new Set(finals.map(v => v.call_id)).size).toBe(f.model.calls.length);
  expect(finals[0].requests).toEqual([{ request_id: "A", attempt_id: "attempt-A" }]);
  expect(finals.at(-1)!.requests.map(r => r.request_id).sort()).toEqual(["A", "B", "C"]);
  expect(finals.every(v => v.turn_id === "turn-A")).toBe(true);
  expect(finals[0].counters).toEqual({ input_tokens: 21, output_tokens: 4, cache_read_input_tokens: 7 });
  expect(finals[0].api_equivalent_cost_usd).toBeUndefined();
  expect(f.events.filter(e => e.type === "usage").map(e => e.usage_fact)).toEqual(facts);
  const restored = fixture(store);
  expect(restored.agent.usage().facts).toEqual(facts);
  expect(restored.model.calls).toHaveLength(0);
  expect(JSON.stringify(restored.agent.snapshot())).not.toContain(usageFactType);
  expect(restored.agent.usage(0, 1)).toMatchObject({ facts: [facts[0]], next: 1, more: true });
  expect(restored.agent.usage(facts.length, 1)).toMatchObject({ facts: [], next: facts.length, more: false });
  expect(() => restored.agent.usage(0, 129)).toThrow();
});

test("synthetic error counters remain unknown and a retry is a new invocation", async () => {
  const f = fixture(storage(), () => ({ stopReason: "error", errorMessage: "fixture failure" }));
  await f.agent.prompt("A", meta("A"));
  await f.agent.prompt("B", meta("B"));
  const finals = f.agent.usage().facts.filter(v => v.state !== "started");
  expect(finals).toHaveLength(2);
  expect(finals.every(v => v.state === "failed" && Object.keys(v.counters).length === 0)).toBe(true);
  expect(finals[0].call_id).not.toBe(finals[1].call_id);
});

test("auth retry retains failed usage facts even though the refused message is removed", async () => {
  const store = storage();
  const model = createMockModel({ handler: () => model.calls.length === 1
    ? { stopReason: "error", errorMessage: "401 OAuth access token has expired", usage: { input: 5 } }
    : { content: ["ok"], usage: { input: 9, output: 3 } } });
  const agent = createSeatAgent({ provider: "mock", model: "mock", token: "failed", cwd: dirname(store.path), modelOverride: model, store,
    emit: e => { if (e.type === "auth_retry") agent.authRetry({ ...e, token: "fixture-new", expires_at: Date.now() + 60_000 }); }, callTool: async () => "" });
  await agent.prompt("A", meta("A"));
  const facts = agent.usage().facts.filter(v => v.state !== "started");
  expect(facts.map(v => v.state)).toEqual(["failed", "completed"]);
  expect(facts.map(v => v.counters.input_tokens)).toEqual([5, 9]);
  expect(facts.map(v => v.requests)).toEqual([[{ request_id: "A", attempt_id: "attempt-A" }], [{ request_id: "A", attempt_id: "attempt-A" }]]);
  expect(facts[0].call_id).not.toBe(facts[1].call_id);
  expect(fixture(store).agent.usage().facts.filter(v => v.state !== "started")).toEqual(facts);
});

test("an interrupted invocation keeps explicit cancellation and unknown counters", async () => {
  const f = fixture(storage(), () => { f.agent.abort(); return { content: ["interrupted"] }; });
  await f.agent.prompt("A", meta("A"));
  expect(f.agent.usage().facts.at(-1)).toMatchObject({ state: "cancelled", counters: {} });
});

test("dated spool mirrors typed facts without flattening identity or availability", async () => {
  const store = storage(); const f = fixture(store);
  await f.agent.prompt("A", meta("A"));
  const spool = new Spool(join(dirname(store.path), "spool"));
  const fact = f.agent.usage().facts[1];
  const path = spool.append({ ts: new Date().toISOString(), seat: "worker", type: "usage", usage_fact: fact });
  const record = JSON.parse(readFileSync(path, "utf8").trim());
  expect(record.usage_fact).toEqual(fact);
  expect(record.costUSD).toBeUndefined();
  expect(record.snapshot).toBeUndefined();
  expect(record.usage_fact.counters.cache_creation_input_tokens).toBeUndefined();
});

test("invalid adapter evidence poisons accounting without dispatching twice", async () => {
  for (const input of [-1, Number.NaN, Number.POSITIVE_INFINITY]) {
    const f = fixture(storage(), () => ({ content: ["ok"], usage: { input } }));
    await f.agent.prompt("A", meta("A"));
    expect(f.model.calls).toHaveLength(1);
    expect(f.events.some(e => e.type === "usage_error")).toBe(true);
    expect(() => f.agent.usage()).toThrow(/accounting unavailable/);
  }
});

test("accounting append failure is observable and leaves provider policy unchanged", async () => {
  const store = storage();
  const append = store.append.bind(store);
  store.append = entries => {
    if (entries.some(e => e.type === "custom" && e.customType === usageFactType)) throw new Error("fixture accounting disk failure");
    append(entries);
  };
  const f = fixture(store);
  // The same store also persists the turn. Its existing failure remains an
  // error; the accounting observer itself neither blocks nor retries a call.
  await expect(f.agent.prompt("legacy input")).rejects.toThrow(/disk failure/);
  expect(f.model.calls).toHaveLength(1);
  expect(f.events.some(e => e.type === "usage_error" && e.text?.includes("disk failure"))).toBe(true);
  expect(() => f.agent.usage()).toThrow(/accounting unavailable/);
});

test("local compaction records its own engine calls and survives a reload", async () => {
  const store = storage();
  let summaryCalls = 0;
  const model = createMockModel({ contextWindow: 100_000, handler: () => ({ content: ["ok"], usage: { output: 1 } }) });
  const events: SeatEvent[] = [];
  const agent = createSeatAgent({ provider: "mock", model: "mock", token: "test", cwd: dirname(store.path), modelOverride: model, store, emit: e => events.push(e), callTool: async () => "", maintenance: {
    nativeEligible: () => false,
    summaryOptions: { completeImpl: (async (m: Model) => {
      summaryCalls++;
      return { role: "assistant", content: [{ type: "text", text: "Earlier work summary" }], api: m.api, provider: m.provider, model: m.id, usage: { input: 100, output: 10, cacheRead: 0, cacheWrite: 0, totalTokens: 110, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } }, stopReason: "stop", timestamp: Date.now() } as AssistantMessage;
    }) as never },
  } });
  for (let i = 0; i < 9; i++) await agent.prompt(`${i}: ${"the quick brown fox jumps over the lazy dog. ".repeat(1200)}`, meta(String(i)));
  expect(summaryCalls).toBeGreaterThan(0);
  const facts = agent.usage().facts;
  const summaries = facts.filter(v => v.kind === "compaction" && v.state === "completed");
  expect(summaries).toHaveLength(summaryCalls);
  expect(summaries.every(v => v.counters.input_tokens === 100 && v.counters.output_tokens === 10)).toBe(true);
  expect(events.filter(e => e.type === "usage_error")).toEqual([]);
  expect(fixture(store).agent.usage().facts).toEqual(facts);
});

test("unfinished facts are replayable uncertainty and corrupt identity is refused", () => {
  const store = storage(); const h = new History(store);
  const j = new UsageJournal(h);
  const start = j.begin("model", createMockModel(), { requests: [] });
  const restored = new History(store); restored.restore();
  expect(new UsageJournal(restored).page().facts).toEqual([start]);
  const entry = restored.entries.find(e => e.type === "custom")! as any;
  entry.data.session_id = "wrong";
  expect(() => new UsageJournal(restored)).toThrow(/identity/);
});
