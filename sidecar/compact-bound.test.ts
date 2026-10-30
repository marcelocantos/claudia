// 🎯T244: synchronous bound compaction oracles (auditor fixes).

import { afterEach, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
  compact as engineCompact,
  type CompactionPreparation,
  type SessionEntry,
} from "@oh-my-pi/pi-agent-core";
import { createMockModel, registerMockApi, type MockResponse } from "@oh-my-pi/pi-ai/providers/mock";
import type { AssistantMessage, Context, Model } from "@oh-my-pi/pi-ai";
import { createSeatAgent } from "./seat.ts";
import { SeatStore } from "./store.ts";
import { withSeatCommand } from "./seat_command.ts";

registerMockApi();

const modelWindow = 1_000_000;
const bound180k = 180_000;
const chunkText = "the quick brown fox jumps over the lazy dog. ".repeat(1200);

async function fakeSummary(model: Model): Promise<AssistantMessage> {
  return {
    role: "assistant",
    content: [{ type: "text", text: "SUMMARY OF EARLIER WORK" }],
    api: model.api,
    provider: model.provider,
    model: model.id,
    usage: { input: 0, output: 10, cacheRead: 0, cacheWrite: 0, totalTokens: 10, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
    stopReason: "stop",
    timestamp: Date.now(),
  } as AssistantMessage;
}

function agentWithMaintenance(
  handler: (context: Context) => MockResponse,
  maintenance: Parameters<typeof createSeatAgent>[0]["maintenance"] = {},
  extra: Partial<Parameters<typeof createSeatAgent>[0]> = {},
) {
  const mock = createMockModel({ contextWindow: modelWindow, handler: (context) => handler(context) });
  return createSeatAgent({
    provider: "mock",
    model: "mock-model",
    token: "t",
    cwd: process.cwd(),
    emit: () => {},
    callTool: async () => "",
    modelOverride: mock as never,
    maintenance: {
      compactImpl: engineCompact,
      summaryOptions: { completeImpl: (async (model: Model) => fakeSummary(model)) as never },
      ...maintenance,
    },
    ...extra,
  });
}

const dirs: string[] = [];
function tempDir(): string {
  const d = mkdtempSync(join(tmpdir(), "t244-"));
  dirs.push(d);
  return d;
}
afterEach(() => {
  for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true });
});

describe("bound compact (🎯T244)", () => {
  test("compact refuses before mutation while a turn is open", async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    const agent = agentWithMaintenance(
      async () => {
        await gate;
        return { content: ["ok"], usage: { input: 1000, output: 1 } };
      },
      {
        compactImpl: (async () => {
          throw new Error("compact must not run during turn");
        }) as typeof engineCompact,
      },
    );
    const turn = agent.prompt("hold the turn open");
    await expect(agent.compactAtBound(50_000)).rejects.toMatchObject({ reason: "turn_in_flight" });
    release();
    await turn;
  });

  test("already under bound returns sizes without committing", async () => {
    let compactions = 0;
    const agent = agentWithMaintenance(() => ({ content: ["ok"], usage: { input: 1000, output: 1 } }), {
      compactImpl: (async () => {
        compactions++;
        return { summary: "folded", tokensBefore: 1, firstKeptEntryId: "x", details: {} };
      }) as typeof engineCompact,
    });
    await agent.prompt("small");
    const result = await agent.compactAtBound(200_000);
    expect(result.committed).toBe(false);
    expect(result.tokensBefore).toBe(result.tokensAfter);
    expect(compactions).toBe(0);
  });

  test("folds above 180K below model window with committed before/after", async () => {
    const agent = agentWithMaintenance(() => ({ content: ["ok"], usage: { input: 1000, output: 1 } }));
    for (let i = 0; i < 16; i++) await agent.prompt(`${i}: ${chunkText}`);
    const result = await agent.compactAtBound(bound180k);
    expect(result.committed).toBe(true);
    expect(result.tokensBefore).toBeGreaterThan(bound180k);
    expect(result.tokensAfter).toBeLessThanOrEqual(bound180k);
    expect(result.tokensAfter).toBeLessThan(result.tokensBefore);
  });

  test("no_eligible_fold when compaction cannot reach the bound", async () => {
    const agent = agentWithMaintenance(() => ({ content: ["ok"], usage: { input: 1000, output: 1 } }), {
      compactImpl: (async () => {
        throw new Error("engine refused to fold");
      }) as typeof engineCompact,
    });
    for (let i = 0; i < 16; i++) await agent.prompt(`${i}: ${chunkText}`);
    await expect(agent.compactAtBound(bound180k)).rejects.toMatchObject({ reason: "no_eligible_fold" });
  });

  test("pins and durable store survive bound compact and reload", async () => {
    const root = tempDir();
    const store = new SeatStore(root, "po", "s1");
    const pin = "the owner ships MINOR releases only";
    const agent = agentWithMaintenance(() => ({ content: ["ok"], usage: { input: 1000, output: 1 } }), {}, {
      store,
      pins: [pin],
    });
    for (let i = 0; i < 16; i++) await agent.prompt(`${i}: ${chunkText}`);
    const folded = await agent.compactAtBound(bound180k);
    expect(folded.committed).toBe(true);

    let seen = "";
    const reloaded = agentWithMaintenance(
      (context) => {
        seen = JSON.stringify(context.messages);
        return { content: ["ok"], usage: { input: 1000, output: 1 } };
      },
      {},
      { store },
    );
    await reloaded.prompt("after reload");
    expect(seen).toContain("SUMMARY OF EARLIER WORK");
    expect(seen).toContain(pin);
    const entries = store.load() as SessionEntry[];
    expect(entries.some((e) => e.type === "compaction")).toBe(true);
  });

  test("abort during bound compact leaves replayable history without a new compaction entry", async () => {
    const root = tempDir();
    const store = new SeatStore(root, "po", "s1");
    const agent = agentWithMaintenance(() => ({ content: ["ok"], usage: { input: 1000, output: 1 } }), {
      compactImpl: (async (_prep: CompactionPreparation, _model: unknown, _token: unknown, _instructions: unknown, signal?: AbortSignal) => {
        await new Promise<void>((_, reject) => {
          if (signal?.aborted) {
            reject(new Error("aborted"));
            return;
          }
          signal?.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
        });
      }) as typeof engineCompact,
    }, { store });
    for (let i = 0; i < 16; i++) await agent.prompt(`${i}: ${chunkText}`);
    const entriesBefore = (store.load() as SessionEntry[]).length;
    const compacting = agent.compactAtBound(bound180k);
    await Bun.sleep(20);
    agent.abort();
    await expect(compacting).rejects.toBeDefined();
    const entriesAfter = store.load() as SessionEntry[];
    expect(entriesAfter.length).toBe(entriesBefore);
    expect(entriesAfter.some((e) => e.type === "compaction")).toBe(false);
    await agent.prompt("still usable");
  });

  test("withSeatCommand serializes concurrent seat handlers", async () => {
    const seat = "jv-serial";
    const order: string[] = [];
    const a = withSeatCommand(seat, async () => {
      order.push("a-start");
      await Bun.sleep(5);
      order.push("a-end");
    });
    const b = withSeatCommand(seat, async () => {
      order.push("b-start");
      order.push("b-end");
    });
    await Promise.all([a, b]);
    expect(order).toEqual(["a-start", "a-end", "b-start", "b-end"]);
  });
});
