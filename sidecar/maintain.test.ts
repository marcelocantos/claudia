// 🎯T150 (jevons 🎯T926): a seat manages its context the way the Oh My Pi CLI
// does — it compacts before a prompt, between tool calls, after a turn and on
// overflow, through pi-agent-core's engine, and never loops. Supersedes the
// 🎯T148 tests; their cases are kept below.

import { describe, expect, test } from "bun:test";
import {
  compact as engineCompact,
  NativeCompactionError,
  Tokenizer,
  type AgentMessage,
  type CompactionPreparation,
} from "@oh-my-pi/pi-agent-core";
import { createMockModel, registerMockApi, type MockResponse } from "@oh-my-pi/pi-ai/providers/mock";
import type { AssistantMessage, Context, Model } from "@oh-my-pi/pi-ai";
import { ContextOverflow, thresholdTokens } from "./maintain.ts";
import { createSeatAgent, type HostTool, type SeatEvent } from "./seat.ts";

registerMockApi();

// window is small enough that a handful of prompts fills it.
const window = 100_000;
// promptText is roughly 12k tokens: eight of them overflow the window.
const promptText = "the quick brown fox jumps over the lazy dog. ".repeat(1200);
const summaryText = "SUMMARY OF EARLIER WORK";

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

function hasText(context: Context, text: string): boolean {
  return JSON.stringify(context.messages).includes(text);
}

