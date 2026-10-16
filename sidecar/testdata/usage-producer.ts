// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Hermetic producer for the Go socket/pump test. No provider or auth lookup.
import { createMockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";
import { createSeatAgent } from "../seat.ts";
import { SeatStore } from "../store.ts";
import { join } from "node:path";
registerMockApi();
const root = process.argv[2];
const model = createMockModel({ handler: () => ({ content: ["fixture answer"], usage: { input: 11, output: 3, cacheRead: 5 } }) });
const agent = createSeatAgent({ provider: "mock", model: "mock", token: "fixture", cwd: root,
  modelOverride: model, store: new SeatStore(join(root, "omp-seats"), "seat", "generation"), emit: () => {}, callTool: async () => "" });
await agent.prompt("fixture input", { request_id: "logical-A", attempt_id: "attempt-A", session_id: "generation", turn_id: "turn-A" });
console.log(JSON.stringify(agent.usage()));
