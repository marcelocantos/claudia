// 🎯T151: a seat's conversation survives a sidecar restart. The store keeps
// its engine entries (messages and compactions); a reload of the same seat
// and session rebuilds the conversation from them, and an unreadable store
// is a hard error, never a silent empty conversation.

import { afterEach, describe, expect, test } from "bun:test";
import { appendFileSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { connect } from "node:net";
import { compact as engineCompact, type SessionEntry } from "@oh-my-pi/pi-agent-core";
import { createMockModel, registerMockApi, type MockResponse } from "@oh-my-pi/pi-ai/providers/mock";
import type { AssistantMessage, Context, Model } from "@oh-my-pi/pi-ai";
import { createSeatAgent, type SeatEvent } from "./seat.ts";
import { SeatStore } from "./store.ts";

registerMockApi();

const window = 100_000;
const promptText = "the quick brown fox jumps over the lazy dog. ".repeat(1200);
const summaryText = "SUMMARY OF EARLIER WORK";

const dirs: string[] = [];
function tempDir(): string {
  const d = mkdtempSync(join(tmpdir(), "t151-"));
  dirs.push(d);
  return d;
}
afterEach(() => {
  for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true });
});

async function fakeSummary(model: Model): Promise<AssistantMessage> {
  return {
    role: "assistant",
    content: [{ type: "text", text: summaryText }],
    api: model.api,
    provider: model.provider,
    model: model.id,
    usage: { input: 0, output: 10, cacheRead: 0, cacheWrite: 0, totalTokens: 10, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
    stopReason: "stop",
    timestamp: Date.now(),
  } as AssistantMessage;
}

function seat(store: SeatStore, handler: (context: Context) => MockResponse) {
  const mock = createMockModel({ contextWindow: window, handler: (context) => handler(context) });
  const events: SeatEvent[] = [];
  let summaries = 0;
  const agent = createSeatAgent({
    provider: "mock",
    model: "mock-model",
    token: "t",
    cwd: process.cwd(),
    emit: (ev) => events.push(ev),
    callTool: async () => "",
    modelOverride: mock as never,
    store,
    maintenance: {
      compactImpl: engineCompact,
      summaryOptions: {
        completeImpl: (async (model: Model) => {
          summaries++;
          return fakeSummary(model);
        }) as never,
      },
    },
  });
  return { mock, events, agent, summaries: () => summaries };
}

const ok = (): MockResponse => ({ content: ["ok"], usage: { input: 1000, output: 1 } });
const sentText = (context: Context) => JSON.stringify(context.messages);

describe("durable seat conversations (🎯T151)", () => {
  test("a reloaded seat resumes its conversation and its compaction, without re-summarizing", async () => {
    const root = tempDir();
    const first = seat(new SeatStore(root, "po", "s1"), ok);
    for (let i = 0; i < 12; i++) await first.agent.prompt(`${i}: ${promptText}`);
    await first.agent.prompt("remember the word PELICAN");
    expect(first.events.some((e) => e.type === "compacted")).toBe(true);

    // A new sidecar: a fresh seat on the same seat and session.
    let seen = "";
    const second = seat(new SeatStore(root, "po", "s1"), (context) => {
      seen = sentText(context);
      return ok();
    });
    await second.agent.prompt("what was the word?");

    expect(seen).toContain(summaryText); // the compaction, as stored
    expect(seen).toContain("PELICAN"); // the tail after it
    expect(second.summaries()).toBe(0); // nothing was recomputed
  });

  test("each message is stored once, however many arrays hold a copy of it", async () => {
    const root = tempDir();
    const first = seat(new SeatStore(root, "po", "s1"), ok);
    await first.agent.prompt("one");
    await first.agent.prompt("two");
    const entries = new SeatStore(root, "po", "s1").load() as SessionEntry[];
    const roles = entries.filter((e) => e.type === "message").map((e) => (e as { message: { role: string } }).message.role);
    expect(roles).toEqual(["user", "assistant", "user", "assistant"]);
  });

  test("a store that already holds a message twice resumes it once, and is rewritten clean", async () => {
    const root = tempDir();
    const store = new SeatStore(root, "po", "s1");
    const t = Date.now();
    const answer = { role: "assistant", content: [{ type: "text", text: "ack" }], api: "mock", provider: "mock", model: "mock-model", usage: { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } }, stopReason: "stop", timestamp: t + 1 };
    store.rewrite([
      { type: "message", id: "u", parentId: null, timestamp: new Date(t).toISOString(), message: { role: "user", content: "hi PELICAN", timestamp: t } },
      { type: "message", id: "a1", parentId: "u", timestamp: new Date(t + 1).toISOString(), message: answer },
      { type: "message", id: "a2", parentId: "a1", timestamp: new Date(t + 1).toISOString(), message: { ...answer } },
    ] as SessionEntry[]);

    let assistants = -1;
    const second = seat(store, (context) => {
      assistants = context.messages.filter((m) => m.role === "assistant").length;
      return ok();
    });
    await second.agent.prompt("next");

    expect(assistants).toBe(1);
    const ids = (store.load() as SessionEntry[]).map((e) => e.id);
    expect(ids.slice(0, 2)).toEqual(["u", "a1"]);
    expect(ids).not.toContain("a2");
  });

  test("a new session starts fresh", async () => {
    const root = tempDir();
    const first = seat(new SeatStore(root, "po", "s1"), ok);
    await first.agent.prompt("remember the word PELICAN");

    let seen = "";
    const other = seat(new SeatStore(root, "po", "s2"), (context) => {
      seen = sentText(context);
      return ok();
    });
    await other.agent.prompt("hello");
    expect(seen).not.toContain("PELICAN");
  });

  test("an unreadable store fails the load instead of starting empty", () => {
    const root = tempDir();
    const store = new SeatStore(root, "po", "s1");
    store.rewrite([]);
    appendFileSync(store.path, "{not json}\n" + JSON.stringify({ type: "message", id: "x" }) + "\n");
    expect(() => seat(store, ok)).toThrow(/conversation store .* line 2 is not JSON/);
  });

  test("a torn last line (a crash mid-append) is dropped and the rest resumes", async () => {
    const root = tempDir();
    const first = seat(new SeatStore(root, "po", "s1"), ok);
    await first.agent.prompt("remember the word PELICAN");
    const store = new SeatStore(root, "po", "s1");
    appendFileSync(store.path, '{"type":"message","id":"torn","mess');

    let seen = "";
    const second = seat(store, (context) => {
      seen = sentText(context);
      return ok();
    });
    await second.agent.prompt("again");
    expect(seen).toContain("PELICAN");
    expect(store.load()?.some((e) => e.id === "torn")).toBe(false);
  });

  test("a refused turn taken back before its retry is not resumed", async () => {
    const root = tempDir();
    let refuse = false;
    const first = seat(new SeatStore(root, "po", "s1"), () =>
      refuse
        ? { stopReason: "error", errorMessage: `400 prompt is too long: 999999 tokens > ${window} maximum` }
        : ok(),
    );
    for (let i = 0; i < 4; i++) await first.agent.prompt(`${i}: ${promptText}`);
    refuse = true;
    await first.agent.prompt("one more");
    refuse = false;

    const entries = new SeatStore(root, "po", "s1").load() as SessionEntry[];
    const refusals = entries.filter(
      (e) => e.type === "message" && (e as { message: { stopReason?: string } }).message.stopReason === "error",
    );
    // The final refusal that ended the turn stays (it is the answer the host
    // saw); the one that was dropped for the retry is gone.
    expect(refusals.length).toBe(1);
  });
});

