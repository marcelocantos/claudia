// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// 🎯T206: every accepted message is in the seat store before the next model call.

import { expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { SessionEntry } from "@oh-my-pi/pi-agent-core";
import { createMockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";
import { createSeatAgent } from "./seat.ts";
import { SeatStore } from "./store.ts";

registerMockApi();

function messageRoles(store: SeatStore): string[] {
  const entries = (store.load() ?? []) as SessionEntry[];
  return entries.filter(e => e.type === "message").map(e => (e as { message: { role: string } }).message.role);
}

test("a multi-tool turn persists assistant and tool results before the second model call", async () => {
  const root = mkdtempSync(join(tmpdir(), "t206-"));
  writeFileSync(join(root, "package.json"), "{}");
  const storage = new SeatStore(root, "worker", "session");
  let n = 0;
  const model = createMockModel({
    contextWindow: 100_000,
    handler: () => {
      n++;
      if (n === 2) {
        expect(messageRoles(storage)).toEqual(["user", "assistant", "toolResult"]);
      }
      if (n === 1) {
        return {
          content: [{ type: "toolCall", id: "call-1", name: "Read", arguments: { path: "package.json" } }],
          usage: { input: 10, output: 1 },
        };
      }
      return { content: ["done"], usage: { input: 10, output: 1 } };
    },
  });
  const agent = createSeatAgent({
    provider: "mock",
    model: "mock-model",
    token: "t",
    cwd: root,
    emit: () => {},
    callTool: async () => "",
    modelOverride: model as never,
    store: storage,
  });
  await agent.prompt("go");
  expect(n).toBe(2);
  rmSync(root, { recursive: true, force: true });
});

test("required resume names the store path and not only bookkeeping facts", () => {
  const root = mkdtempSync(join(tmpdir(), "seat-resume-"));
  try {
    const store = new SeatStore(root, "seat", "empty");
    store.rewrite([{ type: "custom", id: "u", customType: "claudia_usage_v1", data: {}, parentId: null, timestamp: new Date().toISOString() }] as SessionEntry[]);
    const model = createMockModel({ handler: () => ({ content: ["unexpected"] }) });
    expect(() => createSeatAgent({
      provider: "mock", model: "mock", token: "test", cwd: root, modelOverride: model,
      store, requireResume: true, emit: () => {}, callTool: async () => "",
    })).toThrow(store.path);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
