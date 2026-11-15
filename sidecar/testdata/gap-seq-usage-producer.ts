// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Hermetic producer: conversation journal with a stored seq gap (compaction-style).
import { readFileSync, writeFileSync } from "node:fs";
import { createMockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";
import { createSeatAgent } from "../seat.ts";
import { SeatStore } from "../store.ts";
import { History } from "../maintain.ts";
import { UsageJournal } from "../usage.ts";
import { join } from "node:path";

registerMockApi();
const root = process.argv[2];
const store = new SeatStore(join(root, "omp-seats"), "seat", "generation");
const model = createMockModel({ handler: () => ({ content: ["ok"], usage: { input: 11, output: 3 } }) });
const agent = createSeatAgent({
  provider: "mock", model: "mock", token: "fixture", cwd: root, modelOverride: model, store,
  emit: () => {}, callTool: async () => "",
});
await agent.prompt("fixture", { request_id: "A", attempt_id: "attempt-A", session_id: "generation", turn_id: "turn-A" });
let text = readFileSync(store.path, "utf8");
text = text.replace('"seq":2', '"seq":4');
writeFileSync(store.path, text);
const restored = new History(store);
restored.restore();
const errors: string[] = [];
const journal = new UsageJournal(restored, () => {}, msg => errors.push(msg));
let pageError = "";
try {
  journal.page();
} catch (err) {
  pageError = err instanceof Error ? err.message : String(err);
}
console.log(JSON.stringify({ poisoned: errors.length > 0, errors, page_error: pageError }));
