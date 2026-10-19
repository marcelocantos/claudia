// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// T1074.12: two seats in one sidecar process, each hosted by the SDK's own
// AgentSession (createOmpSession without preserveSeatLoop), with the SDK
// owning compaction. Driven over the real Unix socket against the unchanged
// server, with the catalog row replaced by the mock provider.
//
// The three review catches each have an assertion here:
//   1. registry: the direct AgentSession constructor never registers with the
//      process AgentRegistry, and each seat has its own session, settings,
//      model registry and agent (probe).
//   2. process-wide settings: a per-seat setting (compaction threshold)
//      differs between the seats and only the seat that set it compacts; a
//      process-global setting is refused on a seat (separate test below).
//   3. isTerminal: a request whose provider call overflowed is compacted and
//      retried by the session. Its receipt stays started until the retried
//      call has dispatched and closes once, completed.

import { afterEach, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { connect, type Socket } from "node:net";

const dirs: string[] = [];
afterEach(() => {
  for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true });
});

type Ev = Record<string, any>;
type Dispatch = { seat: string; kind: string; requests: string[]; compacted: boolean; receipts: Record<string, string>; system_has_claudia: boolean; at: number };

class Client {
  events: Ev[] = [];
  #buf = "";
  #waiters: { pred: (e: Ev) => boolean; resolve: (e: Ev) => void }[] = [];
  constructor(readonly socket: Socket) {
    socket.on("data", d => {
      this.#buf += d.toString();
      let nl: number;
      while ((nl = this.#buf.indexOf("\n")) >= 0) {
        const ev = JSON.parse(this.#buf.slice(0, nl));
        this.#buf = this.#buf.slice(nl + 1);
        this.events.push(ev);
        for (const w of [...this.#waiters]) if (w.pred(ev)) {
          this.#waiters.splice(this.#waiters.indexOf(w), 1);
          w.resolve(ev);
        }
      }
    });
  }
  send(line: object) { this.socket.write(JSON.stringify(line) + "\n"); }
  waitFor(pred: (e: Ev) => boolean, label: string, ms = 20_000): Promise<Ev> {
    const seen = this.events.find(pred);
    if (seen) return Promise.resolve(seen);
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`timed out waiting for ${label}; last events: ${JSON.stringify(this.events.slice(-8).map(e => ({ seat: e.seat, type: e.type, request_id: e.request_id, text: e.text, state: e.receipt?.state })))}`)), ms);
      this.#waiters.push({ pred, resolve: e => { clearTimeout(timer); resolve(e); } });
    });
  }
  // The next event matching pred that arrives after this call.
  next(pred: (e: Ev) => boolean, label: string, ms = 20_000): Promise<Ev> {
    const from = this.events.length;
    return this.waitFor(e => this.events.indexOf(e) >= from && pred(e), label, ms);
  }
}

async function dial(sock: string): Promise<Client> {
  for (let i = 0; i < 200; i++) {
    try {
      return await new Promise<Client>((resolve, reject) => {
        const c = connect(sock, () => resolve(new Client(c)));
        c.on("error", reject);
      });
    } catch { await Bun.sleep(50); }
  }
  throw new Error("sidecar never listened");
}

function startSidecar(root: string) {
  const sock = join(root, "omp.sock");
  const log = join(root, "dispatch.jsonl");
  const proc = Bun.spawn([process.execPath, join(import.meta.dir, "testdata/two-seats-provider.ts"), sock, log], {
    stdout: "ignore",
    stderr: "pipe",
    env: {
      PATH: process.env.PATH ?? "",
      HOME: root, TMPDIR: root,
      XDG_CONFIG_HOME: join(root, "config"), XDG_DATA_HOME: join(root, "data"),
      XDG_STATE_HOME: join(root, "state"), XDG_CACHE_HOME: join(root, "cache"),
      PI_CONFIG_DIR: ".omp", PI_CODING_AGENT_DIR: join(root, ".omp/agent"),
      OMP_PROFILE: "", PI_PROFILE: "", PI_NO_TITLE: "1",
      CLAUDIA_OMP_SDK_ROOT: root,
      CLAUDIA_SIDECAR_SDK_PROBE: "1",
      CLAUDIA_OMP_SPOOL_DIR: join(root, "spool"),
    },
  });
  const dispatches = (): Dispatch[] => {
    try { return readFileSync(log, "utf8").split("\n").filter(Boolean).map(l => JSON.parse(l)); }
    catch { return []; }
  };
  return { sock, proc, dispatches };
}

