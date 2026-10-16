// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { expect, test } from "bun:test";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createMockModel, registerMockApi } from "@oh-my-pi/pi-ai/providers/mock";
import type { SessionEntry } from "@oh-my-pi/pi-agent-core";
import { createSeatAgent } from "./seat.ts";
import { History } from "./maintain.ts";
import { ReceiptJournal } from "./receipts.ts";
import { SeatStore } from "./store.ts";

registerMockApi();
test("required resume rejects empty and bookkeeping-only generations without mutation or model dispatch", () => {
  const root = mkdtempSync(join(tmpdir(), "seat-resume-"));
  try {
    const model = createMockModel({ handler: () => ({ content: ["unexpected"] }) });
    for (const customType of ["", "claudia_usage_v1"]) {
      const store = new SeatStore(root, "seat", customType || "empty");
      const entries = customType ? [{ type: "custom", id: "fact", customType, data: {} }] as SessionEntry[] : [];
      store.rewrite(entries);
      const before = readFileSync(store.path, "utf8");
      expect(() => createSeatAgent({ provider: "mock", model: "mock", token: "test", cwd: root, modelOverride: model,
        store, requireResume: true, emit: () => {}, callTool: async () => "" })).toThrow("existing conversation required");
      expect(readFileSync(store.path, "utf8")).toBe(before);
    }
    expect(model.calls).toHaveLength(0);
  } finally { rmSync(root, { recursive: true, force: true }); }
});

test("required resume recovers accepted input through the existing receipt queue, but not terminal-only receipts", async () => {
  const root = mkdtempSync(join(tmpdir(), "seat-resume-pending-"));
  try {
    const store = new SeatStore(root, "seat", "generation");
    const history = new History(store);
    const journal = new ReceiptJournal(history);
    const meta = { request_id: "pending", session_id: "generation", attempt_id: "attempt" };
    journal.accept("recover this exact input", meta, "prompt");
    const model = createMockModel({ handler: () => ({ content: ["ok"] }) });
    const opts = { provider: "mock", model: "mock", token: "test", cwd: root, modelOverride: model,
      store, requireResume: true, emit: () => {}, callTool: async () => "" };
    const before = readFileSync(store.path, "utf8");
    const restored = createSeatAgent(opts);
    expect(readFileSync(store.path, "utf8")).toBe(before);
    expect(model.calls).toHaveLength(0);
    await restored.prompt("recover this exact input", meta);
    expect(model.calls).toHaveLength(1);
    expect(model.calls[0].context.messages.some(m => m.role === "user" && JSON.stringify(m).includes("recover this exact input"))).toBe(true);
    // A completed receipt alone carries no recoverable conversation or work.
    const terminal = new SeatStore(root, "seat", "terminal");
    const terminalHistory = new History(terminal);
    const terminalJournal = new ReceiptJournal(terminalHistory);
    terminalJournal.accept("finished", { ...meta, session_id: "terminal" }, "prompt");
    terminalJournal.start(["pending"], "turn");
    terminalJournal.finish(["pending"], "completed");
    const closed = readFileSync(terminal.path, "utf8");
    expect(() => createSeatAgent({ ...opts, store: terminal })).toThrow("existing conversation required");
    expect(readFileSync(terminal.path, "utf8")).toBe(closed);
  } finally { rmSync(root, { recursive: true, force: true }); }
});
