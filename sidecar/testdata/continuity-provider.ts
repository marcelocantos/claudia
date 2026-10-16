// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Test-only entrypoint: replace the selected catalog row with the registered
// mock API before dynamically importing the unchanged production server. No
// provider transport is called; tools and the complete turn loop stay real.
import assert from "node:assert/strict";
import { appendFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import { createMockModel, MockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";

// This is defense in depth, not the egress isolation: positively replacing the
// model API below is what prevents native provider transports being selected.
globalThis.fetch = (async () => { throw new Error("unexpected fixture fetch"); }) as typeof fetch;
registerMockApi();
const model = createMockModel({ provider: "anthropic", id: "claude-sonnet-5", handler: context => {
  const user = context.messages.find(message => message.role === "user");
  assert.ok(user && user.role === "user");
  const text = typeof user.content === "string" ? user.content : user.content.filter(c => c.type === "text").map(c => c.text).join("");
  assert.ok(text.startsWith("CONTINUITY_FIXTURE "));
  const spec = JSON.parse(text.slice("CONTINUITY_FIXTURE ".length));
  const results = context.messages.filter(message => message.role === "toolResult");
  appendFileSync(spec.calls, results.length ? "result\n" : "call\n");
  assert.ok(model.calls.length <= 2, "unexpected extra model invocation");
  if (results.length) {
    assert.equal(results.length, 1);
    assert.ok(JSON.stringify(results[0]).includes("continuity-tool-result"), JSON.stringify(results));
    return { content: ["continuity-complete"] };
  }
  assert.ok(context.tools?.some(tool => tool.name === spec.tool), "tool was not offered");
  return { content: [{ type: "toolCall", id: "continuity-call", name: spec.tool, arguments: spec.args }] };
} });
const catalog = getBundledModel("anthropic", model.id);
assert.ok(catalog, "fixture requires the exact pinned catalog row");
Object.assign(catalog, model);
Object.setPrototypeOf(catalog, MockModel.prototype);
// Mock call history contains the mutable Context itself; it is diagnostic
// state, not model metadata, and must not enter real server snapshots.
Object.defineProperty(catalog, "calls", { value: model.calls, enumerable: false });
assert.equal(getBundledModel("anthropic", model.id).api, "mock");
assert.ok(getBundledModel("anthropic", model.id) instanceof MockModel);
writeFileSync(join(dirname(process.argv[2]), "mock-api-ready"), "mock\n");
await import("../server.ts");