const big = (label: string) => `${label} ${"lorem ipsum dolor sit amet ".repeat(700)}`;

test("two SDK-hosted seats share one sidecar without sharing registry, settings, receipts or usage", async () => {
  const root = mkdtempSync(join(tmpdir(), "cl-two-seats-"));
  dirs.push(root);
  for (const s of ["A", "B"]) mkdirSync(join(root, `work-${s}`));
  const { sock, proc, dispatches } = startSidecar(root);
  let stderr = "";
  const stderrDone = new Response(proc.stderr).text().then(t => { stderr = t; });
  const c = await dial(sock);
  const ofSeat = (seat: string) => (e: Ev) => e.seat === seat;
  const receiptState = (id: string) => (e: Ev) => e.type === "receipt" && e.request_id === id && ["completed", "failed", "cancelled"].includes(e.receipt?.state);
  const turnEnd = (seat: string) => (e: Ev) => e.seat === seat && e.type === "turn_end";
  const prompt = (seat: string, id: string, text: string) => c.send({ op: "prompt", seat, session_id: `s-${seat}`, request_id: id, attempt_id: `${id}-1`, text: `[seat:${seat}] ${text}` });
  try {
    // Seat A compacts at 6000 tokens; seat B keeps the SDK default for a
    // 200k window. One setting, two values, one process.
    c.send({ op: "load", seat: "A", provider: "anthropic", model: "claude-sonnet-5", token: "tok-plan", cwd: join(root, "work-A"), session_id: "s-A", compact_at_tokens: 6000 });
    c.send({ op: "load", seat: "B", provider: "anthropic", model: "claude-sonnet-5", token: "tok-plan", cwd: join(root, "work-B"), session_id: "s-B" });
    await c.waitFor(e => e.seat === "A" && (e.type === "ready" || e.type === "error"), "A ready");
    await c.waitFor(e => e.seat === "B" && (e.type === "ready" || e.type === "error"), "B ready");
    expect(c.events.filter(e => e.type === "error")).toEqual([]);

    // 1 + 2: overlapping turns, with a steer into A's busy turn.
    prompt("A", "a1", "SLOW first question");
    prompt("B", "b1", "SLOW first question");
    await c.waitFor(e => e.type === "receipt" && e.request_id === "a1" && e.receipt?.state === "started", "a1 started");
    await c.waitFor(e => e.type === "receipt" && e.request_id === "b1" && e.receipt?.state === "started", "b1 started");
    c.send({ op: "steer", seat: "A", session_id: "s-A", request_id: "a-steer", attempt_id: "a-steer-1", text: "[seat:A] steer: also mention the steer" });
    await c.waitFor(receiptState("a-steer"), "a-steer settled");
    await c.waitFor(receiptState("b1"), "b1 settled");
    await c.waitFor(turnEnd("A"), "A turn_end");
    await c.waitFor(turnEnd("B"), "B turn_end");

    // The probe runs after both sessions exist and have dispatched.
    c.send({ op: "sdk_probe", rpc_id: "probe-1" });
    const probe = (await c.waitFor(e => e.type === "sdk_probe", "probe")).sdk_probe;
    expect(probe.registry_ids).toEqual([]);
    expect(probe.distinct).toEqual({ sessions: 2, settings: 2, model_registries: 2, agents: 2 });
    expect(probe.seats.A).toMatchObject({ threshold_tokens: 6000, compaction_enabled: true, agent_id: null });
    expect(probe.seats.B).toMatchObject({ threshold_tokens: -1, compaction_enabled: true, agent_id: null });
    expect(probe.seats.A.session_id).not.toBe(probe.seats.B.session_id);

    // 2: the same oversized history on both seats. Only A's threshold trips.
    for (let i = 0; i < 3; i++) {
      prompt("A", `a-big-${i}`, big(`bulk ${i}`));
      prompt("B", `b-big-${i}`, big(`bulk ${i}`));
      await c.waitFor(receiptState(`a-big-${i}`), `a-big-${i}`);
      await c.waitFor(receiptState(`b-big-${i}`), `b-big-${i}`);
    }
    // 3: an overflow on B. The SDK compacts and retries the same request.
    prompt("B", "b-overflow", "OVERFLOW: answer after compaction");
    await c.waitFor(receiptState("b-overflow"), "b-overflow settled", 30_000);

    // Abort survival: A is aborted mid-turn while B keeps streaming, then
    // the reverse. Each survivor finishes and each aborted seat serves again.
    prompt("A", "a-abort", "SLOW to be aborted");
    prompt("B", "b-keep", "SLOW keep going");
    await c.waitFor(e => e.type === "receipt" && e.request_id === "a-abort" && e.receipt?.state === "started", "a-abort started");
    c.send({ op: "abort", seat: "A" });
    await c.waitFor(receiptState("b-keep"), "b-keep settled");
    prompt("A", "a-after", "after the abort");
    await c.waitFor(receiptState("a-after"), "a-after settled");
    prompt("B", "b-abort", "SLOW to be aborted");
    prompt("A", "a-keep", "SLOW keep going");
    await c.waitFor(e => e.type === "receipt" && e.request_id === "b-abort" && e.receipt?.state === "started", "b-abort started");
    c.send({ op: "abort", seat: "B" });
    await c.waitFor(receiptState("a-keep"), "a-keep settled");
    prompt("B", "b-after", "after the abort");
    await c.waitFor(receiptState("b-after"), "b-after settled");

    const final = (id: string) => c.events.filter(receiptState(id)).map(e => e.receipt.state);
    // Exactly one terminal receipt event per request, with the right state.
    const expected: Record<string, string> = {
      a1: "completed", "a-steer": "completed", b1: "completed",
      "a-big-0": "completed", "a-big-1": "completed", "a-big-2": "completed",
      "b-big-0": "completed", "b-big-1": "completed", "b-big-2": "completed",
      "b-overflow": "completed",
      "a-abort": "cancelled", "b-keep": "completed", "a-after": "completed",
      "b-abort": "cancelled", "a-keep": "completed", "b-after": "completed",
    };
    const got: Record<string, string[]> = {};
    for (const id of Object.keys(expected)) got[id] = final(id);
    expect(got).toEqual(Object.fromEntries(Object.entries(expected).map(([k, v]) => [k, [v]])));
    // Receipts belong to the seat that accepted them.
    for (const e of c.events.filter(e => e.type === "receipt" && e.receipt)) {
      expect(e.receipt.seat).toBe(e.seat);
      expect(e.request_id.startsWith(e.seat.toLowerCase())).toBe(true);
    }

    const log = dispatches();
    const model = log.filter(d => d.kind !== "summary");
    // A seat's provider call never carries another seat's request.
    for (const d of log) {
      for (const id of d.requests) expect(`${d.seat}:${id[0]}`).toBe(`${d.seat}:${d.seat.toLowerCase()}`);
    }
    // The request a model call serves is the newest one in its context. Its
    // durable receipt must be open (started) when the call dispatches: a
    // receipt closed by an earlier, non-terminal agent_end fails here.
    const served = model.map(d => ({ seat: d.seat, kind: d.kind, id: d.requests.at(-1)!, state: d.receipts[d.requests.at(-1)!] }));
    expect(served.filter(s => s.state !== "started")).toEqual([]);
    // The two SLOW first turns were in flight together (each holds its
    // provider call for 1.5 s), and the steer got a provider call of its own
    // inside A's turn, after it was absorbed.
    const firstOf = (id: string) => model.find(d => d.requests.at(-1) === id)!;
    expect(Math.abs(firstOf("a1").at - firstOf("b1").at)).toBeLessThan(1000);
    expect(served.some(s => s.id === "a-steer")).toBe(true);
    expect(c.events.some(e => e.seat === "A" && e.type === "absorbed" && e.request_id === "a-steer")).toBe(true);
    // The SDK kept the seat's system prompt.
    expect(model.every(d => d.system_has_claudia)).toBe(true);
    // The overflow was refused once, compacted, and retried for the same
    // request: two dispatches serve b-overflow, the retry after a summary.
    expect(served.filter(s => s.id === "b-overflow").map(s => s.kind)).toEqual(["overflow", "model"]);
    const overflowAt = log.findIndex(d => d.kind === "overflow");
    expect(log.slice(overflowAt).some(d => d.kind === "summary" && d.seat === "B")).toBe(true);
    // Per-seat threshold: A compacted on its own setting before any overflow;
    // B's threshold never tripped, so B summarized only for the overflow.
    const summaries = (seat: string) => log.filter(d => d.kind === "summary" && d.seat === seat).length;
    const bBeforeOverflow = log.slice(0, overflowAt).filter(d => d.kind === "summary" && d.seat === "B").length;
    expect(summaries("A")).toBeGreaterThan(0);
    expect(bBeforeOverflow).toBe(0);
    expect(c.events.some(e => e.seat === "A" && e.type === "compacted")).toBe(true);

    // Usage facts are recorded per seat and attributed to that seat's requests.
    for (const seat of ["A", "B"]) {
      c.send({ op: "usage", seat, session_id: `s-${seat}`, rpc_id: `usage-${seat}` });
      const page = (await c.waitFor(e => e.type === "usage_page" && e.rpc_id === `usage-${seat}`, `usage ${seat}`)).usage_page;
      expect(page.seat).toBe(seat);
      const settled = page.facts.filter((f: any) => f.state !== "started");
      expect(settled.length).toBeGreaterThan(0);
      for (const fact of page.facts) {
        expect(fact.seat).toBe(seat);
        for (const ref of fact.requests) expect(ref.request_id[0]).toBe(seat.toLowerCase());
      }
      const completedModel = settled.filter((f: any) => f.kind === "model" && f.state === "completed");
      for (const f of completedModel) expect(f.counters).toEqual({ input_tokens: 1000, output_tokens: 7 });
      // Summaries are measured as compaction on the seat that compacted.
      expect(page.facts.filter((f: any) => f.kind === "compaction" && f.state === "started").length)
        .toBe(log.filter(d => d.seat === seat && d.kind === "summary").length);
      // Every model dispatch the provider saw for this seat has one fact.
      expect(page.facts.filter((f: any) => f.kind === "model" && f.state === "started").length)
        .toBe(log.filter(d => d.seat === seat && d.kind !== "summary").length);
    }
    // Every settle above came from the session's terminal agent_end. The
    // seat's idle fallback reports itself on stderr; it never ran.
    c.socket.end();
    proc.kill();
    await proc.exited;
    await stderrDone;
    expect(stderr).not.toContain("settled at idle without a terminal agent_end");
  } finally {
    c.socket.end();
    proc.kill();
    await proc.exited;
    if (process.env.T1074_DEBUG) console.error(stderr, JSON.stringify(dispatches(), null, 1));
  }
}, 90_000);

