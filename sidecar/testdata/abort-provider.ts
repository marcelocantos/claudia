// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Test-only entrypoint for abort-turn.test.ts: replace the pinned catalog row
// with the registered mock API, then import the unchanged production server.
// No provider transport is called; the seat, socket and turn loop are real.
//
// A prompt containing HOLD waits (honouring the abort signal) and so never
// answers on its own; a prompt containing FAIL ends as a provider error.
// Anything else is answered at once with "reply: <its text>".
import assert from "node:assert/strict";
import { connect } from "node:net";
import { writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import { createMockModel, MockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";

globalThis.fetch = (async () => { throw new Error("unexpected fixture fetch"); }) as typeof fetch;
registerMockApi();
const holdMs = 10 * 60 * 1000;
function unpairedToolCall(messages: { role?: string; content?: unknown }[]): boolean {
  let pending = false;
  for (const m of messages) {
    if (m.role === "assistant" && Array.isArray(m.content) && m.content.some((c: { type?: string }) => c?.type === "toolCall")) pending = true;
    if (m.role === "toolResult") pending = false;
  }
  return pending;
}
const model = createMockModel({ provider: "anthropic", id: "claude-sonnet-5", handler: context => {
  const users = context.messages.filter(message => message.role === "user");
  const last = users.at(-1);
  assert.ok(last && last.role === "user");
  const text = typeof last.content === "string" ? last.content : last.content.filter(c => c.type === "text").map(c => c.text).join("");
  if (text.startsWith("BASH_CANCEL ") && !context.messages.some(m => m.role === "toolResult")) {
    const marker = text.slice("BASH_CANCEL ".length);
    return { content: [{ type: "toolCall", id: "cancel-bash", name: "Bash", arguments: { command: `sleep 60 & child=$!; echo $child > '${marker}'; wait $child`, timeout: 65 } }] };
  }
  // 🎯T236: an unanswered function call is a rejection, never an empty
  // successful stop. Pairing must close the call before this prompt.
  if (unpairedToolCall(context.messages)) {
    return { content: [], stopReason: "stop" };
  }
  const exact = /Reply with exactly:\s*(\S+)/.exec(text);
  if (exact) return { content: [exact[1]] };
  if (text.includes("HOLD")) return { content: ["never"], delayMs: holdMs };
  if (text.includes("FAIL")) return { stopReason: "error", errorMessage: "fixture provider refusal" };
  return { content: [`reply: ${text}`] };
} });
const catalog = getBundledModel("anthropic", model.id);
assert.ok(catalog, "fixture requires the exact pinned catalog row");
Object.assign(catalog, model);
Object.setPrototypeOf(catalog, MockModel.prototype);
Object.defineProperty(catalog, "calls", { value: model.calls, enumerable: false });
assert.ok(getBundledModel("anthropic", model.id) instanceof MockModel);
// Go process fixtures use this to attest mock-provider selection. It is not
// server readiness: the Bun fixture waits for the connection evidence below.
writeFileSync(join(dirname(process.argv[2]), "mock-api-ready"), "mock\n");
await import("../server.ts");
// Importing the server starts its listener. A real connection, rather than a
// marker written before import, proves that the production socket is ready.
await new Promise<void>((resolve, reject) => {
  const probe = connect(process.argv[2]);
  probe.once("error", reject);
  probe.once("connect", () => { probe.end(); resolve(); });
});
console.log("abort-fixture-ready");
