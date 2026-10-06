// 🎯T171: a sidecar seat publishes its own tool calls (Bash, Read, Glob,
// Grep) to the host, so a long Bash call is visible while it runs. Host
// tools are not re-published: the host already knows it is running them.

import { expect, test } from "bun:test";
import { createMockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";
import { createSeatAgent, type SeatEvent } from "./seat.ts";

registerMockApi();

test("own tool calls emit tool_start; host tool calls do not", async () => {
  let n = 0;
  const mock = createMockModel({
    contextWindow: 100_000,
    handler: () => {
      n++;
      if (n === 1) {
        return {
          content: [
            { type: "toolCall", name: "Read", arguments: { path: "package.json" } },
            { type: "toolCall", name: "jevons_host", arguments: {} },
          ],
          usage: { input: 10, output: 1 },
        };
      }
      return { content: ["done"], usage: { input: 10, output: 1 } };
    },
  });
  const events: SeatEvent[] = [];
  const agent = createSeatAgent({
    provider: "mock",
    model: "mock-model",
    token: "t",
    cwd: process.cwd(),
    emit: (ev) => events.push(ev),
    callTool: async () => "ok",
    tools: [{ name: "jevons_host", description: "a host tool", input_schema: { type: "object" } }],
    modelOverride: mock as never,
  });
  await agent.prompt("go");

  const starts = events.filter((e) => e.type === "tool_start");
  expect(starts.length).toBe(1);
  expect(starts[0].name).toBe("Read");
  expect(starts[0].call_id).not.toBe("");
  expect(JSON.parse(starts[0].text as string)).toEqual({ path: "package.json" });
  // The start precedes the turn's end.
  const startAt = events.indexOf(starts[0]);
  const endAt = events.findIndex((e) => e.type === "turn_end");
  expect(startAt).toBeLessThan(endAt);
});