// A real sidecar process: restart it under a stored seat and read back what
// it rebuilt. `abort` answers with the seat's full state, so no provider
// call is needed.
describe("post-bind tool attestation (🎯T173)", () => {
  test("names come from the model-bound state, including after rebind", () => {
    const agent = createSeatAgent({
      provider: "mock", model: "mock-model", token: "t", cwd: process.cwd(),
      emit: () => {}, callTool: async () => "",
      modelOverride: createMockModel({ contextWindow: window, handler: ok }) as never,
      tools: [{ name: "jevons_agent_send", input_schema: { type: "object" } }],
    });
    expect(agent.boundToolNames()).toContain("Bash");
    expect(agent.boundToolNames()).toContain("jevons_agent_send");
    agent.setHostTools([{ name: "jevons_agent_list", input_schema: { type: "object" } }]);
    expect(agent.boundToolNames()).toContain("jevons_agent_list");
    expect(agent.boundToolNames()).not.toContain("jevons_agent_send");
  });
});

describe("sidecar restart (🎯T151)", () => {
  function startSidecar(sock: string) {
    return Bun.spawn(["bun", join(import.meta.dir, "server.ts"), sock], {
      stdout: "ignore",
      stderr: "pipe",
      env: { ...process.env, JEVONS_SPOOL_DIR: join(sock, "..", "spool") },
    });
  }

  async function ask(sock: string, lines: object[], until: (ev: Record<string, unknown>) => boolean) {
    // The socket appears once the sidecar is listening.
    for (let i = 0; i < 100; i++) {
      try {
        return await new Promise<Record<string, unknown>[]>((resolve, reject) => {
          const out: Record<string, unknown>[] = [];
          let buf = "";
          const c = connect(sock, () => {
            for (const l of lines) c.write(JSON.stringify(l) + "\n");
          });
          c.on("error", reject);
          c.on("data", (d) => {
            buf += d.toString();
            let nl: number;
            while ((nl = buf.indexOf("\n")) >= 0) {
              const ev = JSON.parse(buf.slice(0, nl));
              buf = buf.slice(nl + 1);
              out.push(ev);
              if (until(ev)) {
                c.end();
                resolve(out);
              }
            }
          });
        });
      } catch {
        await Bun.sleep(50);
      }
    }
    throw new Error("sidecar never answered");
  }

  test("a restarted sidecar reloads a seat's stored conversation, compaction included", async () => {
    const dir = tempDir();
    const sock = join(dir, "omp.sock");
    const store = new SeatStore(join(dir, "omp-seats"), "po", "s1");
    const t = Date.now();
    const iso = (d: number) => new Date(t + d).toISOString();
    const msg = (id: string, parentId: string | null, d: number, role: string, text: string): SessionEntry =>
      ({
        type: "message",
        id,
        parentId,
        timestamp: iso(d),
        message:
          role === "user"
            ? { role, content: text, timestamp: t + d }
            : { role, content: [{ type: "text", text }], api: "anthropic-messages", provider: "anthropic", model: "claude-sonnet-5", usage: { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } }, stopReason: "stop", timestamp: t + d },
      }) as SessionEntry;
    store.rewrite([
      msg("a", null, 0, "user", "old question"),
      msg("b", "a", 1, "assistant", "old answer"),
      msg("c", "b", 2, "user", "kept question PELICAN"),
      msg("d", "c", 3, "assistant", "kept answer"),
      { type: "compaction", id: "e", parentId: "d", timestamp: iso(4), summary: summaryText, firstKeptEntryId: "c", tokensBefore: 5000, details: { pins: ["PINNED-FACT-42"] } } as SessionEntry,
      msg("f", "e", 5, "user", "after the compaction"),
      msg("g", "f", 6, "assistant", "latest answer"),
    ]);
    const load = { op: "load", seat: "po", provider: "anthropic", model: "claude-sonnet-5", token: "t", cwd: dir, session_id: "s1" };

    for (let run = 0; run < 2; run++) {
      const proc = startSidecar(sock);
      try {
        const out = await ask(sock, [load, { op: "abort", seat: "po" }], (ev) => ev.type === "turn_end");
        expect(out[0].type).toBe("ready");
        // The host words its restart note by this (jevons 🎯T929).
        expect(out[0].restored).toBe(true);
        const state = out.at(-1)?.snapshot as { messages: { role: string }[] };
        const text = JSON.stringify(state.messages);
        expect(state.messages[0].role).toBe("compactionSummary");
        expect(text).toContain(summaryText);
        expect(text).toContain("PELICAN"); // the kept tail
        expect(text).toContain("latest answer"); // after the compaction
        expect(text).toContain("PINNED-FACT-42"); // pins survive a restart (🎯T152)
        expect(text).not.toContain("old question"); // folded away
      } finally {
        proc.kill();
        await proc.exited;
      }
    }
  });

  test("ready attests the tools bound on load and rebind (🎯T173)", async () => {
    const dir = tempDir();
    const sock = join(dir, "omp.sock");
    const proc = startSidecar(sock);
    try {
      const base = { seat: "tool-seat", provider: "anthropic", model: "claude-sonnet-5", token: "t", cwd: dir };
      const out = await ask(sock, [
        { ...base, op: "load", tools: [{ name: "jevons_agent_send", input_schema: { type: "object" } }] },
        { ...base, op: "adopt", tools: [{ name: "jevons_agent_list", input_schema: { type: "object" } }] },
      ], (ev) => ev.type === "ready" && ev.how === "adopted");
      expect(out.map((ev) => ev.how)).toEqual(["launched", "adopted"]);
      expect(out[0].bound_tools).toContain("Bash");
      expect(out[0].bound_tools).toContain("jevons_agent_send");
      expect(out[1].bound_tools).toContain("jevons_agent_list");
      expect(out[1].bound_tools).not.toContain("jevons_agent_send");
    } finally {
      proc.kill();
      await proc.exited;
    }
  });

  test("a seat with nothing stored is reported as not restored (jevons 🎯T929)", async () => {
    const dir = tempDir();
    const sock = join(dir, "omp.sock");
    const proc = startSidecar(sock);
    try {
      const out = await ask(
        sock,
        [{ op: "load", seat: "fresh", provider: "anthropic", model: "claude-sonnet-5", token: "t", cwd: dir, session_id: "s-new" }],
        (ev) => ev.type === "ready" || ev.type === "error",
      );
      expect(out.at(-1)?.type).toBe("ready");
      expect(out.at(-1)?.restored).toBe(false);
    } finally {
      proc.kill();
      await proc.exited;
    }
  });

  test("a restarted sidecar refuses a seat whose store is unreadable", async () => {
    const dir = tempDir();
    const sock = join(dir, "omp.sock");
    const store = new SeatStore(join(dir, "omp-seats"), "po", "s1");
    store.rewrite([]);
    writeFileSync(store.path, "garbage\n");
    const proc = startSidecar(sock);
    try {
      const out = await ask(
        sock,
        [{ op: "load", seat: "po", provider: "anthropic", model: "claude-sonnet-5", token: "t", cwd: dir, session_id: "s1" }],
        (ev) => ev.type === "error" || ev.type === "ready",
      );
      expect(out.at(-1)?.type).toBe("error");
      expect(String(out.at(-1)?.text)).toContain("conversation store");
    } finally {
      proc.kill();
      await proc.exited;
    }
  });
});