// Catch 2. Settings.isolated() is per instance, but the SDK's setting hooks
// write module-level state in pi-ai when ANY instance sets one of a few keys.
// The control specimen shows the leak on raw SDK instances; the seat's
// settings refuse those keys, and an ordinary per-seat key stays per seat.
test("process-global SDK settings are refused on a seat; per-seat settings stay per seat", async () => {
  const { Settings } = await import("@oh-my-pi/pi-coding-agent/config/settings");
  const { configureCredentialRedaction, redactSensitiveCredentials } = await import("@oh-my-pi/pi-ai/providers/transform-messages");
  const { Agent } = await import("@oh-my-pi/pi-agent-core");
  const { createMockModel, registerMockApi } = await import("@oh-my-pi/pi-ai/providers/mock");
  const { SessionManager } = await import("@oh-my-pi/pi-coding-agent/session/session-manager");
  const { createOmpSession, processGlobalSettingKeys } = await import("./omp-session.ts");
  const { History } = await import("./maintain.ts");
  const { ReceiptJournal } = await import("./receipts.ts");
  const { UsageJournal } = await import("./usage.ts");
  const secret = `sk-ant-${"Q7xP2mVt9LkR4wZs8NbY3hJc6FdG1aE5uK0oTiW".repeat(1)}`;
  try {
    // Control: two raw isolated instances; seat A's override changes what
    // seat B's outbound text becomes.
    const a = Settings.isolated({});
    const b = Settings.isolated({});
    expect(redactSensitiveCredentials(`B says ${secret}`)).toContain(secret);
    a.override("secrets.enabled", true);
    expect(b.get("secrets.enabled")).toBe(false);
    expect(redactSensitiveCredentials(`B says ${secret}`)).not.toContain(secret);
  } finally {
    configureCredentialRedaction(false);
  }
  registerMockApi();
  const root = mkdtempSync(join(tmpdir(), "cl-two-settings-"));
  dirs.push(root);
  const make = async (threshold: number) => {
    const agent = new Agent({ initialState: { model: createMockModel({}), systemPrompt: ["x"], tools: [] }, getApiKey: () => "t" });
    const history = new History();
    return createOmpSession({
      agent, sessionManager: SessionManager.inMemory(root), stateDir: root, sdkRoot: null, cwd: root,
      tokenFor: () => "t", sideStreamFn: agent.streamFn, nativeCompactionOptions: () => ({ fetch }),
      receipts: new ReceiptJournal(history), usage: new UsageJournal(history), usageAttribution: () => ({ requests: [] }),
      settings: { "compaction.thresholdTokens": threshold },
    });
  };
  const seatA = await make(6000);
  const seatB = await make(9000);
  try {
    expect(seatA.settings).not.toBe(seatB.settings);
    expect([seatA.settings.get("compaction.thresholdTokens"), seatB.settings.get("compaction.thresholdTokens")]).toEqual([6000, 9000]);
    seatA.settings.override("compaction.keepRecentTokens", 1234);
    expect(seatB.settings.get("compaction.keepRecentTokens")).toBe(20000);
    for (const key of processGlobalSettingKeys) {
      expect(() => seatA.settings.override(key as never, true as never)).toThrow(/process-wide/);
      expect(() => seatA.settings.set(key as never, true as never)).toThrow(/process-wide/);
    }
    expect(redactSensitiveCredentials(`B says ${secret}`)).toContain(secret);
    const agent = new Agent({ initialState: { model: createMockModel({}), systemPrompt: ["x"], tools: [] }, getApiKey: () => "t" });
    const history = new History();
    await expect(createOmpSession({
      agent, sessionManager: SessionManager.inMemory(root), stateDir: root, sdkRoot: null, cwd: root,
      tokenFor: () => "t", sideStreamFn: agent.streamFn, nativeCompactionOptions: () => ({ fetch }),
      receipts: new ReceiptJournal(history), usage: new UsageJournal(history), usageAttribution: () => ({ requests: [] }),
      settings: { "secrets.enabled": true },
    })).rejects.toThrow(/process-wide/);
  } finally {
    configureCredentialRedaction(false);
    await seatA.dispose();
    await seatB.dispose();
  }
});

