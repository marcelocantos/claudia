// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// 🎯T224 bounded wire repro: steer during an in-flight native Bash tool.
import assert from "node:assert/strict";
import { appendFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import { createMockModel, MockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";

const prefix = "STEER_MIDSTREAM_FIXTURE ";
const steerNeedle = "STEER_INSTRUCTION_T224";

globalThis.fetch = (async () => { throw new Error("unexpected fixture fetch"); }) as typeof fetch;
registerMockApi();

function bashHold(dir: string): string {
  const release = join(dir, "release");
  const entered = join(dir, "tool-entered");
  return `touch ${entered}; while [ ! -f ${release} ]; do sleep 0.05; done; echo tool-done`;
}

const model = createMockModel({ provider: "anthropic", id: "claude-sonnet-5", handler: context => {
  const user = context.messages.find(message => message.role === "user");
  assert.ok(user && user.role === "user");
  const text = typeof user.content === "string" ? user.content : user.content.filter(c => c.type === "text").map(c => c.text).join("");
  assert.ok(text.startsWith(prefix));
  const spec = JSON.parse(text.slice(prefix.length)) as { dir: string };
  const log = join(spec.dir, "events.jsonl");
  const results = context.messages.filter(message => message.role === "toolResult");
  const steerInContext = JSON.stringify(context.messages).includes(steerNeedle);
  appendFileSync(log, JSON.stringify({ ts: Date.now(), modelCall: model.calls.length, steerInContext, toolResults: results.length }) + "\n");
  if (results.length) {
    return { content: ["steer-midstream-complete"] };
  }
  return { content: [{ type: "toolCall", id: "sm-call", name: "Bash", arguments: { command: bashHold(spec.dir) } }] };
} });
const catalog = getBundledModel("anthropic", model.id);
assert.ok(catalog, "fixture requires the exact pinned catalog row");
Object.assign(catalog, model);
Object.setPrototypeOf(catalog, MockModel.prototype);
Object.defineProperty(catalog, "calls", { value: model.calls, enumerable: false });
writeFileSync(join(dirname(process.argv[2]), "mock-api-ready"), "mock\n");
await import("../server.ts");
