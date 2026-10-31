// 🎯T245 / Jevons J53 (T1371): completed source turn → switch → destination must
// answer only the new codeword probe, not re-answer or glue STORED onto it.
// Provenance: jevons 7534c923 …/j53-t1371-switch-post-turn/CLAUDIA_HANDOFF.md

import { describe, expect, test } from "bun:test";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createMockModel, registerMockApi, type MockResponse } from "@oh-my-pi/pi-ai/providers/mock";
import type { AgentMessage, Context, Model } from "@oh-my-pi/pi-ai";
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

function messageText(message: unknown): string {
  return JSON.stringify(message);
}

function unansweredSourceUserCount(ctx: Context): number {
  const msgs = ctx.messages;
  let dangling = 0;
  for (let i = 0; i < msgs.length; i++) {
    const m = msgs[i];
    if (m.role !== "user") continue;
    if (!messageText(m).includes(sourceCodeword)) continue;
    let answered = false;
    for (let j = i + 1; j < msgs.length; j++) {
      if (msgs[j].role === "user") break;
      if (msgs[j].role === "assistant" && messageText(msgs[j]).includes(storedAnswer)) {
        answered = true;
        break;
      }
    }
    if (!answered) dangling++;
  }
  return dangling;
}

function latestUserMessage(ctx: Context): AgentMessage | undefined {
  for (let i = ctx.messages.length - 1; i >= 0; i--) {
    if (ctx.messages[i].role === "user") return ctx.messages[i] as AgentMessage;
  }
  return undefined;
}

type DestinationOracle = {
  handler: (ctx: Context) => MockResponse;
  sourceReanswers: () => number;
  newCodewordAnswers: () => number;
};

function strictDestinationOracle(): DestinationOracle {
  let sourceReanswers = 0;
  let newCodewordAnswers = 0;
  return {
    sourceReanswers: () => sourceReanswers,
    newCodewordAnswers: () => newCodewordAnswers,
    handler(ctx: Context) {
      const lastUser = latestUserMessage(ctx);
      const lastUserText = messageText(lastUser);
      if (lastUserText.includes(newCodeword)) {
        if (unansweredSourceUserCount(ctx) > 0) {
          sourceReanswers++;
          return reply(storedAnswer);
        }
        newCodewordAnswers++;
        return reply(newCodeword);
      }
      if (lastUserText.includes(sourceCodeword)) {
        sourceReanswers++;
        return reply(storedAnswer);
      }
      return reply("ok");
    },
  };
}

function assistantText(message: unknown): string {
  return messageText(message);
}

