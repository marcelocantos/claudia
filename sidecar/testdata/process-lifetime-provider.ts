// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Hermetic entry for 🎯T219 / 🎯T223: mock API + unchanged production server.
// PROCESS_LIFETIME_FIXTURE prompts run real seat Bash with child+grandchild.
import assert from "node:assert/strict";
import { appendFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import { createMockModel, MockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";

const prefix = "PROCESS_LIFETIME_FIXTURE ";

globalThis.fetch = (async () => { throw new Error("unexpected fixture fetch"); }) as typeof fetch;
registerMockApi();

function bashCommand(spec: { mode: string; dir: string }): string {
  const grand = join(spec.dir, "grand.pid");
  const parent = join(spec.dir, "parent.pid");
  const release = join(spec.dir, "release");
  const effect = join(spec.dir, "effect");
  if (spec.mode === "flood") {
    return `/bin/sleep 120 & echo $! > ${grand}; echo $$ > ${parent}; exec yes`;
  }
  return `/bin/sleep 300 & echo $! > ${grand}; echo $$ > ${parent}; while [ ! -f ${release} ]; do sleep 0.05; done; echo done > ${effect}`;
}

const model = createMockModel({ provider: "anthropic", id: "claude-sonnet-5", handler: context => {
  const user = context.messages.find(message => message.role === "user");
  assert.ok(user && user.role === "user");
  const text = typeof user.content === "string" ? user.content : user.content.filter(c => c.type === "text").map(c => c.text).join("");
  assert.ok(text.startsWith(prefix));
  const spec = JSON.parse(text.slice(prefix.length)) as { mode: string; dir: string };
  const results = context.messages.filter(message => message.role === "toolResult");
  appendFileSync(join(spec.dir, "provider-calls"), results.length ? "result\n" : "call\n");
  assert.ok(model.calls.length <= 2, "unexpected extra model invocation");
  if (results.length) {
    assert.equal(results.length, 1);
    return { content: ["process-lifetime-complete"] };
  }
  assert.ok(context.tools?.some(tool => tool.name === "Bash"), "Bash was not offered");
  return { content: [{ type: "toolCall", id: "pl-call", name: "Bash", arguments: { command: bashCommand(spec) } }] };
} });
const catalog = getBundledModel("anthropic", model.id);
assert.ok(catalog, "fixture requires the exact pinned catalog row");
Object.assign(catalog, model);
Object.setPrototypeOf(catalog, MockModel.prototype);
Object.defineProperty(catalog, "calls", { value: model.calls, enumerable: false });
assert.equal(getBundledModel("anthropic", model.id).api, "mock");
assert.ok(getBundledModel("anthropic", model.id) instanceof MockModel);
writeFileSync(join(dirname(process.argv[2]), "mock-api-ready"), "mock\n");
await import("../server.ts");