// fakeSummary stands in for the summarizer's provider call, so the real
// engine (prepareCompaction, compact) runs against a mock.
async function fakeSummary(model: Model): Promise<AssistantMessage> {
  return {
    role: "assistant",
    content: [{ type: "text", text: summaryText }],
    api: model.api,
    provider: model.provider,
    model: model.id,
    usage: { input: 0, output: 10, cacheRead: 0, cacheWrite: 0, totalTokens: 10, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
    stopReason: "stop",
    timestamp: Date.now(),
  } as AssistantMessage;
}

type SeatOptions = {
  tools?: HostTool[];
  callTool?: (name: string) => string;
  compactImpl?: typeof engineCompact;
  nativeEligible?: () => boolean;
};

function seat(handler: (context: Context) => MockResponse, o: SeatOptions = {}) {
  const mock = createMockModel({ contextWindow: window, handler: (context) => handler(context) });
  const events: SeatEvent[] = [];
  const summaries: number[] = [];
  const agent = createSeatAgent({
    provider: "mock",
    model: "mock-model",
    token: "t",
    cwd: process.cwd(),
    emit: (ev) => events.push(ev),
    callTool: async (_id, name) => o.callTool?.(name) ?? "",
    tools: o.tools,
    modelOverride: mock as never,
    maintenance: {
      compactImpl: o.compactImpl,
      nativeEligible: o.nativeEligible,
      summaryOptions: {
        completeImpl: (async (model: Model) => {
          summaries.push(1);
          return fakeSummary(model);
        }) as never,
      },
    },
  });
  return { mock, events, summaries, agent };
}

const compactions = (events: SeatEvent[]) => events.filter((e) => e.type === "compacted");

describe("compaction as the Oh My Pi CLI does it (🎯T150)", () => {
  test("a seat near its window compacts before the prompt, and the summary reaches the provider", async () => {
    const tokenizer = new Tokenizer(null);
    const requests: number[] = [];
    let summarySeen = false;
    const { mock, events, summaries, agent } = seat((context) => {
      const tokens = sent(context, tokenizer);
      requests.push(tokens);
      if (hasText(context, summaryText)) summarySeen = true;
      if (tokens > window) return tooLong(tokens);
      return { content: ["ok"], usage: { input: tokens, output: 1 } };
    });

    for (let i = 0; i < 20; i++) await agent.prompt(`${i}: ${promptText}`);

    const ends = events.filter((e) => e.type === "turn_end");
    expect(ends.length).toBe(20);
    expect(ends.filter((e) => e.error)).toEqual([]);
    expect(Math.max(...requests)).toBeLessThanOrEqual(window);
    expect(mock.calls.length).toBe(20);
    expect(compactions(events).length).toBeGreaterThan(0);
    expect(summaries.length).toBeGreaterThan(0);
    // The Agent's default converter would drop the summary role: the model
    // must actually be sent what was folded.
    expect(summarySeen).toBe(true);
    // It waited until the conversation was past the threshold.
    expect(Math.max(...requests)).toBeGreaterThan(thresholdTokens(window) - 2 * 12_500);
  });

  test("a long tool loop compacts between tool calls and keeps the newest tool result", async () => {
    const tokenizer = new Tokenizer(null);
    const big = "tool output line of moderate length for testing. ".repeat(1400); // ~14k tokens
    let call = 0;
    let lastResultKept = true;
    const { events, agent } = seat(
      (context) => {
        const tokens = sent(context, tokenizer);
        if (tokens > window) return tooLong(tokens);
        call++;
        // Each tool result must be in the request that follows it.
        if (call > 1 && !hasText(context, `result ${call - 1}`)) lastResultKept = false;
        if (call <= 12) {
          return { content: [{ type: "toolCall", name: "jevons_big", arguments: { n: call } }], usage: { input: tokens, output: 1 } };
        }
        return { content: ["done"], usage: { input: tokens, output: 1 } };
      },
      {
        tools: [{ name: "jevons_big", description: "returns a lot", input_schema: { type: "object" } }],
        callTool: () => `result ${call}\n${big}`,
      },
    );

    await agent.prompt("start the long job");

    const end = events.filter((e) => e.type === "turn_end").at(-1);
    expect(end?.error).toBeUndefined();
    const mid = compactions(events).filter((e) => (e.text ?? "").includes("mid_turn"));
    expect(mid.length).toBeGreaterThan(0);
    expect(lastResultKept).toBe(true);
  });

  test("an overflow compacts, drops the refused turn and retries it once — and it succeeds", async () => {
    let refuseOnce = false;
    const { mock, events, agent } = seat((context) => {
      if (refuseOnce && !hasText(context, summaryText)) {
        refuseOnce = false;
        return tooLong(123_456);
      }
      return { content: ["ok"], usage: { input: 1000, output: 1 } };
    });
    for (let i = 0; i < 4; i++) await agent.prompt(`${i}: ${promptText}`);
    const before = mock.calls.length;
    refuseOnce = true;

    await agent.prompt("one more");

    expect(mock.calls.length - before).toBe(2);
    const end = events.filter((e) => e.type === "turn_end").at(-1);
    expect(end?.error).toBeUndefined();
    expect(compactions(events).some((e) => (e.text ?? "").startsWith("overflow"))).toBe(true);
  });

  test("a 400 prompt-too-long is terminal: one compaction retry, then an overflow turn_end", async () => {
    let refuse = false;
    const { mock, events, summaries, agent } = seat(() =>
      refuse ? tooLong(1_058_579) : { content: ["ok"], usage: { input: 1000, output: 1 } },
    );
    for (let i = 0; i < 4; i++) await agent.prompt(`${i}: ${promptText}`);
    const before = mock.calls.length;
    refuse = true;

    await agent.prompt("one more");

    expect(mock.calls.length - before).toBe(2);
    // One compaction for the overflow; the retry's refusal is not compacted again.
    expect(compactions(events).filter((e) => (e.text ?? "").startsWith("overflow")).length).toBe(1);
    expect(summaries.length).toBeGreaterThan(0);
    const end = events.filter((e) => e.type === "turn_end").at(-1);
    expect(end?.error).toContain("prompt is too long");
    expect(end?.reason).toBe(ContextOverflow);

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

  test("an ordinary refusal is not labelled overflow and does not compact", async () => {
    const { mock, events, agent } = seat(() => ({
      stopReason: "error",
      errorMessage: "429 usage_limit_reached: weekly limit resets 2026-10-03T23:20Z",
    }));

    await agent.prompt("hello");

    expect(mock.calls.length).toBe(1);
    const end = events.find((e) => e.type === "turn_end");
    expect(end?.error).toContain("429");
    expect(end?.reason).toBeUndefined();
    expect(compactions(events)).toEqual([]);
  });

  test("a failed provider-native compaction falls through to a local summary", async () => {
    const methods: string[] = [];
    const { events, agent } = seat(() => ({ content: ["ok"], usage: { input: 1000, output: 1 } }), {
      nativeEligible: () => true,
      compactImpl: (async (prep: CompactionPreparation, ...rest: unknown[]) => {
        methods.push(prep.settings.remoteEnabled === false ? "soft" : "remote");
        if (prep.settings.remoteEnabled !== false) throw new NativeCompactionError(new Error("native lane refused"));
        return (engineCompact as (...a: unknown[]) => unknown)(prep, ...rest);
      }) as never,
    });

    for (let i = 0; i < 9; i++) await agent.prompt(`${i}: ${promptText}`);

    expect(methods.slice(0, 2)).toEqual(["remote", "soft"]);
    expect(compactions(events).some((e) => (e.text ?? "").includes("method soft"))).toBe(true);
  });

  test("a second compaction builds on the first summary", async () => {
    const previous: (string | undefined)[] = [];
    const { agent } = seat(() => ({ content: ["ok"], usage: { input: 1000, output: 1 } }), {
      compactImpl: (async (prep: CompactionPreparation, ...rest: unknown[]) => {
        previous.push(prep.previousSummary);
        return (engineCompact as (...a: unknown[]) => unknown)(prep, ...rest);
      }) as never,
    });

    for (let i = 0; i < 20; i++) await agent.prompt(`${i}: ${promptText}`);

    expect(previous.length).toBeGreaterThan(1);
    expect(previous[0]).toBeUndefined();
    expect(previous[1]).toContain(summaryText);
  });

  test("an oversized first prompt is a dead end: one warning, no summary, no loop", async () => {
    // Past the threshold but inside the window, with nothing before it to fold.
    const huge = "an enormous single message that cannot be folded. ".repeat(8000);
    const { mock, events, agent } = seat(() => ({ content: ["ok"], usage: { input: 1000, output: 1 } }));

    await agent.prompt(huge);

    const warnings = events.filter((e) => e.type === "compaction_warning");
    expect(warnings.length).toBe(1);
    expect(warnings[0].text).toContain("nothing to fold");
    // The turn itself still ran, once.
    expect(mock.calls.length).toBe(1);
    expect(events.filter((e) => e.type === "turn_end").at(-1)?.error).toBeUndefined();
    // Once a cut point exists (the answer now follows the prompt), the parked
    // pass re-arms and folds it, once: the CLI's rule.
    expect(compactions(events).length).toBeLessThanOrEqual(1);
  });

  test("a message that arrives while the seat is compacting is delivered in that turn", async () => {
    let compacting: (() => void) | undefined;
    const started = new Promise<void>((r) => (compacting = r));
    const { events, agent } = seat(
      (context) =>
        hasText(context, "arrived while compacting")
          ? { content: ["got it"], usage: { input: 1000, output: 1 } }
          : { content: ["ok"], usage: { input: 1000, output: 1 } },
      {
        // A slow summary, so a prompt can arrive in the middle of it.
        compactImpl: (async (...args: unknown[]) => {
          compacting?.();
          await Bun.sleep(50);
          return (engineCompact as (...a: unknown[]) => unknown)(...args);
        }) as never,
      },
    );
    const turns: Promise<void>[] = [];
    for (let i = 0; i < 20 && compactions(events).length === 0; i++) {
      const turn = agent.prompt(`${i}: ${promptText}`);
      turns.push(turn);
      const first = await Promise.race([turn.then(() => "done"), started.then(() => "compacting")]);
      if (first === "compacting") {
        await agent.prompt("arrived while compacting");
        await turn;
        break;
      }
    }
    await Promise.all(turns);

    expect(compactions(events).length).toBeGreaterThan(0);
    expect(events.filter((e) => e.type === "absorbed").map((e) => e.text)).toContain("arrived while compacting");
  });

  test("a message that arrives during a post-turn compaction is delivered, not stranded", async () => {
    let compacting: (() => void) | undefined;
    const started = new Promise<void>((r) => (compacting = r));
    let answers = 0;
    const { events, agent } = seat(
      (context) => {
        if (hasText(context, "arrived after the answer")) return { content: ["got it"], usage: { input: 1000, output: 1 } };
        answers++;
        // The fourth answer reports a context past the threshold: only the
        // post-turn check can see it.
        return { content: ["ok"], usage: { input: answers === 4 ? 90_000 : 1000, output: 1 } };
      },
      {
        compactImpl: (async (...args: unknown[]) => {
          compacting?.();
          await Bun.sleep(50);
          return (engineCompact as (...a: unknown[]) => unknown)(...args);
        }) as never,
      },
    );
    for (let i = 0; i < 3; i++) await agent.prompt(`${i}: short prompt`);
    const turn = agent.prompt("3: the answer to this one is large");
    await started;
    await agent.prompt("arrived after the answer");
    await turn;

    const phases = compactions(events).map((e) => e.text ?? "");
    expect(phases.some((t) => t.includes("post_turn"))).toBe(true);
    expect(events.filter((e) => e.type === "absorbed").map((e) => e.text)).toContain("arrived after the answer");
  });
});
