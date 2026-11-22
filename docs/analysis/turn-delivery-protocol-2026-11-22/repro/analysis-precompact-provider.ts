// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Analysis fixture, 2026-11-22 (defect P2). Copied into sidecar/testdata/ by
// run_sidecar_repros.sh. The real server.ts on a mock model with a 240k-token
// window, so a heavy store makes the pre-turn compaction due. The first
// compaction is held until aborted (CLAUDIA_TEST_COMPACT_HOLD); SIGUSR2 lifts
// the hold for every later pass, so a turn that starts afterwards can finish.
// No provider transport is called.
import assert from "node:assert/strict";
import { connect } from "node:net";
import { writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import { createMockModel, MockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";

globalThis.fetch = (async () => { throw new Error("unexpected fixture fetch"); }) as typeof fetch;
registerMockApi();
const handler = (context: { messages: { role?: string; content?: unknown }[] }) => {
  const last = context.messages.filter((m) => m.role === "user").at(-1);
  assert.ok(last);
  const text = typeof last.content === "string" ? last.content
    : (last.content as { type?: string; text?: string }[]).filter((c) => c.type === "text").map((c) => c.text).join("");
  const exact = /Reply with exactly:\s*(\S+)/.exec(text);
  if (exact) return { content: [exact[1]], usage: { input: 1_000, output: 1 } };
  return { content: ["fixture summary of the earlier conversation"], usage: { input: 1_000, output: 1 } };
};
const model = createMockModel({ provider: "anthropic", id: "claude-sonnet-5", contextWindow: 240_000, handler });
const catalog = getBundledModel("anthropic", model.id);
assert.ok(catalog, "fixture requires the exact pinned catalog row");
Object.assign(catalog, model);
Object.setPrototypeOf(catalog, MockModel.prototype);
Object.defineProperty(catalog, "calls", { value: model.calls, enumerable: false });
writeFileSync(join(dirname(process.argv[2]), "mock-api-ready"), "mock\n");
process.env.CLAUDIA_TEST_COMPACT_HOLD = "1";
process.env.CLAUDIA_TEST_COMPACT_SOFT_ONLY = "1";
process.on("SIGUSR2", () => { delete process.env.CLAUDIA_TEST_COMPACT_HOLD; });
await import("../server.ts");
await new Promise<void>((resolve, reject) => {
  const probe = connect(process.argv[2]);
  probe.once("error", reject);
  probe.once("connect", () => { probe.end(); resolve(); });
});
console.log("analysis-precompact-ready");
