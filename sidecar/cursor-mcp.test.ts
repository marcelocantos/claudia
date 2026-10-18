// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Cursor runs a listed seat tool as an MCP exec frame on the open stream.
// The seat must execute that tool and return its text on the same frame.
// Answering tool-not-found leaves the host with no tool_call (2026-10-18).

import { afterEach, expect, spyOn, test } from "bun:test";
import http2 from "node:http2";
import { EventEmitter } from "node:events";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import {
  type AgentServerMessage,
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

const roots: string[] = [];
let restore: (() => void) | undefined;
afterEach(() => {
  restore?.();
  restore = undefined;
  for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true });
});

function frame(message: AgentServerMessage): Buffer {
  const data = toBinary(AgentServerMessageSchema, message);
  const bytes = Buffer.alloc(5 + data.length);
  bytes[0] = 0;
  bytes.writeUInt32BE(data.length, 1);
  bytes.set(data, 5);
  return bytes;
}

function mcpFrame(name: string, args: Record<string, string>): Buffer {
  const encoded: Record<string, Uint8Array> = {};
  for (const [key, value] of Object.entries(args)) encoded[key] = encodeJsonValue(value);
  return frame(create(AgentServerMessageSchema, {
    message: {
      case: "execServerMessage",
      value: create(ExecServerMessageSchema, {
        id: 7,
        execId: "exec-7",
        message: {
          case: "mcpArgs",
          value: create(McpArgsSchema, {
            name,
            toolName: name,
            toolCallId: "call-1",
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
  constructor(readonly headers: Record<string, unknown>, readonly respond: (request: Request) => void) { super(); }
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

function transport(respond: (request: Request) => void) {
  const requests: Request[] = [];
  const mock = spyOn(http2, "connect").mockImplementation((url: unknown) => {
    expect(String(url)).toBe("https://fixture.invalid");
    const session = new EventEmitter() as EventEmitter & { request: (headers: Record<string, unknown>) => Request; close: () => void };
    session.request = (headers) => {
      expect(headers[":path"]).toBe("/agent.v1.AgentService/Run");
      const request = new Request(headers, respond);
      requests.push(request);
      return request;
    };
    session.close = () => {};
    return session as never;
  });
  restore = () => mock.mockRestore();
  return requests;
}

function seat(callTool: (id: string, name: string, args: string) => Promise<string>) {
  const root = mkdtempSync(join(tmpdir(), "claudia-cursor-mcp-"));
  roots.push(root);
  const bundled = getBundledModel("cursor", "claude-4-sonnet");
  if (!bundled) throw new Error("cursor model missing");
  return createSeatAgent({
    provider: "cursor",
    model: bundled.id,
    token: "fixture",
    cwd: root,
    emit: () => {},
    callTool,
    tools: [{
      name: "jevons_idea_capture",
      description: "Record one idea.",
      input_schema: { type: "object", properties: { text: { type: "string" }, source: { type: "string" } } },
    }],
    modelOverride: { ...bundled, baseUrl: "https://fixture.invalid" },
  });
}

test("a cursor MCP frame runs the listed host tool and returns its text on the exec channel", async () => {
  const hostAnswer = `idea-${crypto.randomUUID()}`;
  const calls: { name: string; args: string }[] = [];
  const trace: string[] = [];
  const requests = transport((request) => {
    request.onMcp = (result) => {
      const text = result.result.case === "success" ? mcpResultText(result) : result.result.case;
      trace.push(`wire:${result.result.case}:${text}`);
      queueMicrotask(() => request.finish());
    };
    request.push(mcpFrame("jevons_idea_capture", { text: "probe-idea", source: "mcp" }));
  });
  const agent = seat(async (_id, name, args) => {
    calls.push({ name, args });
    trace.push(`call:${name}:${args}`);
    return hostAnswer;
  });
  await agent.prompt("capture one idea");
  expect(calls).toEqual([{ name: "jevons_idea_capture", args: JSON.stringify({ text: "probe-idea", source: "mcp" }) }]);
  expect(trace[0]).toStartWith("call:jevons_idea_capture:");
  expect(trace[1]).toBe(`wire:success:${hostAnswer}`);
  expect(requests).toHaveLength(1);
});

test("a cursor MCP frame for an unlisted tool does not call the host", async () => {
  const calls: string[] = [];
  let wire = "";
  transport((request) => {
    request.onMcp = (result) => {
      wire = result.result.case;
      queueMicrotask(() => request.finish());
    };
    request.push(mcpFrame("jevons_not_listed", { text: "nope" }));
  });
  const agent = seat(async (_id, name) => {
    calls.push(name);
    return "should-not-run";
  });
  await agent.prompt("capture one idea");
  expect(calls).toEqual([]);
  expect(wire).toBe("error");
});
