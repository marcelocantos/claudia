// 🎯T195: a seat moves to another provider or model in place, between turns,
// keeping its conversation. Nothing is summarised for the move itself; a
// conversation too large for the new model is folded first, by the model
// that wrote it.

import { describe, expect, test } from "bun:test";
import { createMockModel, registerMockApi, type MockResponse } from "@oh-my-pi/pi-ai/providers/mock";
import type { AssistantMessage, Context, Model } from "@oh-my-pi/pi-ai";
import { createSeatAgent, type SeatEvent } from "./seat.ts";

registerMockApi();

const summaryText = "SUMMARY OF EARLIER WORK";

function reply(text: string): MockResponse {
  return { content: [text], usage: { input: 10, output: 1 } };
}

function mentions(context: Context, text: string): boolean {
  return JSON.stringify(context.messages).includes(text);
}

// seat builds a seat on model `from`, whose switches resolve through models.
function seat(
  from: Model,
  models: Record<string, Model>,
  opts: {
    tools?: boolean;
    summaries?: string[];
    planToken?: (provider: string) => string;
    maintenance?: Pick<import("./maintain.ts").MaintenanceHost, "nativeEligible" | "summaryOptions">;
  } = {},
) {
  const events: SeatEvent[] = [];
  let release: (v: string) => void = () => {};
  const defaultComplete = (async (model: Model) => {
    opts.summaries?.push(`${model.provider}/${model.id}`);
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
  }) as never;
  const agent = createSeatAgent({
    provider: from.provider,
    model: from.id,
    token: "t",
    cwd: process.cwd(),
    emit: (ev) => events.push(ev),
    callTool: () => new Promise<string>((resolve) => (release = resolve)),
    tools: opts.tools ? [{ name: "jevons_wait", description: "wait on the host", input_schema: { type: "object" } }] : undefined,
    modelOverride: from as never,
    planToken: opts.planToken,
    resolveModelOverride: (provider, model) => {
      const m = models[`${provider}/${model}`];
      if (!m) throw new Error(`unknown model ${provider}/${model}`);
      return m;
    },
    maintenance: {
      nativeEligible: () => false,
      ...opts.maintenance,
      summaryOptions: {
        ...opts.maintenance?.summaryOptions,
        completeImpl: opts.maintenance?.summaryOptions?.completeImpl ?? defaultComplete,
      },
    },
  });
  return { agent, events, release: (v: string) => release(v) };
}

