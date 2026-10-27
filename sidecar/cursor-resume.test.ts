// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createMockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";
import type { SessionEntry } from "@oh-my-pi/pi-agent-core";
import { createSeatAgent, INTERRUPTED_TOOL_RESULT } from "./seat.ts";
import { SeatStore } from "./store.ts";

registerMockApi();
const usage = { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
function seeded(root: string, provider: string, stopReason: string) {
  const store = new SeatStore(root, "seat", `${provider}-${stopReason}`);
  const entries = [
    { type: "message", id: "u1", parentId: null, timestamp: new Date().toISOString(),
      message: { role: "user", content: [{ type: "text", text: "check the tree" }], timestamp: Date.now() } },
    { type: "message", id: "a1", parentId: "u1", timestamp: new Date().toISOString(),
      message: { role: "assistant", api: "mock", provider, model: "m", usage, stopReason, timestamp: Date.now(),
        content: [{ type: "toolCall", id: "call-1", name: "bash", arguments: { command: "git status" } },
          { type: "text", text: "The tree is clean." }] } },
  ] as unknown as SessionEntry[];
  store.rewrite(entries);
  return store;
}
function restore(store: SeatStore, root: string) {
  const model = createMockModel({ handler: () => ({ content: ["unexpected"] }) });
  const agent = createSeatAgent({ provider: "mock", model: "mock", token: "test", cwd: root, modelOverride: model,
    store, emit: () => {}, callTool: async () => "" });
  return { agent, model };
}

test("a restored Cursor reply that ended with stop has run its tools: not mid-turn, nothing paired as interrupted", () => {
  const root = mkdtempSync(join(tmpdir(), "cursor-resume-"));
  try {
    const store = seeded(root, "cursor", "stop");
    const { agent, model } = restore(store, root);
    expect(agent.transcriptEndsMidTurn).toBe(false);
    expect(readFileSync(store.path, "utf8")).not.toContain(INTERRUPTED_TOOL_RESULT);
    expect(model.calls).toHaveLength(0);
  } finally { rmSync(root, { recursive: true, force: true }); }
});

test("another provider's unfinished tool call is still paired as interrupted and reads as mid-turn", () => {
  const root = mkdtempSync(join(tmpdir(), "cursor-resume-"));
  try {
    const store = seeded(root, "openai-codex", "toolUse");
    const { agent } = restore(store, root);
    expect(agent.transcriptEndsMidTurn).toBe(true);
    expect(readFileSync(store.path, "utf8")).toContain(INTERRUPTED_TOOL_RESULT);
  } finally { rmSync(root, { recursive: true, force: true }); }
});
