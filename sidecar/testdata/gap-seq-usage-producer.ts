// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Hermetic producer: conversation journal with a stored seq gap (compaction-style).
// Emits facts observed before the gap (for control-plane fixture seeding), poison
// status, and a post-gap createSeatAgent reload + second prompt.
import { readFileSync, writeFileSync } from "node:fs";
import { createMockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";
import { createSeatAgent } from "../seat.ts";
import { SeatStore } from "../store.ts";
import { History } from "../maintain.ts";
import { UsageJournal } from "../usage.ts";
import { join } from "node:path";

registerMockApi();
const root = process.argv[2];
const mode = process.argv[3] ?? "seed";
const store = new SeatStore(join(root, "omp-seats"), "seat", "generation");
const model = createMockModel({ handler: () => ({ content: ["ok"], usage: { input: 11, output: 3 } }) });
const meta = (requestId: string) => ({
  request_id: requestId,
  attempt_id: `attempt-${requestId}`,
  session_id: "generation",
  turn_id: `turn-${requestId}`,
});

if (mode === "prompt-after-gap") {
  const agent = createSeatAgent({
    provider: "mock", model: "mock", token: "fixture", cwd: root, modelOverride: model, store,
    emit: () => {}, callTool: async () => "",
  });
  await agent.prompt("after gap", meta("B"));
  const afterHistory = new History(store);
  afterHistory.restore();
  const errors: string[] = [];
  const journal = new UsageJournal(afterHistory, () => {}, msg => errors.push(msg));
  let pageError = "";
  try {
    journal.page();
  } catch (err) {
    pageError = err instanceof Error ? err.message : String(err);
  }
  console.log(JSON.stringify({
    messages: agent.snapshot().messages.length,
    usage_poisoned: errors.length > 0 || !!pageError,
    page_error: pageError,
  }));
  process.exit(0);
}

const agent = createSeatAgent({
  provider: "mock", model: "mock", token: "fixture", cwd: root, modelOverride: model, store,
  emit: () => {}, callTool: async () => "",
});
await agent.prompt("fixture", meta("A"));
const preGapHistory = new History(store);
preGapHistory.restore();
const beforeGap = new UsageJournal(preGapHistory, () => {}, () => {});
const cpFacts = beforeGap.page().facts;

let text = readFileSync(store.path, "utf8");
text = text.replace('"seq":2', '"seq":4');
writeFileSync(store.path, text);

const gapHistory = new History(store);
gapHistory.restore();
const errors: string[] = [];
const journal = new UsageJournal(gapHistory, () => {}, msg => errors.push(msg));
let pageError = "";
try {
  journal.page();
} catch (err) {
  pageError = err instanceof Error ? err.message : String(err);
}

const reloaded = createSeatAgent({
  provider: "mock", model: "mock", token: "fixture", cwd: root, modelOverride: model, store,
  emit: () => {}, callTool: async () => "",
});
const reloadHistory = new History(store);
reloadHistory.restore();
const reloadErrors: string[] = [];
const j2 = new UsageJournal(reloadHistory, () => {}, m => reloadErrors.push(m));
let reloadUsageError = "";
try {
  j2.page();
} catch (err) {
  reloadUsageError = err instanceof Error ? err.message : String(err);
}
if (!reloadUsageError && reloadErrors.length > 0) {
  reloadUsageError = reloadErrors[0]!;
}

console.log(JSON.stringify({
  poisoned: errors.length > 0,
  errors,
  page_error: pageError,
  cp_facts: cpFacts,
  message_count: reloaded.snapshot().messages.length,
  reload_usage_error: reloadUsageError,
}));