describe("switching a seat in place (🎯T195)", () => {
  test("a provider switch keeps the conversation: the new model sees the earlier turns", async () => {
    const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 200_000, handler: () => reply("noted") });
    let seenByB: Context | undefined;
    const b = createMockModel({
      id: "b",
      provider: "openai-codex",
      contextWindow: 200_000,
      handler: (context) => {
        seenByB = context;
        return reply("it was 4217");
      },
    });
    const s = seat(a as never, { "openai-codex/b": b as never });
    await s.agent.prompt("remember the number 4217");

    await s.agent.switchModel("openai-codex", "b");
    expect(s.agent.snapshot().model?.id).toBe("b");
    expect(s.agent.snapshot().model?.provider).toBe("openai-codex");

    await s.agent.prompt("what was the number?");
    expect(seenByB).toBeDefined();
    expect(mentions(seenByB!, "remember the number 4217")).toBe(true);
    expect(mentions(seenByB!, "noted")).toBe(true);
    // Nothing was summarised for the move.
    expect(s.events.some((e) => e.type === "compacting")).toBe(false);
  });

  test("a switch while a turn runs is refused and the seat stays on its model", async () => {
    let calls = 0;
    const a = createMockModel({
      id: "a",
      provider: "anthropic",
      contextWindow: 200_000,
      handler: () => {
        calls++;
        if (calls === 1) return { content: [{ type: "toolCall", name: "jevons_wait", arguments: {} }], usage: { input: 10, output: 1 } };
        return reply("done");
      },
    });
    const b = createMockModel({ id: "b", provider: "openai-codex", contextWindow: 200_000, handler: () => reply("b") });
    const s = seat(a as never, { "openai-codex/b": b as never }, { tools: true });
    const turn = s.agent.prompt("go");
    for (let i = 0; i < 300 && calls === 0; i++) await Bun.sleep(10);

    await expect(s.agent.switchModel("openai-codex", "b")).rejects.toThrow("a turn is running");
    expect(s.agent.snapshot().model?.id).toBe("a");

    s.release("ok");
    await turn;
  });

  test("an unknown model is refused and the seat stays on its model", async () => {
    const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 200_000, handler: () => reply("ok") });
    const s = seat(a as never, {});
    await expect(s.agent.switchModel("openai-codex", "nope")).rejects.toThrow("unknown model");
    expect(s.agent.snapshot().model?.id).toBe("a");
  });

  test("a switch to a smaller window folds the conversation first, with the model that wrote it", async () => {
    const big = "the quick brown fox jumps over the lazy dog. ".repeat(1200); // ~12k tokens
    const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 400_000, handler: () => reply("ok") });
    let seenByB: Context | undefined;
    const b = createMockModel({
      id: "b",
      provider: "openai-codex",
      contextWindow: 60_000,
      handler: (context) => {
        seenByB = context;
        return reply("fine");
      },
    });
    const summaries: string[] = [];
    const s = seat(a as never, { "openai-codex/b": b as never }, { summaries });
    for (let i = 0; i < 8; i++) await s.agent.prompt(`${i}: ${big}`);

    await s.agent.switchModel("openai-codex", "b");
    // The fold ran (the engine makes more than one summary call) and only
    // the old model wrote it.
    expect(summaries.length).toBeGreaterThan(0);
    expect([...new Set(summaries)]).toEqual(["anthropic/a"]);
    expect(s.agent.snapshot().model?.id).toBe("b");

    await s.agent.prompt("carry on");
    expect(mentions(seenByB!, summaryText)).toBe(true);
  });

  test("destination compactor folds when the source summarizer is throttled", async () => {
    const big = "the quick brown fox jumps over the lazy dog. ".repeat(1200);
    const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 400_000, handler: () => reply("ok") });
    const b = createMockModel({
      id: "b",
      provider: "openai-codex",
      contextWindow: 60_000,
      handler: () => reply("fine"),
    });
    const summaries: string[] = [];
    const s = seat(a as never, { "openai-codex/b": b as never }, {
      summaries,
      planToken: () => "t",
      maintenance: {
        nativeEligible: () => false,
        summaryOptions: {
          completeImpl: (async (model: Model) => {
            if (model.provider === "anthropic") throw new Error("rate limit exceeded");
            summaries.push(`${model.provider}/${model.id}`);
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
          }) as never,
        },
      },
    });
    for (let i = 0; i < 8; i++) await s.agent.prompt(`${i}: ${big}`);

    await s.agent.switchModel("openai-codex", "b", { on: "destination" });
    expect(summaries.length).toBeGreaterThan(0);
    expect([...new Set(summaries)]).toEqual(["openai-codex/b"]);
    expect(s.agent.snapshot().model?.id).toBe("b");
  });

  test("default switch fold still uses only the source model", async () => {
    const big = "word ".repeat(8000);
    const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 400_000, handler: () => reply("ok") });
    const b = createMockModel({ id: "b", provider: "openai-codex", contextWindow: 50_000, handler: () => reply("fine") });
    const summaries: string[] = [];
    const s = seat(a as never, { "openai-codex/b": b as never }, { summaries, planToken: () => "t" });
    for (let i = 0; i < 6; i++) await s.agent.prompt(`${i}: ${big}`);
    await s.agent.switchModel("openai-codex", "b");
    expect([...new Set(summaries)]).toEqual(["anthropic/a"]);
  });
});

describe("history after a native compaction, for another provider (🎯T195)", () => {
  test("an OpenAI replacement history is used by its own provider only; another gets the summary and the kept turns", async () => {
    const { History } = await import("./maintain.ts");
    const h = new History();
    const user = (text: string, t: number) => ({ role: "user", content: [{ type: "text", text }], timestamp: t }) as never;
    h.sync([user("old turn", 1), user("kept turn", 2), user("after", 3)]);
    const kept = h.entries[1].id;
    h.appendCompaction(
      {
        summary: "SUMMARY",
        firstKeptEntryId: kept,
        tokensBefore: 1000,
        preserveData: { openaiRemoteCompaction: { provider: "openai-codex", replacementHistory: [{ type: "message", role: "user", content: "opaque" }] } },
      },
      "remote",
    );
    h.sync([user("old turn", 1), user("kept turn", 2), user("after", 3), user("newest", 4)]);

    const own = JSON.stringify(h.rebuild("openai-codex"));
    expect(own).toContain("opaque");
    expect(own).not.toContain("kept turn");

    const other = JSON.stringify(h.rebuild("anthropic"));
    expect(other).not.toContain("opaque");
    expect(other).toContain("SUMMARY");
    expect(other).toContain("kept turn");
    expect(other).toContain("newest");
  });
});
