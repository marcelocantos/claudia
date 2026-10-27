// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { createMockModel, registerMockApi, type MockResponse } from "@oh-my-pi/pi-ai/providers/mock";
import type { AssistantMessage, Context } from "@oh-my-pi/pi-ai";
import { createSeatAgent, hollowLengthStop, type SeatEvent } from "./seat.ts";
import { SeatStore } from "./store.ts";
import { History } from "./maintain.ts";

registerMockApi();
const roots: string[] = [];
afterEach(() => { for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true }); });
function fixture(handler: (context: Context) => MockResponse) {
  const root = mkdtempSync(join(tmpdir(), "claudia-hollow-"));
  roots.push(root);
  const storage = new SeatStore(root, "worker", "session");
  const events: SeatEvent[] = [];
  const model = createMockModel({ handler });
  const agent = createSeatAgent({ provider: "mock", model: "mock", token: "test", cwd: dirname(storage.path), modelOverride: model,
    store: storage, emit: e => events.push(e), callTool: async () => "" });
  return { agent, model, events };
}
const meta = { request_id: "A", session_id: "session", attempt_id: "attempt-A", turn_id: "turn-A" };

test("a hollow length stop is retried inside the turn instead of ending it", async () => {
  const f = fixture(() => f.model.calls.length === 1 ? { stopReason: "length", content: [] } : { content: ["done"] });
  await f.agent.prompt("work", meta);
  expect(f.model.calls.length).toBe(2);
  expect(f.events.filter(e => e.type === "text").map(e => e.text)).toEqual(["done"]);
  expect(f.agent.receipt("A").state).toBe("completed");
});

test("a provider that only returns hollow stops ends the turn when there is nothing to fold", async () => {
  const f = fixture(() => ({ stopReason: "length", content: [] }));
  await f.agent.prompt("work", meta);
  expect(f.model.calls.length).toBe(2);
});

test("a second hollow stop in a row folds the conversation before asking again", async () => {
  const root = mkdtempSync(join(tmpdir(), "claudia-hollow-"));
  roots.push(root);
  const storage = new SeatStore(root, "worker", "session");
  const history = new History(storage);
  const large = "the quick brown fox jumps over the lazy dog. ".repeat(1200);
  const usage = { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
  for (let i = 0; i < 8; i++) history.sync([
    { role: "user", content: `${i}: ${large}`, timestamp: i * 2 },
    { role: "assistant", content: [{ type: "text", text: "ok" }], api: "mock", provider: "mock", model: "mock", timestamp: i * 2 + 1, usage, stopReason: "stop" } as AssistantMessage,
  ]);
  history.persist();
  const events: SeatEvent[] = [];
  const model = createMockModel({ contextWindow: 400_000,
    handler: () => model.calls.length <= 2 ? { stopReason: "length", content: [] } : { content: ["done"] } });
  let compactions = 0;
  const agent = createSeatAgent({ provider: "mock", model: "mock", token: "t", cwd: dirname(storage.path), store: storage,
    modelOverride: model, emit: e => events.push(e), callTool: async () => "", maintenance: {
      nativeEligible: () => false,
      compactImpl: (async (prep: any) => {
        compactions++;
        return { summary: "earlier work", firstKeptEntryId: prep.firstKeptEntryId, tokensBefore: prep.tokensBefore };
      }) as never,
    } });
  await agent.prompt("work", meta);
  expect(compactions).toBe(1);
  expect(model.calls.length).toBe(3);
  expect(events.filter(e => e.type === "text").map(e => e.text)).toEqual(["done"]);
});

test("a length stop that produced output or usage is a real length stop", () => {
  const usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0 };
  expect(hollowLengthStop({ role: "assistant", stopReason: "length", content: [], usage })).toBe(true);
  expect(hollowLengthStop({ role: "assistant", stopReason: "length", content: [{ type: "text", text: "partial" }], usage })).toBe(false);
  expect(hollowLengthStop({ role: "assistant", stopReason: "length", content: [], usage: { ...usage, output: 9, totalTokens: 9 } })).toBe(false);
  expect(hollowLengthStop({ role: "assistant", stopReason: "stop", content: [], usage })).toBe(false);
});