// An abort that lands while the SDK compacts before a prompt makes
// AgentSession drop that prompt (promptCustomMessage resolves false). The
// accepted request never reached the engine; it must stay an obligation and
// be served in this process, after what was already queued.
test("a prompt the SDK drops during an aborted pre-prompt compaction is still served", async () => {
  const { createMockModel, registerMockApi } = await import("@oh-my-pi/pi-ai/providers/mock");
  const { createSeatAgent } = await import("./seat.ts");
  const { SeatStore } = await import("./store.ts");
  const { History } = await import("./maintain.ts");
  registerMockApi();
  const root = mkdtempSync(join(tmpdir(), "cl-dropped-"));
  dirs.push(root);
  const storage = new SeatStore(root, "worker", "session");
  const history = new History(storage);
  const large = "the quick brown fox jumps over the lazy dog. ".repeat(1200);
  for (let i = 0; i < 8; i++) history.sync([
    { role: "user", content: `${i}: ${large}`, timestamp: i * 2 },
    { role: "assistant", content: [{ type: "text", text: "ok" }], api: "mock", provider: "mock", model: "mock", timestamp: i * 2 + 1,
      usage: { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } }, stopReason: "stop" } as never,
  ]);
  history.persist();
  const preparing = Promise.withResolvers<void>();
  const releasePreparation = Promise.withResolvers<void>();
  const done = Promise.withResolvers<void>();
  const finished = new Set<string>();
  const model = createMockModel({ contextWindow: 60_000, handler: () => ({ content: ["ok"] }) });
  let preparations = 0;
  const meta = (id: string) => ({ request_id: id, session_id: "session", attempt_id: `attempt-${id}` });
  const agent = createSeatAgent({ provider: "mock", model: "mock", token: "t", cwd: root, store: storage, modelOverride: model,
    emit: e => {
      if (e.type === "receipt" && e.receipt?.state === "completed") finished.add(e.request_id!);
      if (finished.size === 3) done.resolve();
    }, callTool: async () => "", maintenance: {
      nativeEligible: () => false,
      compactImpl: (async (prep: any) => {
        if (++preparations === 1) {
          preparing.resolve();
          await releasePreparation.promise;
          throw new Error("cancelled preparation");
        }
        return { summary: "earlier work", firstKeptEntryId: prep.firstKeptEntryId, tokensBefore: prep.tokensBefore };
      }) as never,
    } });
  const first = agent.prompt("A", meta("A"));
  await preparing.promise;
  await agent.prompt("B", meta("B"));
  agent.abort();
  await agent.prompt("C", meta("C"));
  releasePreparation.resolve();
  await first;
  await Promise.race([done.promise, Bun.sleep(10_000).then(() => { throw new Error(`not all served: ${[...finished]}`); })]);
  for (const id of ["A", "B", "C"]) expect(agent.receipt(id).state).toBe("completed");
  const users = (m: any) => m.claudiaRequest?.request_id;
  const served = model.calls.map(c => c.context.messages.map(users).filter(Boolean).at(-1));
  expect(served.includes("A")).toBe(true);
  expect(served.indexOf("A")).toBeGreaterThan(served.indexOf("B"));
}, 20_000);
