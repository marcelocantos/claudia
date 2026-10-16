// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

import { afterEach, expect, test } from "bun:test";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { createMockModel, registerMockApi, type MockResponse } from "@oh-my-pi/pi-ai/providers/mock";
import type { AssistantMessage, Context } from "@oh-my-pi/pi-ai";
import type { SessionEntry } from "@oh-my-pi/pi-agent-core";
import { createSeatAgent, type SeatEvent } from "./seat.ts";
import { SeatStore } from "./store.ts";
import { History } from "./maintain.ts";
import { ReceiptJournal } from "./receipts.ts";

registerMockApi();
const roots: string[] = [];
afterEach(() => { for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true }); });
function store() {
  const root = mkdtempSync(join(tmpdir(), "claudia-receipts-"));
  roots.push(root);
  return new SeatStore(root, "worker", "session");
}
const meta = (id: string) => ({ request_id: id, session_id: "session", attempt_id: `attempt-${id}`, turn_id: `turn-${id}` });
function fixture(storage: SeatStore, handler: (context: Context) => MockResponse = () => ({ content: ["ok"] })) {
  const events: SeatEvent[] = [];
  const model = createMockModel({ handler });
  const agent = createSeatAgent({ provider: "mock", model: "mock", token: "test", cwd: dirname(storage.path), modelOverride: model,
    store: storage, emit: e => events.push(e), callTool: async () => "" });
  return { agent, model, events };
}

test("identified prompts deduplicate across acknowledgement loss and restart, not by text", async () => {
  const storage = store();
  const first = fixture(storage);
  await first.agent.prompt("same text", meta("A"));
  await first.agent.prompt("same text", meta("A"));
  await first.agent.prompt("same text", meta("B"));
  expect(first.model.calls.length).toBe(2);
  expect(first.agent.receipt("A").state).toBe("completed");
  expect(first.agent.receipt("B").turn_ids).toEqual(["turn-B"]);
  const second = fixture(storage);
  expect(second.agent.receipt("A")).toEqual(first.agent.receipt("A"));
  await second.agent.prompt("same text", meta("A"));
  expect(second.model.calls.length).toBe(0);
  await expect(second.agent.prompt("different", meta("A"))).rejects.toThrow(/conflicting payload/);
  await expect(second.agent.prompt("same text", { ...meta("C"), session_id: "other" })).rejects.toThrow(/identity mismatch/);
});

test("active A, identical-text steer B and later prompt C retain separate receipts", async () => {
  const f = fixture(store(), () => {
    if (f.model.calls.length === 1) {
      expect(f.agent.receipt("A").state).toBe("started");
      f.agent.steer("same text", meta("B"));
      f.agent.steer("same text", { ...meta("B"), attempt_id: "another-attempt" });
      void f.agent.prompt("same text", meta("C"));
      expect(f.agent.receipt("B").state).toBe("accepted");
      expect(f.agent.receipt("C").state).toBe("accepted");
    }
    return { content: ["ok"] };
  });
  await f.agent.prompt("same text", meta("A"));
  for (const id of ["A", "B", "C"]) expect(f.agent.receipt(id).state).toBe("completed");
  expect(f.events.filter(e => e.type === "absorbed").map(e => e.request_id)).toEqual(["B", "C"]);
  const finalUsers = f.model.calls.at(-1)!.context.messages.filter(m => m.role === "user");
  expect(finalUsers.map(m => (m as any).claudiaRequest.request_id)).toEqual(["A", "B", "C"]);
});

test("an unconsumed follow-up survives terminal failure and restart without being marked complete", async () => {
  const storage = store();
  const first = fixture(storage, () => {
    void first.agent.prompt("pending", meta("B"));
    return { stopReason: "error", errorMessage: "429 capacity unavailable" };
  });
  await first.agent.prompt("original", meta("A"));
  expect(first.agent.receipt("A").state).toBe("failed");
  expect(first.agent.receipt("B").state).toBe("accepted");
  const second = fixture(storage);
  expect(second.model.calls.length).toBe(0);
  expect(second.agent.receipt("B").state).toBe("accepted");
  await second.agent.prompt("pending", meta("B"));
  expect(second.model.calls.length).toBe(1);
  expect(second.agent.receipt("B").state).toBe("completed");
  expect(second.model.calls[0].context.messages.filter(m => (m as any).claudiaRequest?.request_id === "B").length).toBe(1);
});

test("a restart during dispatch exposes uncertainty and never automatically replays it", async () => {
  const storage = store();
  let recovered: ReturnType<typeof fixture> | undefined;
  const first = fixture(storage, () => {
    recovered = fixture(storage);
    expect(recovered.agent.receipt("A").state).toBe("unknown");
    expect(recovered.agent.receipt("A").reason).toBe("interrupted");
    return { content: ["ok"] };
  });
  await first.agent.prompt("effect", meta("A"));
  await recovered!.agent.prompt("effect", meta("A"));
  expect(recovered!.model.calls.length).toBe(0);
});

