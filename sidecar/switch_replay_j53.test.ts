// 🎯T245 / Jevons J53: after a completed source turn, switching must seed the
// destination from post-turn durable state — the next model call answers only
// the new prompt, not the already-answered source prompt again.

import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createMockModel, registerMockApi, type MockResponse } from "@oh-my-pi/pi-ai/providers/mock";
import type { Context, Model } from "@oh-my-pi/pi-ai";
import { createSeatAgent, type SeatEvent } from "./seat.ts";
import { SeatStore } from "./store.ts";

registerMockApi();

const oldPrompt = "owner probe: remember MARBLEWREN951";
const storedAnswer = "STOREDMARBLEWREN951";
const newPrompt = "Acknowledged — what is 2+2?";

function reply(text: string): MockResponse {
  return { content: [text], usage: { input: 10, output: 1 } };
}

describe("switch replay after completed source turn (J53 / 🎯T245)", () => {
  test("destination next reply is only for the new prompt, not the stored source answer", async () => {
    const root = mkdtempSync(join(tmpdir(), "t245-j53-"));
    try {
      let postSwitchContext: Context | undefined;
      const a = createMockModel({
        id: "a",
        provider: "anthropic",
        contextWindow: 200_000,
        handler: () => reply(storedAnswer),
      });
      const b = createMockModel({
        id: "b",
        provider: "openai-codex",
        contextWindow: 200_000,
        handler: (ctx) => {
          postSwitchContext = ctx;
          return reply("FOUR");
        },
      });
      const models = { "anthropic/a": a, "openai-codex/b": b } as Record<string, Model>;
      const store = new SeatStore(root, "worker", "session");
      const agent = createSeatAgent({
        provider: "anthropic",
        model: "a",
        token: "t",
        cwd: root,
        store,
        emit: () => {},
        callTool: async () => "",
        modelOverride: a as never,
        planToken: () => "t",
        resolveModelOverride: (provider, model) => models[`${provider}/${model}`]!,
        maintenance: { nativeEligible: () => false },
      });

      await agent.prompt(oldPrompt);
      expect(JSON.stringify(agent.snapshot().messages)).toContain(storedAnswer);

      await agent.switchModel("openai-codex", "b");
      expect(agent.snapshot().model?.id).toBe("b");
      const afterSwitch = store.load({ repairTail: false })!;
      expect(JSON.stringify(afterSwitch)).toContain(storedAnswer);
      expect(JSON.stringify(afterSwitch)).not.toContain("FOUR");

      await agent.prompt(newPrompt);
      const ctx = postSwitchContext!;
      expect(ctx.messages.at(-1)?.role).toBe("user");
      expect(ctx.messages.at(-2)?.role).toBe("assistant");
      expect(JSON.stringify(ctx.messages.at(-2))).toContain(storedAnswer);
      expect(JSON.stringify(ctx.messages)).toContain(newPrompt);

      const answer = agent.snapshot().messages.at(-1);
      expect(answer?.role).toBe("assistant");
      const answerText = JSON.stringify(answer);
      expect(answerText).toContain("FOUR");
      expect(answerText).not.toContain(storedAnswer);

      expect(JSON.stringify(store.load({ repairTail: false })!)).toContain("FOUR");
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("strict store reload after switch still seeds destination without replaying the old prompt", async () => {
    const root = mkdtempSync(join(tmpdir(), "t245-j53-reload-"));
    try {
      const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 200_000, handler: () => reply(storedAnswer) });
      const b = createMockModel({ id: "b", provider: "openai-codex", contextWindow: 200_000, handler: () => reply("NEW-ONLY") });
      const models = { "anthropic/a": a, "openai-codex/b": b } as Record<string, Model>;
      const store = new SeatStore(root, "worker", "session");
      const agent = createSeatAgent({
        provider: "anthropic",
        model: "a",
        token: "t",
        cwd: root,
        store,
        emit: () => {},
        callTool: async () => "",
        modelOverride: a as never,
        planToken: () => "t",
        resolveModelOverride: (provider, model) => models[`${provider}/${model}`]!,
        maintenance: { nativeEligible: () => false },
      });
      await agent.prompt(oldPrompt);
      await agent.switchModel("openai-codex", "b");

      const reloaded = createSeatAgent({
        provider: "openai-codex",
        model: "b",
        token: "t",
        cwd: root,
        store,
        emit: () => {},
        callTool: async () => "",
        modelOverride: b as never,
        planToken: () => "t",
        resolveModelOverride: (provider, model) => models[`${provider}/${model}`]!,
        maintenance: { nativeEligible: () => false },
      });

      await reloaded.prompt(newPrompt);
      const answerText = JSON.stringify(reloaded.snapshot().messages.at(-1));
      expect(answerText).toContain("NEW-ONLY");
      expect(answerText).not.toContain(storedAnswer);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("switch-time compaction uses post_turn phase after a completed source turn", async () => {
    const root = mkdtempSync(join(tmpdir(), "t245-j53-phase-"));
    try {
      const big = "the quick brown fox jumps over the lazy dog. ".repeat(1200);
      const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 400_000, handler: () => reply(storedAnswer) });
      const b = createMockModel({ id: "b", provider: "openai-codex", contextWindow: 60_000, handler: () => reply("ok") });
      const models = { "anthropic/a": a, "openai-codex/b": b } as Record<string, Model>;
      const events: SeatEvent[] = [];
      const store = new SeatStore(root, "worker", "session");
      const agent = createSeatAgent({
        provider: "anthropic",
        model: "a",
        token: "t",
        cwd: root,
        store,
        emit: (ev) => events.push(ev),
        callTool: async () => "",
        modelOverride: a as never,
        planToken: () => "t",
        resolveModelOverride: (provider, model) => models[`${provider}/${model}`]!,
        maintenance: { nativeEligible: () => false },
      });
      await agent.prompt(oldPrompt);
      for (let i = 0; i < 8; i++) await agent.prompt(`${i}: ${big}`);
      await agent.switchModel("openai-codex", "b", { on: "destination" });
      const progress = events.filter((e) => e.type === "compacting" || e.type === "compacted").map((e) => e.text ?? "");
      expect(progress.some((t) => t.includes("post_turn"))).toBe(true);
      expect(progress.some((t) => t.includes("pre_turn") && t.includes("switch"))).toBe(false);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });
});
