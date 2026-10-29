// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// 🎯T236: cancel mid-Bash, then a later separate prompt. The cancelled tool
// call must be closed in the history sent on the second request. A stub that
// rejects an unanswered function call must surface as an error event, never
// an empty end_turn.

import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createMockModel, registerMockApi, type MockResponse } from "@oh-my-pi/pi-ai/providers/mock";
import type { Context } from "@oh-my-pi/pi-ai";
import {
  ABORTED_TOOL_RESULT,
  createSeatAgent,
  emptyAnswerAfterUnpairedTool,
  pairUnpairedToolCalls,
  type SeatEvent,
} from "./seat.ts";
import { turnRefusal } from "./turn.ts";

registerMockApi();

const roots: string[] = [];
afterEach(() => {
  for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true });
});

async function waitFor(pred: () => boolean, ms = 4000): Promise<void> {
  const start = Date.now();
  while (!pred()) {
    if (Date.now() - start > ms) throw new Error("timeout waiting for condition");
    await Bun.sleep(10);
  }
}

function unpairedToolCall(messages: Context["messages"] | undefined): boolean {
  let pending = false;
  for (const m of messages ?? []) {
    const content = (m as { content?: unknown }).content;
    if (m.role === "assistant" && Array.isArray(content) && content.some((c: { type?: string }) => c?.type === "toolCall")) {
      pending = true;
    }
    if (m.role === "toolResult") pending = false;
  }
  return pending;
}

test("cancel during Bash then a fresh prompt closes the tool call in the next request (🎯T236)", async () => {
  const cwd = mkdtempSync(join(tmpdir(), "t236-bash-"));
  roots.push(cwd);
  const events: SeatEvent[] = [];
  const second: { context?: Context } = {};
  const mock = createMockModel({
    contextWindow: 100_000,
    handler: (context): MockResponse => {
      if (mock.calls.length === 1) {
        return {
          content: [{
            type: "toolCall",
            id: "cancel-bash",
            name: "Bash",
            arguments: { command: "sleep 60", timeout: 65 },
          }],
          usage: { input: 10, output: 1 },
        };
      }
      second.context = context;
      if (unpairedToolCall(context.messages)) {
        // Live openai-codex shape: empty stop, no error (J3 2026-10-29).
        return { content: [], stopReason: "stop", usage: { input: 1, output: 0 } };
      }
      return { content: ["journey-cancel-ok"], usage: { input: 10, output: 1 } };
    },
  });
  const agent = createSeatAgent({
    provider: "mock",
    model: "mock-model",
    token: "t",
    cwd,
    emit: ev => events.push(ev),
    callTool: async () => "",
    modelOverride: mock as never,
  });

  const first = agent.prompt("hold with Bash");
  await waitFor(() => events.some(e => e.type === "tool_start" && e.name === "Bash"));
  agent.abort();
  await first.catch(() => {});
  await waitFor(() => events.some(e => e.type === "turn" || e.type === "turn_end"));

  await agent.prompt("Reply with exactly: journey-cancel-ok");
  expect(unpairedToolCall(second.context?.messages)).toBe(false);
  const results = (second.context?.messages ?? []).filter(m => m.role === "toolResult") as Array<{
    toolCallId?: string;
    isError?: boolean;
    content?: Array<{ text?: string }>;
  }>;
  const closed = results.find(m => m.toolCallId === "cancel-bash");
  expect(closed?.isError).toBe(true);
  // Engine Bash abort already pairs with its own error result; host pairing
  // is the fallback when the engine leaves the call unanswered.
  const closedText = JSON.stringify(closed?.content ?? "");
  expect(closedText.toLowerCase()).toContain("abort");
  expect(events.filter(e => e.type === "text").map(e => e.text).join("")).toContain("journey-cancel-ok");
  expect(events.filter(e => e.type === "turn_end").at(-1)?.error).toBeUndefined();
}, 15_000);

test("empty stop after an unanswered function call is a refusal, not a silent end_turn (🎯T236)", () => {
  const toolCall = {
    role: "assistant" as const,
    content: [{ type: "toolCall", id: "c1", name: "Bash", arguments: {} }],
    stopReason: "toolUse",
  };
  const empty = {
    role: "assistant" as const,
    content: [{ type: "text", text: "" }],
    stopReason: "stop",
  };
  const refusal = emptyAnswerAfterUnpairedTool([toolCall, empty]);
  expect(refusal).toContain("unanswered function call");
  expect(turnRefusal({ messages: [toolCall, empty] })).toBe("");
  expect(emptyAnswerAfterUnpairedTool([
    toolCall,
    { role: "toolResult", toolCallId: "c1", toolName: "Bash", content: [{ type: "text", text: "ok" }], isError: true, timestamp: 1 },
    empty,
  ])).toBe("");
  expect(emptyAnswerAfterUnpairedTool([{ role: "assistant", content: [{ type: "text", text: "hi" }], stopReason: "stop" }])).toBe("");
  expect(ABORTED_TOOL_RESULT).toContain("aborted by the host");
});

test("pairUnpairedToolCalls writes an error result for a live abort (🎯T236)", () => {
  const messages: Array<{ role: string; content?: unknown; toolCallId?: string; stopReason?: string }> = [{
    role: "assistant",
    stopReason: "toolUse",
    content: [{ type: "toolCall", id: "cancel-bash", name: "Bash", arguments: {} }],
  }];
  const added: Array<{ isError?: boolean; toolCallId?: string; content?: Array<{ text?: string }> }> = [];
  expect(pairUnpairedToolCalls(messages as never, m => added.push(m), ABORTED_TOOL_RESULT)).toBe(1);
  expect(added[0]?.toolCallId).toBe("cancel-bash");
  expect(added[0]?.isError).toBe(true);
  expect(added[0]?.content?.[0]?.text).toContain("aborted by the host");
  expect(pairUnpairedToolCalls([
    ...messages,
    { role: "toolResult", toolCallId: "cancel-bash", content: [{ type: "text", text: "done" }] },
  ] as never, () => { throw new Error("already paired"); }, ABORTED_TOOL_RESULT)).toBe(0);
});
