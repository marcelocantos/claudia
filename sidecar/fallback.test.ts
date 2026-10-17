// An unlisted name is not a tool, including one that carries a consumer
// prefix. The host declares every tool the model may call.

import { expect, test } from "bun:test";
import { createMockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";
import { createSeatAgent } from "./seat.ts";

registerMockApi();

test("an unlisted consumer-prefixed name does not call the host", async () => {
  const calls: string[] = [];
  let turns = 0;
  const mock = createMockModel({
    contextWindow: 100_000,
    handler: () => {
      turns++;
      if (turns === 1) {
        return {
          content: [{ type: "toolCall", name: "jevons_secret", arguments: {} }],
          usage: { input: 10, output: 1 },
        };
      }
      return { content: ["done"], usage: { input: 10, output: 1 } };
    },
  });
  const agent = createSeatAgent({
    provider: "mock",
    model: "mock-model",
    token: "t",
    cwd: process.cwd(),
    emit: () => {},
    callTool: async (_id, name) => {
      calls.push(name);
      return "ok";
    },
    tools: [{ name: "bash", description: "declared", input_schema: { type: "object" } }],
    modelOverride: mock as never,
  });
  let refused: unknown;
  try {
    await agent.prompt("go");
  } catch (err) {
    refused = err;
  }
  expect(calls).toEqual([]);
  if (refused === undefined) {
    // The engine reported the missing tool and asked the model again.
    // A host call would have been the thing it waited on.
    expect(turns).toBeGreaterThanOrEqual(2);
  }
});
