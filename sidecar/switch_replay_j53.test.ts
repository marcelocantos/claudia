// 🎯T245 / Jevons J53 (T1371): completed source turn → switch → destination must
// answer only the new codeword probe, not re-answer or glue STORED onto it.
// Provenance: jevons 7534c923 …/j53-t1371-switch-post-turn/CLAUDIA_HANDOFF.md

import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createMockModel, registerMockApi, type MockResponse } from "@oh-my-pi/pi-ai/providers/mock";
import type { Context, Model } from "@oh-my-pi/pi-ai";
import { createSeatAgent } from "./seat.ts";
import { SeatStore } from "./store.ts";

registerMockApi();

const sourceCodeword = "MARBLEWREN951";
const storedAnswer = `STORED${sourceCodeword}`;
const newCodeword = "LIMESTONE827";
const sourcePrompt = `owner probe: remember ${sourceCodeword}`;
const newPrompt = `Reply with only the codeword ${newCodeword}, nothing else.`;
const big = "the quick brown fox jumps over the lazy dog. ".repeat(1200);

function reply(text: string): MockResponse {
  return { content: [text], usage: { input: 10, output: 1 } };
}

function assistantText(message: unknown): string {
  return JSON.stringify(message);
}

function destinationHandler(ctx: Context): MockResponse {
  const lastUser = JSON.stringify(ctx.messages.at(-1));
  if (lastUser.includes(newCodeword)) return reply(newCodeword);
  if (lastUser.includes(sourceCodeword)) return reply(storedAnswer);
  return reply("ok");
}

describe("switch replay after completed source turn (J53 / 🎯T245)", () => {
  test("seeded STORED source reply, switch, then next answer is only the new codeword", async () => {
    const root = mkdtempSync(join(tmpdir(), "t245-j53-"));
    try {
      let destinationContext: Context | undefined;
      const phases: string[] = [];
      const a = createMockModel({
        id: "a",
        provider: "anthropic",
        contextWindow: 400_000,
        handler: () => reply(storedAnswer),
      });
      const b = createMockModel({
        id: "b",
        provider: "openai-codex",
        contextWindow: 60_000,
        handler: (ctx) => {
          destinationContext = ctx;
          return destinationHandler(ctx);
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
        emit: (ev) => {
          if (ev.type === "compacting" || ev.type === "compacted") phases.push(ev.text ?? "");
        },
        callTool: async () => "",
        modelOverride: a as never,
        planToken: () => "t",
        resolveModelOverride: (provider, model) => models[`${provider}/${model}`]!,
        maintenance: { nativeEligible: () => false },
      });

      await agent.prompt(sourcePrompt);
      expect(JSON.stringify(agent.snapshot().messages)).toContain(storedAnswer);
      for (let i = 0; i < 8; i++) await agent.prompt(`${i}: ${big}`);

      await agent.switchModel("openai-codex", "b", { on: "destination" });
      expect(agent.snapshot().model?.id).toBe("b");
      expect(phases.some((t) => t.includes("post_turn"))).toBe(true);
      expect(phases.some((t) => t.includes("pre_turn") && t.includes("switch"))).toBe(false);

      const afterSwitch = store.load({ repairTail: false })!;
      expect(JSON.stringify(afterSwitch)).toContain(storedAnswer);
      expect(JSON.stringify(afterSwitch)).not.toContain(newCodeword);

      await agent.prompt(newPrompt);
      const ctx = destinationContext!;
      expect(ctx.messages.at(-1)?.role).toBe("user");
      expect(ctx.messages.at(-2)?.role).toBe("assistant");
      expect(assistantText(ctx.messages.at(-1))).toContain(newCodeword);
      expect(assistantText(ctx.messages.at(-1))).not.toContain(sourceCodeword);

      const answerText = assistantText(agent.snapshot().messages.at(-1));
      expect(answerText).toContain(newCodeword);
      expect(answerText).not.toContain(storedAnswer);
      expect(answerText).not.toMatch(new RegExp(`${storedAnswer}.*${newCodeword}|${newCodeword}.*${storedAnswer}`));
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("strict store reload after switch still answers only the new codeword", async () => {
    const root = mkdtempSync(join(tmpdir(), "t245-j53-reload-"));
    try {
      const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 200_000, handler: () => reply(storedAnswer) });
      const b = createMockModel({ id: "b", provider: "openai-codex", contextWindow: 200_000, handler: destinationHandler });
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
      await agent.prompt(sourcePrompt);
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
      const answerText = assistantText(reloaded.snapshot().messages.at(-1));
      expect(answerText).toContain(newCodeword);
      expect(answerText).not.toContain(storedAnswer);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });
});
