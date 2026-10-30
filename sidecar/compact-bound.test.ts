// 🎯T244: synchronous bound compaction serializes against in-flight turns.

import { describe, expect, test } from "bun:test";
import { compact as engineCompact, type CompactionPreparation } from "@oh-my-pi/pi-agent-core";
import { createMockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";
import { createSeatAgent } from "./seat.ts";
import { withSeatCommand } from "./seat_command.ts";

registerMockApi();

describe("bound compact (🎯T244)", () => {
  test("compact refuses before mutation while a turn is open", async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    const mock = createMockModel({
      contextWindow: 100_000,
      handler: async () => {
        await gate;
        return { content: ["ok"], usage: { input: 1000, output: 1 } };
      },
    });
    const agent = createSeatAgent({
      provider: "mock",
      model: "mock-model",
      token: "t",
      cwd: process.cwd(),
      emit: () => {},
      callTool: async () => "",
      modelOverride: mock as never,
      maintenance: {
        compactImpl: (async () => {
          throw new Error("compact must not run during turn");
        }) as typeof engineCompact,
      },
    });
    const turn = agent.prompt("hold the turn open");
    await expect(agent.compactAtBound(50_000)).rejects.toMatchObject({ reason: "turn_in_flight" });
    release();
    await turn;
  });

  test("already under bound returns sizes without committing", async () => {
    const mock = createMockModel({
      contextWindow: 100_000,
      handler: () => ({ content: ["ok"], usage: { input: 1000, output: 1 } }),
    });
    let compactions = 0;
    const agent = createSeatAgent({
      provider: "mock",
      model: "mock-model",
      token: "t",
      cwd: process.cwd(),
      emit: () => {},
      callTool: async () => "",
      modelOverride: mock as never,
      maintenance: {
        compactImpl: (async () => {
          compactions++;
          return { summary: "folded", tokensBefore: 1, firstKeptEntryId: "x", details: {} };
        }) as typeof engineCompact,
      },
    });
    await agent.prompt("small");
    const result = await agent.compactAtBound(200_000);
    expect(result.committed).toBe(false);
    expect(result.tokensBefore).toBe(result.tokensAfter);
    expect(compactions).toBe(0);
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
