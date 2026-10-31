// 🎯T243: switch-time compaction must keep host steer (preserve/pins) on
// successful destination/explicit folds and must not durably commit a partial
// summary when the switch fold fails or is canceled.

import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { compact as engineCompact } from "@oh-my-pi/pi-agent-core";
import { createMockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";
import type { AssistantMessage, Model } from "@oh-my-pi/pi-ai";
import { History, HistoryRollbackError, preserveRule } from "./maintain.ts";
import { createSeatAgent, type SeatEvent } from "./seat.ts";
import { SeatStore } from "./store.ts";
import type { SessionEntry } from "@oh-my-pi/pi-agent-core";

registerMockApi();

const summaryText = "SUMMARY OF EARLIER WORK";
const big = "the quick brown fox jumps over the lazy dog. ".repeat(1200);
const marker = "MARKER-BEFORE-SWITCH-7";

function reply(text: string) {
  return { content: [text], usage: { input: 10, output: 1 } };
}

function compactionCount(entries: readonly SessionEntry[]): number {
  return entries.filter((e) => e.type === "compaction").length;
}

function durableSeat(
  root: string,
  from: Model,
  models: Record<string, Model>,
  opts: {
    preserve?: string;
    pins?: string[];
    store?: SeatStore;
    maintenance?: Parameters<typeof createSeatAgent>[0]["maintenance"];
  } = {},
) {
  const store = opts.store ?? new SeatStore(root, "worker", "session");
  const events: SeatEvent[] = [];
  const told: (string | undefined)[] = [];
  const defaultComplete = (async (model: Model) => {
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
    cwd: root,
    store,
    preserve: opts.preserve,
    pins: opts.pins,
    emit: (ev) => events.push(ev),
    callTool: async () => "",
    modelOverride: from as never,
    planToken: () => "t",
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
      compactImpl:
        opts.maintenance?.compactImpl ??
        ((async (prep, model, token, instructions, signal, extras) => {
          told.push(instructions);
          return engineCompact(prep, model, token, instructions, signal, extras);
        }) as never),
    },
  });
  return { agent, store, told };
}

function reloadSeat(root: string, store: SeatStore, model: Model, models: Record<string, Model>, opts: { preserve?: string; pins?: string[] }) {
  const history = new History(store);
  history.restore();
  const agent = createSeatAgent({
    provider: model.provider,
    model: model.id,
    token: "t",
    cwd: root,
    store,
    preserve: opts.preserve,
    pins: opts.pins,
    emit: () => {},
    callTool: async () => "",
    modelOverride: model as never,
    planToken: () => "t",
    resolveModelOverride: (provider, id) => models[`${provider}/${id}`]!,
    maintenance: { nativeEligible: () => false },
  });
  return { agent, history };
}

