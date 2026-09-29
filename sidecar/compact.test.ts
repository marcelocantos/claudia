// 🎯T148 (jevons 🎯T926/T927): a long-lived seat compacts before its
// conversation outgrows the model's window, and a provider's "prompt is too
// long" refusal ends the turn as terminal instead of being retried forever.

import { describe, expect, test } from "bun:test";
import { Tokenizer, type AgentMessage } from "@oh-my-pi/pi-agent-core";
import { createMockModel, registerMockApi, type MockResponse } from "@oh-my-pi/pi-ai/providers/mock";
import type { Context } from "@oh-my-pi/pi-ai";
import { ContextOverflow, thresholdTokens } from "./compact.ts";
import { createSeatAgent, type SeatEvent } from "./seat.ts";

registerMockApi();

// window is small enough that a handful of prompts fills it.
const window = 100_000;
// promptText is roughly 12k tokens: eight of them overflow the window.
const promptText = "the quick brown fox jumps over the lazy dog. ".repeat(1200);

// sent is what the mock provider counts for a request: every message it was
// sent plus the system prompt and tool schemas.
function sent(context: Context, tokenizer: Tokenizer): number {
  let n = 0;
  for (const m of context.messages) n += tokenizer.countMessage(m as AgentMessage);
  const prefix = [...(context.systemPrompt ?? [])];
  for (const t of context.tools ?? []) prefix.push(t.name, t.description ?? "", JSON.stringify(t.parameters ?? {}));
  return n + tokenizer.countTokens(prefix);
}

function tooLong(tokens: number): MockResponse {
  return {
    stopReason: "error",
    errorMessage: `400 {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: ${tokens} tokens > ${window} maximum"}}`,
  };
}

function seat(handler: (context: Context) => MockResponse) {
  const mock = createMockModel({ contextWindow: window, handler: (context) => handler(context) });
  const events: SeatEvent[] = [];
  const summaries: number[] = [];
  const agent = createSeatAgent({
    provider: "mock",
    model: "mock-model",
    token: "t",
    cwd: process.cwd(),
    emit: (ev) => events.push(ev),
    callTool: async () => "",
    modelOverride: mock as never,
    summarize: async (head) => {
      summaries.push(head.length);
      return `summary of ${head.length} messages`;
    },
  });
  return { mock, events, summaries, agent };
}

describe("compaction (🎯T148)", () => {
  test("a seat near its window compacts before the provider can refuse it", async () => {
    const tokenizer = new Tokenizer(null);
    const requests: number[] = [];
    const { mock, events, summaries, agent } = seat((context) => {
      const tokens = sent(context, tokenizer);
      requests.push(tokens);
      if (tokens > window) return tooLong(tokens);
      return { content: ["ok"], usage: { input: tokens, output: 1 } };
    });

    // Twenty prompts: without compaction the eighth is past the window.
    for (let i = 0; i < 20; i++) {
      await agent.prompt(`${i}: ${promptText}`);
    }

    const ends = events.filter((e) => e.type === "turn_end");
    expect(ends.length).toBe(20);
    expect(ends.filter((e) => e.error)).toEqual([]);
    // No request the provider saw was ever over the window: compaction ran
    // first, every time it was needed.
    expect(Math.max(...requests)).toBeLessThanOrEqual(window);
    expect(mock.calls.length).toBe(20);
    const compacted = events.filter((e) => e.type === "compacted");
    expect(compacted.length).toBeGreaterThan(0);
    expect(summaries.length).toBe(compacted.length);
    // It waited until the conversation was past the threshold, not sooner.
    expect(Math.max(...requests)).toBeGreaterThan(thresholdTokens(window) - 2 * 12_500);
  });

  test("a 400 prompt-too-long is terminal: one compaction retry, then an overflow turn_end", async () => {
    let refuse = false;
    const { mock, events, summaries, agent } = seat(() =>
      refuse ? tooLong(1_058_579) : { content: ["ok"], usage: { input: 1000, output: 1 } },
    );
    // A conversation with history to fold, then a provider that refuses
    // everything as too long (its window is smaller than the catalog says).
    for (let i = 0; i < 4; i++) await agent.prompt(`${i}: ${promptText}`);
    const before = mock.calls.length;
    refuse = true;

    await agent.prompt("one more");

    // Exactly two requests: the refused one, and one after compacting.
    expect(mock.calls.length - before).toBe(2);
    expect(summaries.length).toBe(1);
    const end = events.filter((e) => e.type === "turn_end").at(-1);
    expect(end?.error).toContain("prompt is too long");
    expect(end?.reason).toBe(ContextOverflow);

    // Nothing keeps retrying after the turn ended.
    await Bun.sleep(200);
    expect(mock.calls.length - before).toBe(2);
    expect(events.at(-1)?.type).toBe("turn_end");
  });

  test("a 400 with nothing to fold ends the turn at once, without a retry", async () => {
    const { mock, events, summaries, agent } = seat(() => tooLong(1_058_579));

    await agent.prompt("hello");

    expect(mock.calls.length).toBe(1);
    expect(summaries.length).toBe(0);
    const end = events.find((e) => e.type === "turn_end");
    expect(end?.reason).toBe(ContextOverflow);
  });

  test("an ordinary refusal is not labelled overflow", async () => {
    const { mock, events, agent } = seat(() => ({
      stopReason: "error",
      errorMessage: "429 usage_limit_reached: weekly limit resets 2026-10-03T23:20Z",
    }));

    await agent.prompt("hello");

    expect(mock.calls.length).toBe(1);
    const end = events.find((e) => e.type === "turn_end");
    expect(end?.error).toContain("429");
    expect(end?.reason).toBeUndefined();
  });
});
