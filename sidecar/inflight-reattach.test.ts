// 🎯T202: a seat reattached mid-turn must report that turn open from the
// very first read — not only once a later accepted/receipt event names
// one. seat.ts's openTurnID() is what server.ts's adopt and
// load-of-an-already-running-seat ready replies now carry as turn_id.

import { expect, test } from "bun:test";
import { createMockModel, registerMockApi, type MockResponse } from "@oh-my-pi/pi-ai/providers/mock";
import type { Context } from "@oh-my-pi/pi-ai";
import { HostCalls } from "./hostcalls.ts";
import { createSeatAgent, type SeatEvent } from "./seat.ts";

registerMockApi();

async function waitFor(pred: () => boolean, ms = 3000): Promise<void> {
  const start = Date.now();
  while (!pred()) {
    if (Date.now() - start > ms) throw new Error("timeout waiting for condition");
    await Bun.sleep(10);
  }
}

test("openTurnID names the running turn while a host tool call is outstanding, and clears once the turn ends", async () => {
  const calls = new HostCalls();
  const events: SeatEvent[] = [];
  let callsToModel = 0;
  const mock = createMockModel({
    contextWindow: 100_000,
    handler: (_context: Context): MockResponse => {
      callsToModel++;
      if (callsToModel === 1) {
        return { content: [{ type: "toolCall", name: "jevons_wait", arguments: {} }], usage: { input: 10, output: 1 } };
      }
      return { content: ["done"], usage: { input: 10, output: 1 } };
    },
  });
  const agent = createSeatAgent({
    provider: "mock",
    model: "mock-model",
    token: "t",
    cwd: process.cwd(),
    emit: (ev) => events.push(ev),
    callTool: (id) => calls.open(id, "seat"),
    tools: [{ name: "jevons_wait", description: "wait on the host", input_schema: { type: "object" } }],
    modelOverride: mock as never,
  });

  // Before any prompt, there is no turn to name.
  expect(agent.openTurnID()).toBeUndefined();

  const turn = agent.prompt("do the thing");
  await waitFor(() => calls.size === 1);

  // This is the fact a reattach must be able to read immediately: the turn
  // is open, and it has an id, while the host tool call is still pending —
  // exactly the window a reconnecting host would otherwise see as idle.
  const openID = agent.openTurnID();
  expect(openID).toBeDefined();
  expect(typeof openID).toBe("string");
  expect((openID as string).length).toBeGreaterThan(0);

  // Fail the outstanding call so the turn can close (we don't know its
  // generated call id here; failAll is the available way to unblock it).
  calls.failAll("done waiting");
  await turn;

  expect(events.some((e) => e.type === "turn_end")).toBe(true);
  expect(agent.openTurnID()).toBeUndefined();
});
