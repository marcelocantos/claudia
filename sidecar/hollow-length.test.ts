// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { createMockModel, registerMockApi, type MockResponse } from "@oh-my-pi/pi-ai/providers/mock";
import type { Context } from "@oh-my-pi/pi-ai";
import { createSeatAgent, hollowLengthStop, type SeatEvent } from "./seat.ts";
import { SeatStore } from "./store.ts";

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

test("a provider that only returns hollow stops ends the turn after the bounded retries", async () => {
  const f = fixture(() => ({ stopReason: "length", content: [] }));
  await f.agent.prompt("work", meta);
  expect(f.model.calls.length).toBe(4);
});

test("a length stop that produced output or usage is a real length stop", () => {
  const usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0 };
  expect(hollowLengthStop({ role: "assistant", stopReason: "length", content: [], usage })).toBe(true);
  expect(hollowLengthStop({ role: "assistant", stopReason: "length", content: [{ type: "text", text: "partial" }], usage })).toBe(false);
  expect(hollowLengthStop({ role: "assistant", stopReason: "length", content: [], usage: { ...usage, output: 9, totalTokens: 9 } })).toBe(false);
  expect(hollowLengthStop({ role: "assistant", stopReason: "stop", content: [], usage })).toBe(false);
});
