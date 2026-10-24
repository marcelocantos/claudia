import { expect, test } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { createMockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";
import { createSeatAgent, type SeatEvent } from "./seat.ts";
import { cursorExecHandlers, type CursorExecVisibility } from "./cursorexec.ts";
import { encodeSidecarWireLine } from "./tool-visibility.ts";

registerMockApi();

test("seat tool_end wire lines carry terminal status without fabricating success", async () => {
  let n = 0;
  const mock = createMockModel({
    contextWindow: 100_000,
    handler: () => {
      n++;
      if (n === 1) {
        return {
          content: [{ type: "toolCall", name: "Read", arguments: { path: "package.json" } }],
          usage: { input: 10, output: 1 },
        };
      }
      return { content: ["done"], usage: { input: 10, output: 1 } };
    },
  });
  const lines: string[] = [];
  const agent = createSeatAgent({
    provider: "mock",
    model: "mock-model",
    token: "t",
    cwd: process.cwd(),
    emit: (ev) => {
      if (ev.type === "tool_start" || ev.type === "tool_end") lines.push(encodeSidecarWireLine(ev));
    },
    callTool: async () => "ok",
    modelOverride: mock as never,
  });
  await agent.prompt("go");
  const parsed = lines.map((l) => JSON.parse(l.trim()) as SeatEvent);
  const end = parsed.find((e) => e.type === "tool_end");
  expect(end?.status).toBe("completed");
  const start = parsed.find((e) => e.type === "tool_start");
  expect(end?.call_id).toBe(start?.call_id);
});

test("native shell without toolCallId still pairs start/end on the wire", async () => {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "wire-pump-")));
  const lines: string[] = [];
  const visibility: CursorExecVisibility = {
    toolStart(call_id, name, text) {
      lines.push(encodeSidecarWireLine({ type: "tool_start", call_id, name, text }));
    },
    toolEnd(call_id, name, text, status) {
      lines.push(encodeSidecarWireLine({ type: "tool_end", call_id, name, text, status }));
    },
  };
  const h = cursorExecHandlers(() => dir, () => 100_000, undefined, undefined, undefined, () => visibility) as any;
  await h.shell({ command: "true", timeout: 5 });
  const parsed = lines.map((l) => JSON.parse(l.trim()));
  expect(parsed[0].type).toBe("tool_start");
  expect(parsed[1].type).toBe("tool_end");
  expect(parsed[1].call_id).toBe(parsed[0].call_id);
  expect(parsed[1].status).toBe("completed");
});

test("visibility callback exceptions do not change tool results or re-run effects", async () => {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "vis-safe-")));
  let runs = 0;
  const visibility: CursorExecVisibility = {
    toolStart() {
      throw new Error("observability blew up");
    },
    toolEnd() {
      throw new Error("observability blew up");
    },
  };
  const h = cursorExecHandlers(() => dir, () => 100_000, undefined, undefined, undefined, () => visibility) as any;
  const msg = await h.shell({ command: "echo once", timeout: 5 });
  runs++;
  expect(runs).toBe(1);
  expect(msg.isError).not.toBe(true);
  const text = (msg.content?.[0] as { text?: string })?.text ?? "";
  expect(text.includes("once")).toBe(true);
});
