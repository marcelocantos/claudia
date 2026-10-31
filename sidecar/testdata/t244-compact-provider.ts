// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import assert from "node:assert/strict";
import { connect } from "node:net";
import { writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import { createMockModel, MockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";

globalThis.fetch = (async () => { throw new Error("unexpected fixture fetch"); }) as typeof fetch;
registerMockApi();

const handler = (context: any) => {
  const users = context.messages.filter((m: { role?: string }) => m.role === "user");
  const last = users.at(-1);
  assert.ok(last && last.role === "user");
  const text = typeof last.content === "string" ? last.content : last.content.filter((c: { type?: string }) => c.type === "text").map((c: { text?: string }) => c.text).join("");
  return { content: [`reply: ${text.slice(0, 32)}`], usage: { input: 12_000, output: 1 } };
};

const model = createMockModel({ provider: "anthropic", id: "claude-sonnet-5", contextWindow: 1_000_000, handler });
const catalog = getBundledModel("anthropic", model.id);
assert.ok(catalog, "fixture requires the exact pinned catalog row");
Object.assign(catalog, model);
Object.setPrototypeOf(catalog, MockModel.prototype);
Object.defineProperty(catalog, "calls", { value: model.calls, enumerable: false });
assert.ok(getBundledModel("anthropic", model.id) instanceof MockModel);
writeFileSync(join(dirname(process.argv[2]), "mock-api-ready"), "mock\n");
await import("../server.ts");
await new Promise<void>((resolve, reject) => {
  const probe = connect(process.argv[2]);
  probe.once("error", reject);
  probe.once("connect", () => { probe.end(); resolve(); });
});
console.log("t244-compact-fixture-ready");