describe("switch replay after completed source turn (J53 / 🎯T245)", () => {
  test("seeded STORED source reply, switch, then next answer is only the new codeword", async () => {
    const root = mkdtempSync(join(tmpdir(), "t245-j53-"));
    try {
      let destinationContext: Context | undefined;
      const phases: string[] = [];
      const destination = strictDestinationOracle();
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
          return destination.handler(ctx);
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

      const sourceReanswersBeforeProbe = destination.sourceReanswers();
      const newAnswersBeforeProbe = destination.newCodewordAnswers();
      await agent.prompt(newPrompt);
      const ctx = destinationContext!;
      expect(unansweredSourceUserCount(ctx)).toBe(0);
      expect(destination.sourceReanswers() - sourceReanswersBeforeProbe).toBe(0);
      expect(destination.newCodewordAnswers() - newAnswersBeforeProbe).toBe(1);
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
      const destination = strictDestinationOracle();
      const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 200_000, handler: () => reply(storedAnswer) });
      const b = createMockModel({
        id: "b",
        provider: "openai-codex",
        contextWindow: 200_000,
        handler: (ctx) => destination.handler(ctx),
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
      expect(destination.sourceReanswers()).toBe(0);
      expect(destination.newCodewordAnswers()).toBe(1);
      const answerText = assistantText(reloaded.snapshot().messages.at(-1));
      expect(answerText).toContain(newCodeword);
      expect(answerText).not.toContain(storedAnswer);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("post-turn persist lands on disk before switch durable backup begins", async () => {
    const root = mkdtempSync(join(tmpdir(), "t245-j53-order-"));
    try {
      let switchStarted = false;
      let persistBeforeBackup = false;
      const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 400_000, handler: () => reply(storedAnswer) });
      const b = createMockModel({ id: "b", provider: "openai-codex", contextWindow: 60_000, handler: () => reply("fine") });
      const models = { "anthropic/a": a, "openai-codex/b": b } as Record<string, Model>;
      const store = new SeatStore(root, "worker", "session");
      const origAppend = store.append.bind(store);
      const origRewrite = store.rewrite.bind(store);
      const origBackup = store.beginSwitchBackup.bind(store);
      const markPreBackupPersist = () => {
        if (switchStarted && !existsSync(store.switchBackupPath())) persistBeforeBackup = true;
      };
      store.append = (entries) => {
        markPreBackupPersist();
        origAppend(entries);
      };
      store.rewrite = (entries) => {
        markPreBackupPersist();
        origRewrite(entries);
      };
      store.beginSwitchBackup = () => {
        expect(persistBeforeBackup).toBe(true);
        expect(JSON.stringify(store.load({ repairTail: false }))).toContain(storedAnswer);
        origBackup();
      };
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
      for (let i = 0; i < 8; i++) await agent.prompt(`${i}: ${big}`);

      switchStarted = true;
      await agent.switchModel("openai-codex", "b", { on: "destination" });
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("pre-backup persist failure leaves no switch backup and replayable STORED journal", async () => {
    const root = mkdtempSync(join(tmpdir(), "t245-j53-prebackup-fail-"));
    try {
      const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 400_000, handler: () => reply(storedAnswer) });
      const b = createMockModel({ id: "b", provider: "openai-codex", contextWindow: 60_000, handler: () => reply("fine") });
      const models = { "anthropic/a": a, "openai-codex/b": b } as Record<string, Model>;
      const store = new SeatStore(root, "worker", "session");
      const origAppend = store.append.bind(store);
      const origRewrite = store.rewrite.bind(store);
      let switchStarted = false;
      const failPreBackupPersist = () => {
        if (switchStarted && !existsSync(store.switchBackupPath())) {
          throw new Error("ENOSPC before backup");
        }
      };
      store.append = (entries) => {
        failPreBackupPersist();
        origAppend(entries);
      };
      store.rewrite = (entries) => {
        failPreBackupPersist();
        origRewrite(entries);
      };
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
      for (let i = 0; i < 8; i++) await agent.prompt(`${i}: ${big}`);

      switchStarted = true;
      await expect(agent.switchModel("openai-codex", "b", { on: "destination" })).rejects.toThrow(/ENOSPC before backup/);
      expect(agent.snapshot().model?.id).toBe("a");
      expect(existsSync(store.switchBackupPath())).toBe(false);
      expect(JSON.stringify(store.load({ repairTail: false }))).toContain(storedAnswer);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("uncompacted failed switch leaves replayable STORED without switch backup", async () => {
    const root = mkdtempSync(join(tmpdir(), "t245-j53-uncompacted-fail-"));
    try {
      const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 400_000, handler: () => reply(storedAnswer) });
      const models = { "anthropic/a": a } as Record<string, Model>;
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
        resolveModelOverride: (provider, model) => {
          const m = models[`${provider}/${model}`];
          if (!m) throw new Error(`unknown model ${provider}/${model}`);
          return m;
        },
        maintenance: { nativeEligible: () => false },
      });
      await agent.prompt(sourcePrompt);
      expect(JSON.stringify(store.load({ repairTail: false }))).toContain(storedAnswer);

      await expect(agent.switchModel("openai-codex", "missing")).rejects.toThrow(/unknown model openai-codex\/missing/);
      expect(agent.snapshot().model?.id).toBe("a");
      expect(existsSync(store.switchBackupPath())).toBe(false);
      expect(JSON.stringify(store.load({ repairTail: false }))).toContain(storedAnswer);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });
});
