// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// 🎯T224 decisive ordering: tool1 completes, steer delivered, then tool2 in one stream.
import assert from "node:assert/strict";
import { appendFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import { createMockModel, MockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";

const prefix = "STEER_ORDERING_FIXTURE ";
const steerNeedle = "STEER_INSTRUCTION_T224_ORDERING";

globalThis.fetch = (async () => { throw new Error("unexpected fixture fetch"); }) as typeof fetch;
registerMockApi();

function bashTool1(dir: string): string {
  const release = join(dir, "release");
  const entered = join(dir, "tool1-entered");
  return `touch ${entered}; while [ ! -f ${release} ]; do sleep 0.05; done; echo tool1-done`;
}

function bashTool2(dir: string): string {
  const entered = join(dir, "tool2-entered");
  return `touch ${entered}; echo tool2-done`;
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
  appendFileSync(log, JSON.stringify({
    ts: Date.now(),
    modelCall: model.calls.length,
    steerInContext,
    toolResults: results.length,
    issuingTool2: results.length === 1,
  }) + "\n");
  assert.ok(model.calls.length <= 3, "unexpected extra model invocation");
  if (results.length === 0) {
    return { content: [{ type: "toolCall", id: "ord-t1", name: "Bash", arguments: { command: bashTool1(spec.dir) } }] };
  }
  if (results.length === 1) {
    return { content: [{ type: "toolCall", id: "ord-t2", name: "Bash", arguments: { command: bashTool2(spec.dir) } }] };
  }
  assert.equal(results.length, 2);
  return { content: ["steer-ordering-complete"] };
} });
const catalog = getBundledModel("anthropic", model.id);
assert.ok(catalog, "fixture requires the exact pinned catalog row");
Object.assign(catalog, model);
Object.setPrototypeOf(catalog, MockModel.prototype);
Object.defineProperty(catalog, "calls", { value: model.calls, enumerable: false });
writeFileSync(join(dirname(process.argv[2]), "mock-api-ready"), "mock\n");
await import("../server.ts");