describe("switch-time compaction durability (🎯T243)", () => {
  test("destination switch fold keeps preserve/pins on disk and after store reload", async () => {
    const root = mkdtempSync(join(tmpdir(), "t243-dest-"));
    try {
      const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 400_000, handler: () => reply("ok") });
      const b = createMockModel({ id: "b", provider: "openai-codex", contextWindow: 60_000, handler: () => reply("fine") });
      const models = { "anthropic/a": a, "openai-codex/b": b } as Record<string, Model>;
      const { agent, store, told } = durableSeat(root, a as never, models, {
        preserve: "Keep the owner's release policy.",
        pins: ["the owner ships MINOR releases only"],
      });
      await agent.prompt(`PIN: the code word is PELICAN-7\n${marker}`);
      for (let i = 0; i < 8; i++) await agent.prompt(`${i}: ${big}`);

      const before = compactionCount(store.load() ?? []);
      await agent.switchModel("openai-codex", "b", { on: "destination" });
      expect(agent.snapshot().model?.id).toBe("b");

      const onDisk = store.load()!;
      expect(compactionCount(onDisk)).toBe(before + 1);
      const last = onDisk.filter((e) => e.type === "compaction").at(-1)! as SessionEntry & { details?: { pins?: string[] } };
      expect(last.details?.pins).toContain("the code word is PELICAN-7");
      expect(last.details?.pins).toContain("the owner ships MINOR releases only");
      expect(told.some((t) => t?.includes(preserveRule) && t.includes("Keep the owner's release policy."))).toBe(true);

      const { agent: reloaded, history } = reloadSeat(root, store, b as never, models, {
        preserve: "Keep the owner's release policy.",
        pins: ["the owner ships MINOR releases only"],
      });
      expect(reloaded.snapshot().model?.id).toBe("b");
      const text = JSON.stringify(history.rebuild("openai-codex"));
      expect(text).toContain("PELICAN-7");
      expect(text).toContain("the owner ships MINOR releases only");
      expect(text).toContain(summaryText);
      expect(text).not.toContain(marker);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("explicit switch fold keeps preserve/pins on disk and after store reload", async () => {
    const root = mkdtempSync(join(tmpdir(), "t243-explicit-"));
    try {
      const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 400_000, handler: () => reply("ok") });
      const b = createMockModel({ id: "b", provider: "openai-codex", contextWindow: 60_000, handler: () => reply("fine") });
      const models = { "anthropic/a": a, "openai-codex/b": b } as Record<string, Model>;
      const { agent, store, told } = durableSeat(root, a as never, models, {
        preserve: "Keep integration notes verbatim.",
        pins: ["INTEGRATION-PIN-99"],
      });
      await agent.prompt(`PIN: ${marker}`);
      for (let i = 0; i < 8; i++) await agent.prompt(`${i}: ${big}`);

      await agent.switchModel("openai-codex", "b", {
        on: "explicit",
        compact_provider: "openai-codex",
        compact_model: "b",
      });
      expect(told.some((t) => t?.includes("Keep integration notes verbatim"))).toBe(true);

      const onDisk = store.load()!;
      const last = onDisk.filter((e) => e.type === "compaction").at(-1)! as SessionEntry & { details?: { pins?: string[] } };
      expect(last.details?.pins).toContain("INTEGRATION-PIN-99");

      const { history } = reloadSeat(root, store, b as never, models, { pins: ["INTEGRATION-PIN-99"] });
      const text = JSON.stringify(history.rebuild("openai-codex"));
      expect(text).toContain("INTEGRATION-PIN-99");
      expect(text).toContain(summaryText);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("canceled destination switch fold leaves no compaction entry and replayable messages on reload", async () => {
    const root = mkdtempSync(join(tmpdir(), "t243-abort-"));
    try {
      const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 400_000, handler: () => reply("ok") });
      const b = createMockModel({ id: "b", provider: "openai-codex", contextWindow: 50_000, handler: () => reply("fine") });
      const models = { "anthropic/a": a, "openai-codex/b": b } as Record<string, Model>;
      let unblock: (() => void) | undefined;
      const { agent, store } = durableSeat(root, a as never, models, {
        maintenance: {
          compactImpl: async (_prep, _model, _token, _instructions, signal) => {
            await new Promise<void>((_, reject) => {
              signal.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
              unblock = () => reject(new Error("unblocked"));
            });
            throw new Error("unreachable");
          },
        },
      });
      await agent.prompt(marker);
      for (let i = 0; i < 6; i++) await agent.prompt(`${i}: ${big}`);

      const beforeCompactions = compactionCount(store.load()!);
      const switching = agent.switchModel("openai-codex", "b", { on: "destination" });
      for (let i = 0; i < 200 && !unblock; i++) await Bun.sleep(5);
      agent.abort();
      await expect(switching).rejects.toThrow();

      expect(compactionCount(store.load()!)).toBe(beforeCompactions);
      expect(JSON.stringify(store.load())).toContain(marker);

      const { agent: reloaded, history } = reloadSeat(root, store, a as never, models, {});
      expect(reloaded.snapshot().model?.id).toBe("a");
      expect(JSON.stringify(history.rebuild("anthropic"))).toContain(marker);
      expect(JSON.stringify(history.rebuild("anthropic"))).not.toContain(summaryText);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("failed destination switch fold leaves no compaction entry and replayable messages on reload", async () => {
    const root = mkdtempSync(join(tmpdir(), "t243-fail-"));
    try {
      const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 400_000, handler: () => reply("ok") });
      const b = createMockModel({ id: "b", provider: "openai-codex", contextWindow: 50_000, handler: () => reply("fine") });
      const models = { "anthropic/a": a, "openai-codex/b": b } as Record<string, Model>;
      const { agent, store } = durableSeat(root, a as never, models, {
        maintenance: {
          compactImpl: async () => {
            throw new Error("compactor unavailable");
          },
        },
      });
      await agent.prompt(marker);
      for (let i = 0; i < 6; i++) await agent.prompt(`${i}: ${big}`);

      const beforeCompactions = compactionCount(store.load()!);
      await expect(agent.switchModel("openai-codex", "b", { on: "destination" })).rejects.toThrow();
      expect(agent.snapshot().model?.id).toBe("a");
      expect(compactionCount(store.load()!)).toBe(beforeCompactions);

      const { history } = reloadSeat(root, store, a as never, models, {});
      expect(JSON.stringify(history.rebuild("anthropic"))).toContain(marker);
      expect(JSON.stringify(history.rebuild("anthropic"))).not.toContain(summaryText);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("still-does-not-fit after switch fold rolls back on disk and reload", async () => {
    const root = mkdtempSync(join(tmpdir(), "t243-still-"));
    try {
      const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 400_000, handler: () => reply("ok") });
      const b = createMockModel({ id: "b", provider: "openai-codex", contextWindow: 3_000, handler: () => reply("fine") });
      const models = { "anthropic/a": a, "openai-codex/b": b } as Record<string, Model>;
      const { agent, store } = durableSeat(root, a as never, models, {
        maintenance: { compactImpl: engineCompact },
      });
      await agent.prompt(marker);
      for (let i = 0; i < 6; i++) await agent.prompt(`${i}: ${big}`);

      const beforeCompactions = compactionCount(store.load()!);
      await expect(agent.switchModel("openai-codex", "b", { on: "destination" })).rejects.toThrow(/even after compaction/);
      expect(agent.snapshot().model?.id).toBe("a");
      expect(compactionCount(store.load()!)).toBe(beforeCompactions);
      expect(JSON.stringify(store.load())).toContain(marker);

      const { agent: reloaded, history } = reloadSeat(root, store, a as never, models, {});
      expect(reloaded.snapshot().model?.id).toBe("a");
      expect(JSON.stringify(history.rebuild("anthropic"))).toContain(marker);
      expect(JSON.stringify(history.rebuild("anthropic"))).not.toContain(summaryText);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });

  test("store rewrite failure during rollback surfaces HistoryRollbackError and may leave compaction on disk", async () => {
    const root = mkdtempSync(join(tmpdir(), "t243-rwfail-"));
    try {
      const a = createMockModel({ id: "a", provider: "anthropic", contextWindow: 400_000, handler: () => reply("ok") });
      const b = createMockModel({ id: "b", provider: "openai-codex", contextWindow: 3_000, handler: () => reply("fine") });
      const models = { "anthropic/a": a, "openai-codex/b": b } as Record<string, Model>;
      const store = new SeatStore(root, "worker", "session");
      let rewriteCalls = 0;
      const origRewrite = store.rewrite.bind(store);
      store.rewrite = (entries) => {
        rewriteCalls++;
        if (rewriteCalls >= 2) throw new Error("ENOSPC");
        origRewrite(entries);
      };
      const { agent } = durableSeat(root, a as never, models, {
        store,
        maintenance: { compactImpl: engineCompact },
      });
      await agent.prompt(marker);
      for (let i = 0; i < 6; i++) await agent.prompt(`${i}: ${big}`);

      const beforeCompactions = compactionCount(store.load()!);
      await expect(agent.switchModel("openai-codex", "b", { on: "destination" })).rejects.toBeInstanceOf(HistoryRollbackError);
      expect(agent.snapshot().model?.id).toBe("a");
      // Rollback rewrite failed: durable state is ambiguous; compaction row may remain.
      expect(compactionCount(store.load()!)).toBeGreaterThan(beforeCompactions);
    } finally {
      rmSync(root, { recursive: true, force: true });
    }
  });
});
