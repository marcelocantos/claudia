// 🎯T149: a sidecar seat's pending host tool call never outlives the
// connection that issued it. A host restart mid-tool-call cannot wedge
// the seat's turn forever.

import { describe, expect, test } from "bun:test";
import { createMockModel, registerMockApi, type MockResponse } from "@oh-my-pi/pi-ai/providers/mock";
import type { Context } from "@oh-my-pi/pi-ai";
import { HOST_CALL_ABORTED, HOST_CONNECTION_LOST, HostCalls } from "./hostcalls.ts";
import { createSeatAgent, type SeatEvent } from "./seat.ts";

registerMockApi();

async function waitFor(pred: () => boolean, ms = 3000): Promise<void> {
  const start = Date.now();
  while (!pred()) {
    if (Date.now() - start > ms) throw new Error("timeout waiting for condition");
    await Bun.sleep(10);
  }
}

function hangingSeat(opts: {
  calls: HostCalls;
  events: SeatEvent[];
  seat?: string;
  afterTool?: (context: Context) => MockResponse;
}) {
  let callsToModel = 0;
  const mock = createMockModel({
    contextWindow: 100_000,
    handler: (context) => {
      callsToModel++;
      if (callsToModel === 1) {
        return { content: [{ type: "toolCall", name: "jevons_wait", arguments: {} }], usage: { input: 10, output: 1 } };
      }
      return opts.afterTool?.(context) ?? { content: ["recovered"], usage: { input: 10, output: 1 } };
    },
  });
  const seat = opts.seat ?? "po";
  const agent = createSeatAgent({
    provider: "mock",
    model: "mock-model",
    token: "t",
    cwd: process.cwd(),
    emit: (ev) => opts.events.push(ev),
    callTool: (id) => opts.calls.open(id, seat),
    tools: [{ name: "jevons_wait", description: "wait on the host", input_schema: { type: "object" } }],
    modelOverride: mock as never,
  });
  return { agent, modelCalls: () => callsToModel };
}

describe("pending host tool lifetime (🎯T149)", () => {
  test("closing the connection without a tool_result ends the turn, and the next prompt is its own turn", async () => {
    const calls = new HostCalls();
    const events: SeatEvent[] = [];
    const { agent } = hangingSeat({ calls, events });

    const turn = agent.prompt("do the thing");
    await waitFor(() => calls.size === 1);

    // Host connection gone; no tool_result will ever arrive.
    expect(calls.failAll(HOST_CONNECTION_LOST)).toBe(1);
    await turn;

    const ends = events.filter((e) => e.type === "turn_end");
    expect(ends.length).toBe(1);
    expect(ends[0].error).toBeUndefined();

    const before = events.filter((e) => e.type === "turn_end").length;
    await agent.prompt("next");
    const after = events.filter((e) => e.type === "turn_end");
    expect(after.length).toBe(before + 1);
    // A follow-up that never ran would only have emitted "accepted".
    expect(after.at(-1)?.error).toBeUndefined();
  });

  test("rebinding to a new connection fails the old connection's pending call so the turn ends on the new one", async () => {
    const oldCalls = new HostCalls();
    const oldEvents: SeatEvent[] = [];
    const newEvents: SeatEvent[] = [];
    const { agent } = hangingSeat({ calls: oldCalls, events: oldEvents, seat: "po" });

    const turn = agent.prompt("do the thing");
    await waitFor(() => oldCalls.size === 1);

    // Adopt on a new connection: fail this seat's pending tools, then point
    // events and new tool calls at the new connection.
    const newCalls = new HostCalls();
    expect(oldCalls.failSeat("po", HOST_CONNECTION_LOST)).toBe(1);
    agent.rebind((ev) => newEvents.push(ev), (id) => newCalls.open(id, "po"));

    await turn;

    expect(newEvents.some((e) => e.type === "turn_end")).toBe(true);
    expect(oldEvents.some((e) => e.type === "turn_end")).toBe(false);

    const before = newEvents.filter((e) => e.type === "turn_end").length;
    await agent.prompt("next");
    expect(newEvents.filter((e) => e.type === "turn_end").length).toBe(before + 1);
  });

  test("abort unblocks a turn waiting on a host tool, and the next prompt is its own turn", async () => {
    const calls = new HostCalls();
    const events: SeatEvent[] = [];
    const { agent } = hangingSeat({ calls, events });

    const turn = agent.prompt("do the thing");
    await waitFor(() => calls.size === 1);

    agent.abort();
    await turn.catch(() => {});

    // The digest closed the turn; wait until the agent is idle so a
    // follow-up is not the next prompt's fate.
    await waitFor(() => events.some((e) => e.type === "turn" || e.type === "turn_end"));

    const before = events.filter((e) => e.type === "turn_end").length;
    await agent.prompt("next");
    expect(events.filter((e) => e.type === "turn_end").length).toBeGreaterThan(before);
  });

  test("the model sees the lost-connection error as the tool result", async () => {
    const calls = new HostCalls();
    const events: SeatEvent[] = [];
    let seen = "";
    const { agent } = hangingSeat({
      calls,
      events,
      afterTool: (context) => {
        seen = JSON.stringify(context.messages);
        return { content: ["recovered"], usage: { input: 10, output: 1 } };
      },
    });

    const turn = agent.prompt("do the thing");
    await waitFor(() => calls.size === 1);
    calls.failAll(HOST_CONNECTION_LOST);
    await turn;

    expect(seen).toContain(HOST_CONNECTION_LOST);
    expect(events.some((e) => e.type === "turn_end")).toBe(true);
  });
});

// HOST_CALL_ABORTED is the abort path's error text; pin that the abort
// test is about that signal, not a lost connection.
test("abort error names the aborted turn, not a lost connection", () => {
  expect(HOST_CALL_ABORTED).not.toContain("host connection");
  expect(HOST_CONNECTION_LOST).toContain("host connection");
});
