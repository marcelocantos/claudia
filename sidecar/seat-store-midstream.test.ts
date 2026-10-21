// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// 🎯T206: within one Cursor streaming request, each exec frame's messages
// survive a mid-stream kill — not only at the next model-call boundary.

import { afterEach, expect, spyOn, test } from "bun:test";
import http2 from "node:http2";
import { EventEmitter } from "node:events";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { SessionEntry } from "@oh-my-pi/pi-agent-core";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import {
  AgentClientMessageSchema,
  AgentServerMessageSchema,
  ExecServerMessageSchema,
  InteractionUpdateSchema,
  McpArgsSchema,
  type McpResult,
  TurnEndedUpdateSchema,
} from "@oh-my-pi/pi-catalog/discovery/cursor-proto";
import { create, encodeJsonValue, fromBinary, toBinary } from "@oh-my-pi/pi-catalog/discovery/protobuf";
import { createSeatAgent } from "./seat.ts";
import { SeatStore } from "./store.ts";

const roots: string[] = [];
let restore: (() => void) | undefined;
afterEach(() => {
  restore?.();
  restore = undefined;
  for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true });
});

function frame(message: ReturnType<typeof create<typeof AgentServerMessageSchema>>): Buffer {
  const data = toBinary(AgentServerMessageSchema, message);
  const bytes = Buffer.alloc(5 + data.length);
  bytes[0] = 0;
  bytes.writeUInt32BE(data.length, 1);
  bytes.set(data, 5);
  return bytes;
}

function mcpFrame(name: string, toolCallId: string, args: Record<string, string>): Buffer {
  const encoded: Record<string, Uint8Array> = {};
  for (const [key, value] of Object.entries(args)) encoded[key] = encodeJsonValue(value);
  return frame(create(AgentServerMessageSchema, {
    message: {
      case: "execServerMessage",
      value: create(ExecServerMessageSchema, {
        id: 7,
        execId: `exec-${toolCallId}`,
        message: {
          case: "mcpArgs",
          value: create(McpArgsSchema, {
            name,
            toolName: name,
            toolCallId,
            providerIdentifier: "pi-agent",
            args: encoded,
          }),
        },
      }),
    },
  }));
}

const turnEnded = frame(create(AgentServerMessageSchema, {
  message: {
    case: "interactionUpdate",
    value: create(InteractionUpdateSchema, {
      message: { case: "turnEnded", value: create(TurnEndedUpdateSchema, {}) },
    }),
  },
}));

function mcpResultText(result: McpResult): string {
  if (result.result.case !== "success") return "";
  return result.result.value.content.map((item) => item.content?.case === "text" ? item.content.value.text : "").join("");
}

class Request extends EventEmitter {
  closed = false;
  writes = 0;
  onMcp: ((result: McpResult) => void) | undefined;
  constructor(readonly respond: (request: Request) => void) { super(); }
  write(bytes: Uint8Array) {
    expect(bytes[0]).toBe(0);
    this.writes++;
    if (this.writes === 1) queueMicrotask(() => this.respond(this));
    else this.note(bytes);
    return true;
  }
  note(bytes: Uint8Array) {
    const msg = fromBinary(AgentClientMessageSchema, bytes.subarray(5));
    if (msg.message.case !== "execClientMessage") return;
    const exec = msg.message.value.message;
    if (exec.case !== "mcpResult" || !this.onMcp) return;
    const deliver = this.onMcp;
    this.onMcp = undefined;
    deliver(exec.value);
  }
  push(payload: Buffer) { this.emit("data", payload); }
  finish() {
    if (this.closed) return;
    this.push(turnEnded);
    this.closed = true;
    this.emit("end");
  }
  close() { this.finish(); }
}

function messageRolesOnDisk(store: SeatStore): string[] {
  const entries = (store.load() ?? []) as SessionEntry[];
  return entries.filter(e => e.type === "message").map(e => (e as { message: { role: string } }).message.role);
}

function seatWithStore(root: string, store: SeatStore, callTool: (id: string, name: string, args: string) => Promise<string>) {
  const bundled = getBundledModel("cursor", "claude-4-sonnet");
  if (!bundled) throw new Error("cursor model missing");
  return createSeatAgent({
    provider: "cursor",
    model: bundled.id,
    token: "fixture",
    cwd: root,
    emit: () => {},
    callTool,
    store,
    tools: [{
      name: "jevons_idea_capture",
      description: "Record one idea.",
      input_schema: { type: "object", properties: { text: { type: "string" }, source: { type: "string" } } },
    }],
    modelOverride: { ...bundled, baseUrl: "https://fixture.invalid" },
  });
}

test("cursor exec frames within one stream persist before the turn ends and survive a mid-stream kill", async () => {
  const root = mkdtempSync(join(tmpdir(), "t206-midstream-"));
  roots.push(root);
  const store = new SeatStore(root, "worker", "session");
  let mcpRound = 0;
  let checkpoint: string[] | undefined;
  let secondGate!: () => void;
  const secondArrived = new Promise<void>(resolve => { secondGate = resolve; });

  const mock = spyOn(http2, "connect").mockImplementation(() => {
    const session = new EventEmitter() as EventEmitter & { request: () => Request; close: () => void };
    session.request = () => {
      const request = new Request((r) => {
        r.onMcp = (result) => {
          mcpRound++;
          expect(result.result.case).toBe("success");
          if (mcpRound === 1) {
            checkpoint = messageRolesOnDisk(store);
            r.push(mcpFrame("jevons_idea_capture", "call-2", { text: "second", source: "mcp" }));
            secondGate();
            return;
          }
          queueMicrotask(() => r.finish());
        };
        r.push(mcpFrame("jevons_idea_capture", "call-1", { text: "first", source: "mcp" }));
      });
      return request;
    };
    session.close = () => {};
    return session as never;
  });
  restore = () => mock.mockRestore();

  const agent = seatWithStore(root, store, async () => "idea-body");
  const running = agent.prompt("capture ideas").catch(() => {});
  await secondArrived;

  expect(checkpoint).toEqual(["user", "assistant", "toolResult"]);

  const killedRoles = messageRolesOnDisk(store);
  expect(killedRoles).toEqual(["user", "assistant", "toolResult"]);

  const bundled = getBundledModel("cursor", "claude-4-sonnet");
  if (!bundled) throw new Error("cursor model missing");
  const resumed = createSeatAgent({
    provider: "cursor",
    model: bundled.id,
    token: "fixture",
    cwd: root,
    emit: () => {},
    callTool: async () => "idea-body",
    store,
    requireResume: true,
    tools: [{
      name: "jevons_idea_capture",
      description: "Record one idea.",
      input_schema: { type: "object", properties: { text: { type: "string" }, source: { type: "string" } } },
    }],
    modelOverride: { ...bundled, baseUrl: "https://fixture.invalid" },
  });
  expect(resumed.restored).toBe(true);
  expect(messageRolesOnDisk(store)).toEqual(killedRoles);
  expect(readFileSync(store.path, "utf8").includes("idea-body")).toBe(true);

  agent.abort();
  await running;
});
