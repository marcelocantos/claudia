// 🎯T245 / Jevons J53 (T1371): completed source turn → switch → destination must
// answer only the new codeword probe, not re-answer or glue STORED onto it.
// Provenance: jevons 7534c923 …/j53-t1371-switch-post-turn/CLAUDIA_HANDOFF.md
//
// Post-backup switch fold rollback is covered by switch_compactor_durable.test.ts
// (🎯T243), e.g. "failed destination switch fold leaves no compaction entry and
// replayable messages on reload" and "still-does-not-fit after switch fold rolls
// back on disk and reload".

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


function isSourceProbeUser(message: unknown): boolean {
  const t = messageText(message);
  // Seat-attributed user probes use string content; compaction summaries embed
  // codewords inside structured blocks and must not count as replay probes.
  return t.includes('"content":"owner probe: remember');
}

function unansweredSourceUserCount(ctx: Context): number {
  const msgs = ctx.messages;
  let dangling = 0;
  for (let i = 0; i < msgs.length; i++) {
    const m = msgs[i];
    if (m.role !== "user") continue;
    if (!isSourceProbeUser(m)) continue;
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

function answeredSourcePairCount(ctx: Context): number {
  const msgs = ctx.messages;
  let pairs = 0;
  for (let i = 0; i < msgs.length; i++) {
    const m = msgs[i];
    if (m.role !== "user") continue;
    if (!isSourceProbeUser(m)) continue;
    for (let j = i + 1; j < msgs.length; j++) {
      if (msgs[j].role === "user") break;
      if (msgs[j].role === "assistant" && messageText(msgs[j]).includes(storedAnswer)) {
        pairs++;
        break;
      }
    }
  }
  return pairs;
}

function assertDestinationSourceContext(ctx: Context): void {
  expect(unansweredSourceUserCount(ctx)).toBe(0);
  expect(answeredSourcePairCount(ctx)).toBe(1);
  const lastUser = ctx.messages.at(-1);
  if (lastUser?.role === "user" && isSourceProbeUser(lastUser)) {
    throw new Error("destination context ends on dangling source user prompt");
  }
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
      if (isSourceProbeUser(lastUser)) {
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

/** Filler turns first so the source codeword turn is the last completed turn before switch. */
async function seedFillerThenSourceTurn(agent: ReturnType<typeof createSeatAgent>): Promise<void> {
  for (let i = 0; i < 8; i++) await agent.prompt(`${i}: ${big}`);
  await agent.prompt(sourcePrompt);
  expect(JSON.stringify(agent.snapshot().messages)).toContain(storedAnswer);
}

function assertProbeAnsweredOnlyNewCodeword(
  destination: DestinationOracle,
  sourceReanswersBefore: number,
  newAnswersBefore: number,
  ctx: Context,
  agentLastMessage: unknown,
): void {
  assertDestinationSourceContext(ctx);
  expect(destination.sourceReanswers() - sourceReanswersBefore).toBe(0);
  expect(destination.newCodewordAnswers() - newAnswersBefore).toBe(1);
  expect(ctx.messages.at(-1)?.role).toBe("user");
  expect(ctx.messages.at(-2)?.role).toBe("assistant");
  expect(assistantText(ctx.messages.at(-1))).toContain(newCodeword);
  expect(assistantText(ctx.messages.at(-1))).not.toContain(sourceCodeword);
  const answerText = assistantText(agentLastMessage);
  expect(answerText).toContain(newCodeword);
  expect(answerText).not.toContain(storedAnswer);
  expect(answerText).not.toMatch(
    new RegExp(`${storedAnswer}.*${newCodeword}|${newCodeword}.*${storedAnswer}`),
  );
}

describe("switch replay after completed source turn (J53 / 🎯T245)", () => {
  test("seeded STORED source reply, switch, then next answer is only the new codeword", async () => {
    const root = mkdtempSync(join(tmpdir(), "t245-j53-"));
    try {
      let destinationContext: Context | undefined;
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
        emit: () => {},
        callTool: async () => "",
        modelOverride: a as never,
        planToken: () => "t",
        resolveModelOverride: (provider, model) => models[`${provider}/${model}`]!,
        maintenance: { nativeEligible: () => false },
      });

      await seedFillerThenSourceTurn(agent);

      await agent.switchModel("openai-codex", "b", { on: "destination" });
      expect(agent.snapshot().model?.id).toBe("b");

      const afterSwitch = store.load({ repairTail: false })!;
      expect(JSON.stringify(afterSwitch)).toContain(storedAnswer);
      expect(JSON.stringify(afterSwitch)).not.toContain(newCodeword);

      expect(destination.sourceReanswers()).toBe(0);

      const sourceReanswersBeforeProbe = destination.sourceReanswers();
      const newAnswersBeforeProbe = destination.newCodewordAnswers();
      await agent.prompt(newPrompt);
      const ctx = destinationContext!;
      assertProbeAnsweredOnlyNewCodeword(
        destination,
        sourceReanswersBeforeProbe,
        newAnswersBeforeProbe,
        ctx,
        agent.snapshot().messages.at(-1),
      );
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
      await seedFillerThenSourceTurn(agent);
      await agent.switchModel("openai-codex", "b", { on: "destination" });

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

      const reBefore = destination.sourceReanswers();
      const newBefore = destination.newCodewordAnswers();
      await reloaded.prompt(newPrompt);
      expect(destination.sourceReanswers() - reBefore).toBe(0);
      expect(destination.newCodewordAnswers() - newBefore).toBe(1);
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
      await seedFillerThenSourceTurn(agent);

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
      let failPreBackupPersist = true;
      const maybeFailPreBackupPersist = () => {
        if (failPreBackupPersist && switchStarted && !existsSync(store.switchBackupPath())) {
          throw new Error("ENOSPC before backup");
        }
      };
      store.append = (entries) => {
        maybeFailPreBackupPersist();
        origAppend(entries);
      };
      store.rewrite = (entries) => {
        maybeFailPreBackupPersist();
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
      await seedFillerThenSourceTurn(agent);
      const journalBeforeFail = JSON.stringify(store.load({ repairTail: false }));

      switchStarted = true;
      await expect(agent.switchModel("openai-codex", "b", { on: "destination" })).rejects.toThrow(/ENOSPC before backup/);
      expect(agent.snapshot().model?.id).toBe("a");
      expect(existsSync(store.switchBackupPath())).toBe(false);
      expect(JSON.stringify(store.load({ repairTail: false }))).toBe(journalBeforeFail);

      failPreBackupPersist = false;
      switchStarted = false;
      await agent.switchModel("openai-codex", "b", { on: "destination" });
      expect(agent.snapshot().model?.id).toBe("b");
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
