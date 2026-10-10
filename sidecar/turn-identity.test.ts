import { expect, test } from "bun:test";
import { createMockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";
import { createSeatAgent, type SeatEvent } from "./seat.ts";

registerMockApi();

test("sidecar stamps streamed text and turn_end with the prompt turn_id", async () => {
  const events: SeatEvent[] = [];
  const model = createMockModel({
    contextWindow: 100_000,
    handler: () => ({ content: ["hello from turn"], usage: { input: 1, output: 1 } }),
  });
  const seat = createSeatAgent({
    provider: "mock", model: "mock-model", token: "t", cwd: process.cwd(),
    emit: (ev) => events.push(ev), callTool: async () => "unused", modelOverride: model as never,
  });
  await seat.prompt("hello", { turn_id: "owner-turn-42" });
  const answers = events.filter((ev) => ev.type === "text" || ev.type === "turn_end");
  expect(answers.some((ev) => ev.type === "text")).toBe(true);
  expect(answers.some((ev) => ev.type === "turn_end")).toBe(true);
  expect(answers.every((ev) => ev.turn_id === "owner-turn-42")).toBe(true);
});