test("acceptance and dispatch persistence failures prevent provider calls", async () => {
  for (const failedState of ["accepted", "started"]) {
    const storage = store();
    const append = storage.append.bind(storage);
    storage.append = (entries: readonly SessionEntry[]) => {
      if (entries.some(e => e.type === "custom" && (e.data as any)?.receipt?.state === failedState)) throw new Error("disk unavailable");
      append(entries);
    };
    const f = fixture(storage);
    await expect(f.agent.prompt("must not run", meta("A"))).rejects.toThrow(/persistence failed/);
    expect(f.model.calls.length).toBe(0);
    expect(f.agent.receipt("A").state).toBe("unknown");
    await expect(f.agent.prompt("legacy must also not run")).rejects.toThrow(/persistence failed/);
    expect(f.model.calls.length).toBe(0);
  }
});

test("cancellation settles only inputs dispatched in the cancelled turn", async () => {
  const f = fixture(store(), () => {
    f.agent.abort();
    return { content: ["interrupted"] };
  });
  await f.agent.prompt("work", meta("A"));
  expect(f.agent.receipt("A").state).toBe("cancelled");
});

test("an uncertain restored tool batch cannot execute while draining an accepted follow-up", async () => {
  const storage = store();
  const h = new History(storage);
  const journal = new ReceiptJournal(h);
  const a = journal.accept("write a file", meta("A"), "prompt");
  h.sync([a.message]);
  journal.start(["A"], "turn-A");
  const effect = join(dirname(storage.path), "must-not-replay.txt");
  h.sync([{ role: "assistant", content: [{ type: "toolCall", id: "write-1", name: "Write", arguments: { path: effect, content: "replayed" } }],
    api: "mock", provider: "mock", model: "mock", stopReason: "toolUse", timestamp: Date.now(),
    usage: { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } } } as AssistantMessage]);
  journal.accept("queued", meta("B"), "prompt");
  const f = fixture(storage);
  expect(f.agent.receipt("A").state).toBe("unknown");
  await expect(f.agent.prompt("queued", meta("B"))).rejects.toMatchObject({ reason: "execution_uncertain" });
  expect(() => f.agent.steer("new", meta("C"))).toThrow(/unfinished tool calls/);
  await expect(f.agent.switchModel("mock", "other")).rejects.toMatchObject({ reason: "execution_uncertain" });
  expect(f.model.calls.length).toBe(0);
  expect(existsSync(effect)).toBe(false);
  expect(f.agent.receipt("B").state).toBe("accepted");
});

test("recovery removes an undispatched history copy before a provider switch rebuilds context", async () => {
  const storage = store();
  const h = new History(storage);
  const journal = new ReceiptJournal(h);
  const b = journal.accept("queued", meta("B"), "prompt");
  // Crash after message_start persisted the input, before the model gate.
  h.persist([b.message]);
  const a = createMockModel({ id: "a", provider: "anthropic", handler: () => ({ content: ["unexpected"] }) });
  const next = createMockModel({ id: "b", provider: "openai-codex", handler: () => ({ content: ["ok"] }) });
  const agent = createSeatAgent({ provider: a.provider, model: a.id, token: "t", cwd: dirname(storage.path), modelOverride: a,
    resolveModelOverride: () => next, store: storage, emit: () => {}, callTool: async () => "" });
  await agent.switchModel(next.provider, next.id);
  await agent.prompt("queued", meta("B"));
  expect(a.calls.length).toBe(0);
  expect(next.calls.length).toBe(1);
  expect(next.calls[0].context.messages.filter(m => (m as any).claudiaRequest?.request_id === "B").length).toBe(1);
  expect(agent.receipt("B").state).toBe("completed");
});

test("auth recovery preserves host identities while its own handshake remains separate", async () => {
  const storage = store();
  const events: SeatEvent[] = [];
  const model = createMockModel({ handler: () => model.calls.length === 1
    ? { stopReason: "error", errorMessage: "401 OAuth access token has expired" } : { content: ["ok"] } });
  const agent = createSeatAgent({ provider: "mock", model: "mock", token: "failed", cwd: dirname(storage.path), modelOverride: model,
    store: storage, emit: e => { events.push(e); if (e.type === "auth_retry") {
      void agent.prompt("queued", { ...meta("B"), rpc_id: "submit-B" });
      agent.steer("steered", { ...meta("C"), rpc_id: "submit-C" });
      agent.authRetry({ ...e, token: "new", expires_at: Date.now() + 60_000 });
    } }, callTool: async () => "" });
  await agent.prompt("original", { ...meta("A"), rpc_id: "submit-A" });
  const ids = (context: Context) => context.messages.filter(m => m.role === "user").map(m => (m as any).claudiaRequest?.request_id);
  expect(ids(model.calls[0].context)).toEqual(["A"]);
  expect(ids(model.calls[1].context)).toEqual(["A"]);
  expect(ids(model.calls.at(-1)!.context)).toEqual(["A", "C", "B"]);
  expect(events.filter(e => e.type === "accepted").map(e => [e.request_id, e.rpc_id])).toEqual([["A", "submit-A"], ["B", "submit-B"], ["C", "submit-C"]]);
  expect(events.filter(e => e.type === "receipt" && e.receipt?.state === "completed").map(e => e.request_id).sort()).toEqual(["A", "B", "C"]);
  const auth = events.find(e => e.type === "auth_retry")!;
  expect(agent.receipt(auth.request_id!).reason).toBe("not_found");
  for (const id of ["A", "B", "C"]) expect(agent.receipt(id).state).toBe("completed");
});

test("provider-switch compaction retains durable receipts through continued dispatch and restart", async () => {
  const storage = store();
  const large = "the quick brown fox jumps over the lazy dog. ".repeat(1200);
  const from = createMockModel({ id: "a", provider: "anthropic", contextWindow: 400_000, handler: () => ({ content: ["ok"] }) });
  const next = createMockModel({ id: "b", provider: "openai-codex", contextWindow: 60_000, handler: () => ({ content: ["ok"] }) });
  let summaries = 0;
  const agent = createSeatAgent({ provider: from.provider, model: from.id, token: "t", cwd: dirname(storage.path), modelOverride: from,
    store: storage, resolveModelOverride: () => next, emit: () => {}, callTool: async () => "", maintenance: {
      nativeEligible: () => false, summaryOptions: { completeImpl: (async (model: any) => {
        summaries++;
        return { role: "assistant", content: [{ type: "text", text: "SUMMARY OF EARLIER WORK" }], api: model.api, provider: model.provider, model: model.id,
          usage: { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
          stopReason: "stop", timestamp: Date.now() } as AssistantMessage;
      }) as never } } });
  for (let i = 0; i < 8; i++) await agent.prompt(`${i}: ${large}`, meta(`R${i}`));
  await agent.switchModel(next.provider, next.id);
  expect(summaries).toBeGreaterThan(0);
  await agent.prompt("continue", meta("later"));
  const recovered = fixture(storage);
  for (const id of [...Array.from({ length: 8 }, (_, i) => `R${i}`), "later"]) {
    expect(agent.receipt(id).state).toBe("completed");
    expect(recovered.agent.receipt(id)).toEqual(agent.receipt(id));
  }
  expect(JSON.stringify(next.calls[0].context.messages)).toContain("SUMMARY OF EARLIER WORK");
  expect(JSON.stringify(next.calls[0].context.messages)).not.toContain("claudia_request_v1");
  await recovered.agent.prompt(`0: ${large}`, meta("R0"));
  expect(recovered.model.calls.length).toBe(0);
});

test("accepted input survives cancelled pre-dispatch compaction without requiring a restart", async () => {
  const storage = store();
  const history = new History(storage);
  const large = "the quick brown fox jumps over the lazy dog. ".repeat(1200);
  for (let i = 0; i < 8; i++) history.sync([
    { role: "user", content: `${i}: ${large}`, timestamp: i * 2 },
    { role: "assistant", content: [{ type: "text", text: "ok" }], api: "mock", provider: "mock", model: "mock", timestamp: i * 2 + 1,
      usage: { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } }, stopReason: "stop" } as AssistantMessage,
  ]);
  history.persist();
  const model = createMockModel({ contextWindow: 60_000, handler: () => ({ content: ["ok"] }) });
  let compactions = 0;
  const agent = createSeatAgent({ provider: "mock", model: "mock", token: "t", cwd: dirname(storage.path), store: storage,
    modelOverride: model, emit: () => {}, callTool: async () => "", maintenance: {
      nativeEligible: () => false,
      compactImpl: (async (prep: any) => {
        compactions++;
        if (compactions === 1) {
          agent.abort();
          throw new Error("cancelled preparation");
        }
        return { summary: "earlier work", firstKeptEntryId: prep.firstKeptEntryId, tokensBefore: prep.tokensBefore };
      }) as never,
    } });
  await expect(agent.prompt("new obligation", meta("A"))).rejects.toThrow("cancelled preparation");
  expect(agent.receipt("A").state).toBe("accepted");
  expect(model.calls.length).toBe(0);
  await agent.prompt("new obligation", meta("A"));
  expect(model.calls.length).toBe(1);
  expect(model.calls[0].context.messages.filter(m => (m as any).claudiaRequest?.request_id === "A").length).toBe(1);
  expect(agent.receipt("A").state).toBe("completed");
});
