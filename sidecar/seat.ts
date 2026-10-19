// Seat factory for the Oh My Pi sidecar (🎯T864.3).
// A prompt must call Agent.prompt on the pinned @oh-my-pi/pi-agent-core.

import { AuthRetry, authRefusal, type RetryReply } from "./authretry.ts";
import { Agent, unpairedToolCallTail, type AgentMessage, type AgentTool } from "@oh-my-pi/pi-agent-core";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import { streamSimple, type AssistantMessage, type CursorExecHandlers, type CursorMcpCall, type Model, type ToolResultMessage } from "@oh-my-pi/pi-ai";
import type { AgentSession } from "@oh-my-pi/pi-coding-agent/session/agent-session";
import { SessionManager } from "@oh-my-pi/pi-coding-agent/session/session-manager";
import { mkdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { classifyMessage, Flag, is as hasFlag } from "@oh-my-pi/pi-ai/error";
import crypto from "node:crypto";
import { cursorExecHandlers } from "./cursorexec.ts";
import { convertOmpMessages, createOmpSession } from "./omp-session.ts";
import { basename, dirname, isAbsolute, join } from "node:path";
import { codingTools, type SandboxPolicy } from "./coding.ts";
import { capToolText, toolResultBound } from "./truncate.ts";
import { awaitHostCall } from "./hostcalls.ts";
import { ContextOverflow, isOverflow, History } from "./maintain.ts";
import { seatRuntimeDirName, type SeatStore } from "./store.ts";
import { ReceiptJournal, type RequestReceipt, type ReceiptCause, type ReceiptMessage, type ReceiptTerminalState } from "./receipts.ts";
import { UsageJournal, measureDispatch, observedNativeCompaction, type UsageFact, type UsagePage } from "./usage.ts";
import {
  beginTurn,
  closeTurn,
  flushHold,
  noteDelta,
  noteTool,
  stripStopTokens,
  turnRefusal,
  type OpenTurn,
  type PromptMeta,
  type Stop,
} from "./turn.ts";

export type SeatEvent = {
  type: string;
  request_id?: string;
  rpc_id?: string;
  turn_id?: string;
  failed_token?: string;
  text?: string;
  call_id?: string;
  name?: string;
  // error is the provider's refusal on a turn_end that got no answer.
  error?: string;
  // reason classifies an error: "context_overflow" is terminal (🎯T148).
  reason?: string;
  snapshot?: unknown;
  receipt?: RequestReceipt;
  usage_fact?: UsageFact;
};

export type SeatEmit = (ev: SeatEvent) => void;
export type SeatCallTool = (callId: string, name: string, args: string) => Promise<string>;

export type SeatAgent = {
  authRetry: (reply: RetryReply) => void;
  prompt: (text: string, meta?: PromptMeta) => Promise<void>;
  steer: (text: string, meta?: PromptMeta) => void;
  abort: () => void;
  // cancelOpenReceipts finishes every started receipt as cancelled.
  // Abort, drop, and process exit call it. Accepted inputs stay accepted.
  cancelOpenReceipts: () => void;
  receipt: (requestId: string) => RequestReceipt;
  usage: (after?: number, limit?: number) => UsagePage;
  // switchModel moves the seat to another provider or model between turns,
  // keeping its conversation (🎯T195). It rejects while a turn runs, for an
  // unknown model, and when the conversation cannot be made to fit.
  switchModel: (provider: string, model: string) => Promise<void>;
  setToken: (token: string) => void;
  setCwd: (cwd: string) => void;
  // setSandbox replaces the policy on the seat's own tools (🎯T178).
  setSandbox: (policy: SandboxPolicy | undefined) => void;
  // sandboxMode is the policy mode in force, echoed on ready (🎯T178).
  sandboxMode: () => string | undefined;
  snapshot: () => unknown;
  // Read the model-visible list after setTools, not the host offer.
  boundToolNames: () => string[];
  // rebind points events and tool calls at the connection that just
  // loaded the seat. A broker restart dials a new socket; the Agent
  // stays, and the new socket has to hear it (🎯T868).
  rebind: (emit: SeatEmit, callTool: SeatCallTool) => void;
  // setHostTools replaces the host tools offered beside the coding tools.
  setHostTools: (tools: HostTool[] | undefined) => void;
  // restored says whether the seat resumed a stored conversation (🎯T151);
  // the host words its restart note by it (jevons 🎯T929).
  restored: boolean;
  // setContext replaces the host's compaction steer (🎯T152).
  setContext: (preserve: string | undefined, pins: string[] | undefined) => void;
  // sdk is the SDK session adapter once it exists (diagnostics and tests).
  sdk?: () => { session: AgentSession; settings: { get: (path: never) => unknown } } | undefined;
};

// HostTool is one tool the host offers a work seat (🎯T886): shown to the
// model with its own description and schema, executed by calling back into
// the host.
export type HostTool = {
  name: string;
  description?: string;
  input_schema?: Record<string, unknown>;
};

export function createSeatAgent(opts: {
  provider: string;
  model: string;
  token: string;
  cwd: string;
  emit: SeatEmit;
  callTool: SeatCallTool;
  tools?: HostTool[];
  // modelOverride replaces the catalog lookup (tests: a mock model).
  modelOverride?: Model;
  // resolveModelOverride replaces the catalog lookup a switch makes (tests).
  resolveModelOverride?: (provider: string, model: string) => Model;
  // The seat's own compaction seams are gone: the SDK session compacts.
  // Accepted and ignored so older callers still type-check.
  maintenance?: unknown;
  // store keeps the conversation across a sidecar restart (🎯T151). A seat
  // loaded with a store that already holds its session resumes from it.
  store?: SeatStore;
  requireResume?: boolean;
  // preserve and pins steered the seat's own compaction (🎯T152). The SDK
  // compactor has no per-call instructions; they are kept but unused.
  preserve?: string;
  pins?: string[];
  // sandbox limits the seat's own tools (🎯T178). Absent: unchanged.
  sandbox?: SandboxPolicy;
  // compactAtTokens is this seat's compaction threshold, as an SDK setting
  // (compaction.thresholdTokens) on this seat's session only.
  compactAtTokens?: number;
}): SeatAgent {
  // Resume admission belongs to the authoritative seat/session store. Validate
  // without repair before constructing an engine or changing any file.
  if (opts.requireResume) {
    const entries = opts.store?.load({ repairTail: false });
    const history = new History(opts.store);
    history.entries = entries ?? [];
    const pending = new ReceiptJournal(history).pending();
    if (history.rebuild(opts.provider).length === 0 && pending.length === 0) {
      throw new Error("existing conversation required for this seat and session; refusing to mint a replacement session");
    }
  }
  let sandbox = opts.sandbox;
  let token = opts.token;
  let cwd = opts.cwd;
  let sessionId = "";
  let turn: OpenTurn | null = null;
  const sink = { emit: opts.emit, callTool: opts.callTool };
  const recovery = new AuthRetry((ev) => sink.emit(ev));
  let attemptToken = "";
  let replayToken = "";
  let requestCount = 0;
  let retrying = false;
  let retryDone = Promise.resolve();
  let cancelled = false;
  let unsafe = false;
  let rootRequest: string | undefined;
  let currentRequests = new Set<string>();
  const cancelledTurns = new Set<string>();
  let turnDone: Promise<void> | undefined;
  const answerRequestID = () => {
    if (!turn) return undefined;
    let id = "";
    for (const next of currentRequests) id = next;
    return id || rootRequest || undefined;
  };
  // History stays: it is the claudia_seat store the host, the receipt and
  // usage projections, generation discovery and the historical usage reader
  // all read. The SDK session's own SessionManager is in memory and is
  // seeded from it on load.
  const history = new History(opts.store);
  const restored = history.restore();
  const receipts = new ReceiptJournal(history);
  const usage = new UsageJournal(history,
    fact => sink.emit({ type: "usage", usage_fact: fact }),
    text => sink.emit({ type: "usage_error", text }));
  const usageAttribution = () => {
    const ids = new Set(turn && !turn.closed ? currentRequests : []);
    if (rootRequest && turn && !turn.closed) ids.add(rootRequest);
    return {
      turn_id: turn && !turn.closed ? turn.turn_id : undefined,
      cause: turn && !turn.closed ? turn.cause : undefined,
      requests: [...ids].map(id => receipts.query(id)).filter(r => r.attempt_id).map(r => ({ request_id: r.request_id, attempt_id: r.attempt_id })),
    };
  };
  const model = opts.modelOverride ?? resolveModel(opts.provider, opts.model);
  const execHandlers = cursorExecHandlers(
    () => cwd,
    () => toolResultBound(agent.state.model?.contextWindow ?? 0),
    () => sandbox,
  );
  bindListedMcp(execHandlers, () => agent.state.tools);
  const countedToken = () => {
    attemptToken = replayToken || token;
    replayToken = "";
    requestCount++;
    return attemptToken;
  };
  const agent = new Agent({
    initialState: {
      systemPrompt: [
        "You are a coding agent hosted by Claudia.",
        "You have Bash, Read, Write, Glob, and Grep in the seat working directory.",
        "Your host may add its own tools for fleet actions such as messaging or starting agents; use those to act, not prose that names them.",
        "Use them. Do not emit XML tool_call prose.",
      ],
      model,
    },
    cwd,
    cwdResolver: () => cwd || undefined,
    // The SDK binding wraps this with usage measurement and the host's
    // Cursor exec handlers.
    streamFn: streamSimple,
    cursorExecHandlers: execHandlers,
    getApiKey: async () => countedToken(),
    convertToLlm: convertOmpMessages,
    resolveFallbackTool: (_name: string) => undefined,
  });
  const boundOf = () => toolResultBound(agent.state.model?.contextWindow ?? 0);
  const callBack = (id: string, toolName: string, args: string) => sink.callTool(id, toolName, args);
  let ownToolNames = new Set<string>();
  let lastHost: HostTool[] | undefined;
  let session: AgentSession | undefined;
  const applyTools = (host: HostTool[] | undefined) => {
    lastHost = host;
    const hosted = (host ?? []).map((t) => jevonsTool(t.name, callBack, t.description, t.input_schema, boundOf));
    const own = codingTools(() => cwd, boundOf, () => sandbox);
    ownToolNames = new Set(own.map((t) => t.name));
    agent.setTools([...own, ...hosted]);
  };
  applyTools(opts.tools);
  if (restored) agent.replaceMessages(history.rebuild(model.provider));
  const uncertainToolRestore = restored && unpairedToolCallTail(agent.state.messages) !== undefined;
  const assertRestorableExecution = () => {
    if (uncertainToolRestore) throw Object.assign(
      new Error("restored history has unfinished tool calls; inspect their effects before starting a fresh session"),
      { reason: "execution_uncertain" },
    );
  };
  if (restored) {
    console.error(
      `sidecar: resumed ${opts.store?.path}: ${history.entries.length} entries, ${agent.state.messages.length} messages in context`,
    );
  }

  const finish = (stop: Stop) => {
    if (!turn || turn.closed) return;
    const tail = flushHold(turn);
    if (tail) sink.emit({ type: "text", text: tail, turn_id: turn.turn_id, request_id: answerRequestID() });
    const digest = closeTurn(turn, stop);
    sink.emit(digest);
  };

  // Queued inputs the model has not taken yet, keyed by request id (or the
  // message itself when the input carries none).
  const pending = new Set<unknown>();
  const heldSteering: AgentMessage[] = [];
  const refusalHeld = { steer: [] as AgentMessage[], followUp: [] as AgentMessage[] };
  const heldFollowUps: AgentMessage[] = [];

  // Accepted inputs that never reached dispatch remain obligations after a
  // restart. Rebuild only that queue; a started/unknown request is never
  // automatically submitted again.
  const recovered = receipts.pending();
  const recoveredIDs = new Set(recovered.map(item => item.receipt.request_id));
  if (recoveredIDs.size > 0) {
    agent.replaceMessages(agent.state.messages.filter(message => {
      const id = requestIdOf(message);
      if (!id || !recoveredIDs.has(id)) return true;
      history.forget(message);
      return false;
    }));
    history.persist(agent.state.messages);
    for (const item of recovered) {
      const message = hostMessage(item.message);
      pending.add(item.receipt.request_id);
      if (item.delivery === "steer") agent.steer(message);
      else agent.followUp(message);
    }
  }

  const settleRequests = (ids: Iterable<string>, state: ReceiptTerminalState, reason?: string, cause?: ReceiptCause) => {
    const started = [...ids].filter(id => receipts.query(id).state === "started");
    if (started.length === 0) return;
    receipts.finish(started, state, reason, cause);
    for (const id of started) sink.emit({ type: "receipt", request_id: id, receipt: receipts.query(id) });
  };
  const cancelOpenReceipts = () => {
    const ids = receipts.startedIDs();
    if (ids.length === 0) return;
    receipts.finish(ids, "cancelled", "cancelled", "cancellation");
    for (const id of ids) sink.emit({ type: "receipt", request_id: id, receipt: receipts.query(id) });
  };
  // The request a model call carries, read from the prepared input. The
  // receipt start itself belongs to the SDK binding (omp-session.ts).
  agent.addBeforeModelCall((context) => {
    receipts.assertHealthy();
    for (const message of context.messages) {
      const id = (message as ReceiptMessage).claudiaRequest?.request_id;
      if (!id) continue;
      const state = receipts.query(id).state;
      if (state === "accepted" || state === "started") currentRequests.add(id);
    }
    // Fail-closed persistence boundary before dispatch.
    history.persist(agent.state.messages);
  });

  // Failure classification shared by the SDK settle and the turn end.
  const classify = (messages: readonly AgentMessage[]): { state: ReceiptTerminalState; reason?: string; cause?: ReceiptCause } => {
    if (turn && cancelledTurns.has(turn.turn_id)) return { state: "cancelled", reason: "cancelled", cause: "cancellation" };
    const last = [...messages].reverse().find(m => m.role === "assistant") as AssistantMessage | undefined;
    if (last?.stopReason === "aborted") return { state: "cancelled", reason: "cancelled", cause: "cancellation" };
    if (!last || (last.stopReason !== "error" && !last.errorMessage)) return { state: "completed" };
    const errorID = classifyMessage(last);
    const refusal = last.errorMessage ?? "provider error";
    if (isOverflow(last, agent.state.model?.contextWindow ?? 0)) return { state: "failed", reason: ContextOverflow, cause: "failure" };
    if (authRefusal(refusal) || hasFlag(errorID, Flag.AuthFailed) || hasFlag(errorID, Flag.OAuthExpiry)) return { state: "failed", reason: "auth_unavailable", cause: "auth" };
    if (hasFlag(errorID, Flag.UsageLimit)) return { state: "failed", reason: "capacity_unavailable", cause: "capacity" };
    if (hasFlag(errorID, Flag.Timeout)) return { state: "failed", reason: "provider_timeout", cause: "failure" };
    if (hasFlag(errorID, Flag.Transient)) return { state: "failed", reason: "provider_unavailable", cause: "failure" };
    return { state: "failed", reason: "provider_refusal", cause: "failure" };
  };

  agent.subscribe((event: {
    type?: string;
    assistantMessageEvent?: { type?: string; delta?: string };
    message?: AgentMessage & { content?: unknown };
    toolCallId?: string;
    toolName?: string;
    args?: unknown;
  }) => {
    if (event.type === "tool_execution_start" && event.toolName && ownToolNames.has(event.toolName)) {
      sink.emit({ type: "tool_start", call_id: event.toolCallId ?? "", name: event.toolName, text: JSON.stringify(event.args ?? {}) });
    }
    if (event.type === "message_start" && event.message && (event.message.role === "user" || event.message.role === "custom")) {
      const text = userText(event.message.content);
      const id = requestIdOf(event.message);
      if (id) currentRequests.add(id);
      if (pending.delete(id ?? event.message)) {
        sink.emit({ type: "absorbed", text, request_id: id, turn_id: turn?.turn_id });
      }
      return;
    }
    // A provider refusal ends the request. AgentSession would continue
    // into queued input after it (a queued continuation); Claudia leaves
    // that input accepted for a later prompt, so a capacity or auth wall does
    // not burn it. Hold the queues until the turn has unwound. An overflow
    // is the session's to compact and retry; its queues stay.
    if (event.type === "agent_end") {
      const last = agent.state.messages.at(-1) as AssistantMessage | undefined;
      if (last?.role === "assistant" && last.stopReason === "error" && !isOverflow(last, agent.state.model?.contextWindow ?? 0) &&
          (agent.peekSteeringQueue().length || agent.peekFollowUpQueue().length)) {
        refusalHeld.steer.push(...agent.peekSteeringQueue());
        refusalHeld.followUp.push(...agent.peekFollowUpQueue());
        agent.replaceQueues([], []);
      }
    }
    if (event.type === "message_end") history.persist(agent.state.messages);
    if (event.type === "message_end" && event.message?.role === "assistant" && Array.isArray(event.message.content)) {
      if (event.message.content.some((part: any) => part?.type === "toolCall" || (part?.type === "text" && part.text))) unsafe = true;
    }
    if (event.assistantMessageEvent?.type === "toolcall_start" ||
        event.assistantMessageEvent?.type === "text_delta" || event.type === "tool_execution_start") unsafe = true;
    if (event.type === "tool_execution_start" && turn && !turn.closed) {
      noteTool(turn);
      return;
    }
    if (event.type === "message_update" && event.assistantMessageEvent?.type === "text_delta") {
      const delta = event.assistantMessageEvent.delta ?? "";
      if (turn && !turn.closed) {
        const visible = noteDelta(turn, delta);
        if (visible) sink.emit({ type: "text", text: visible, turn_id: turn.turn_id, request_id: answerRequestID() });
        return;
      }
      const stripped = stripStopTokens(delta);
      if (stripped.visible) sink.emit({ type: "text", text: stripped.visible, turn_id: turn?.turn_id });
    }
  });

  // The SDK session hosts the loop and owns compaction (🎯T150 moved to the
  // SDK). A seat without an absolute cwd still gets one, rooted at the
  // process directory, since the session manager needs an absolute cwd.
  const sessionCwd = isAbsolute(cwd) ? cwd : process.cwd();
  const sessionManager = SessionManager.inMemory(sessionCwd);
  seedSessionManager(sessionManager, agent.state.messages);
  const stateDir = opts.store
    // Beside the seat directory, not inside it. A file inside the seat
    // directory is conversation history to the host, and generation
    // discovery reports any non-journal entry as an incomplete inventory.
    ? join(dirname(dirname(opts.store.path)), seatRuntimeDirName, basename(dirname(opts.store.path)))
    : join(tmpdir(), "claudia-omp-runtime", crypto.randomUUID());
  mkdirSync(stateDir, { recursive: true });
  const settings: Record<string, unknown> = {};
  if (typeof opts.compactAtTokens === "number" && opts.compactAtTokens > 0) settings["compaction.thresholdTokens"] = opts.compactAtTokens;
  const executionRequests = new Set<string>();
  // The SDK replaces the agent's messages with its session manager's context,
  // which drops a trailing refused turn. continue() would then re-dispatch
  // the refused request. Keep the context the store restored.
  const restoredContext = [...agent.state.messages];
  let adapter: Awaited<ReturnType<typeof createOmpSession>> | undefined;
  const sessionReady = createOmpSession({
    agent,
    sessionManager,
    stateDir,
    // The SDK resolves its directories once per process. Isolation of those
    // directories is the process owner's (CLAUDIA_OMP_SDK_ROOT); without it
    // they are the user's own, as on the stacked adoption this replaces.
    sdkRoot: process.env.CLAUDIA_OMP_SDK_ROOT || null,
    cwd: sessionCwd,
    tokenFor: () => replayToken || token,
    sideStreamFn: measureDispatch(() => usage.durable ? usage : undefined, usageAttribution, streamSimple, "compaction"),
    nativeCompactionOptions: usage.durable
      ? observedNativeCompaction(usage, usageAttribution, fetch)
      : () => ({ fetch }),
    cursorExecHandlers: execHandlers,
    receipts,
    usage,
    usageAttribution,
    settings,
    turnId: () => turn && !turn.closed ? turn.turn_id : "",
    onReceipt: (id, phase) => {
      if (phase === "started") {
        executionRequests.add(id);
        sink.emit({ type: "receipt", request_id: id, turn_id: turn?.turn_id, receipt: receipts.query(id) });
      } else {
        sink.emit({ type: "receipt", request_id: id, receipt: receipts.query(id) });
      }
    },
    settle: (messages) => {
      const verdict = classify(messages);
      // A first-attempt auth refusal is the seat's to retry with a rotated
      // token (AuthRetry). The request is not over until that retry is.
      if (verdict.cause === "auth" && requestCount === 1 && !unsafe && !cancelled) return undefined;
      return verdict;
    },
  }).then(created => {
    agent.replaceMessages(restoredContext);
    adapter = created;
    session = created.session;
    // Count each dispatch's credential for auth recovery; keep the private
    // registry in step with the broker as the SDK binding does.
    agent.getApiKey = async (m) => {
      created.syncToken(m.provider);
      return countedToken();
    };
    session.subscribe(event => {
      if (event.type !== "auto_compaction_end" || event.aborted || !event.result) return;
      sink.emit({ type: "compacted", text: `${event.action}: context ${event.result.tokensBefore} tokens before` });
      // Mirror the SDK's compaction into the durable store, so a restart
      // rebuilds the compacted context and does not resend what was folded.
      history.sync(agent.state.messages.filter(m => m.role !== "compactionSummary"));
      const firstKept = agent.state.messages.find(m => m.role !== "compactionSummary");
      const result = event.result as unknown as Parameters<History["appendCompaction"]>[0];
      history.appendCompaction({ ...result, firstKeptEntryId: (firstKept && history.idOf(firstKept)) ?? "" },
        event.action === "remote" ? "remote" : "soft");
      history.persist();
    });
    session.subscribe(event => {
      if (event.type === "auto_compaction_start") sink.emit({ type: "compacting", text: `${event.reason}: ${event.action}` });
    });
  });
  let sessionError: unknown;
  sessionReady.catch(err => { sessionError = err; });
  const ready = async () => {
    if (!adapter) await sessionReady;
    if (sessionError) throw sessionError;
    return adapter!;
  };

  const executeTurn = async (text: string, meta: PromptMeta | undefined, run: () => Promise<unknown>, accepted?: RequestReceipt) => {
    assertRestorableExecution();
    if (!adapter) await ready();
    if (sessionError) throw sessionError;
    const { session } = adapter!;
    const metaSession = meta?.session_id || sessionId;
    turn = beginTurn({
      seat: "",
      text,
      meta: { ...meta, session_id: metaSession || undefined },
    });
    sessionId = turn.session_id;
    const executionTurn = turn;
    currentRequests = new Set<string>();
    executionRequests.clear();
    rootRequest = meta?.request_id;
    attemptToken = "";
    replayToken = "";
    requestCount = 0;
    cancelled = false;
    unsafe = false;
    if (accepted) sink.emit({ type: "accepted", request_id: accepted.request_id, rpc_id: meta?.rpc_id, receipt: accepted, turn_id: turn.turn_id });
    else if (!meta?.request_id) sink.emit({ type: "accepted", turn_id: turn.turn_id });
    let refusal = "";
    let runInvoked = false;
    let retried = false;
    try {
      const before = new Set(agent.state.messages);
      runInvoked = true;
      await run();
      await session.waitForIdle();
      if (refusalHeld.steer.length || refusalHeld.followUp.length) {
        agent.replaceQueues([...refusalHeld.steer.splice(0), ...agent.peekSteeringQueue()], [...refusalHeld.followUp.splice(0), ...agent.peekFollowUpQueue()]);
      }
      const failed = agent.state.messages.at(-1);
      const authError = turnRefusal(agent.state);
      if (failed && !before.has(failed) && authRefusal(authError) && requestCount === 1 &&
          attemptToken && !unsafe && !cancelled && !turn.closed && turn.tool_calls === 0 && turn.chars === 0) {
        retrying = true;
        let releaseRetry!: () => void;
        retryDone = new Promise<void>(resolve => { releaseRetry = resolve; });
        const steering = [...agent.peekSteeringQueue()];
        const followUps = [...agent.peekFollowUpQueue()];
        agent.replaceQueues([], []);
        try {
          const replacement = await recovery.recover(turn.turn_id, attemptToken, authError);
          if (replacement && !cancelled && !turn.closed && agent.state.messages.at(-1) === failed) {
            token = replacement;
            replayToken = replacement;
            agent.popMessage();
            history.forget(failed as never);
            retried = true;
            await agent.continue();
            await session.waitForIdle();
          }
        } finally {
          agent.replaceQueues([...steering, ...agent.peekSteeringQueue(), ...heldSteering.splice(0)], [...followUps, ...agent.peekFollowUpQueue(), ...heldFollowUps.splice(0)]);
          retrying = false;
          releaseRetry();
        }
      }
      // Input queued behind the seat's own auth retry, or after the session's
      // last queue poll, is answered in this turn (bounded), as before.
      for (let i = 0; i < 8 && !cancelled && !turnRefusal(agent.state) && agent.hasQueuedMessages() && !agent.state.isStreaming; i++) {
        await agent.continue();
        await session.waitForIdle();
      }
      refusal = turnRefusal(agent.state);
      finish(refusal ? "error" : turn?.stop_token ? "stop_token" : "end_turn");
    } catch (err) {
      if (!runInvoked && accepted) {
        const waiting = receipts.pending().find(item => item.receipt.request_id === accepted.request_id);
        if (waiting) {
          agent.followUp(hostMessage(waiting.message));
          pending.add(waiting.receipt.request_id);
        }
      }
      const wasCancelled = cancelledTurns.has(executionTurn.turn_id);
      settleRequests(executionRequests, wasCancelled ? "cancelled" : "failed",
        wasCancelled ? "cancelled" : "execution_error", wasCancelled ? "cancellation" : "failure");
      cancelledTurns.delete(executionTurn.turn_id);
      finish("error");
      throw err;
    } finally {
      receipts.assertHealthy();
      history.persist(agent.state.messages);
    }
    // The SDK settles on its terminal agent_end. Anything this turn started
    // that is still open settles here: the seat's own auth retry, or an SDK
    // path that ended non-terminally and never ended again (seen with the
    // handoff compaction method, now excluded). Say so: it is not the
    // expected path.
    const verdict = classify(agent.state.messages);
    const unsettled = [...executionRequests].filter(id => receipts.query(id).state === "started");
    if (unsettled.length > 0 && !retried) console.error(`sidecar: settled at idle without a terminal agent_end: ${unsettled.join(", ")}`);
    settleRequests(executionRequests, verdict.state, verdict.reason, verdict.cause);
    const overflow = verdict.reason === ContextOverflow;
    cancelledTurns.delete(executionTurn.turn_id);
    if (cancelled) return;
    if (refusal) {
      sink.emit({ type: "turn_end", turn_id: turn.turn_id, request_id: answerRequestID(), error: refusal, reason: overflow ? ContextOverflow : undefined, snapshot: agent.state });
      return;
    }
    sink.emit({ type: "turn_end", turn_id: turn.turn_id, request_id: answerRequestID(), snapshot: agent.state });
  };

  const runTurn = async (text: string, meta: PromptMeta | undefined, run: () => Promise<unknown>, accepted?: RequestReceipt) => {
    if (turnDone) throw new Error("a turn is still unwinding");
    let releaseTurn!: () => void;
    turnDone = new Promise<void>(resolve => { releaseTurn = resolve; });
    try {
      await executeTurn(text, meta, run, accepted);
    } finally {
      turnDone = undefined;
      releaseTurn();
    }
  };
  const busy = () => Boolean(turnDone || retrying || agent.state.isStreaming || session?.isStreaming);

  return {
    receipt: (requestId) => receipts.query(requestId),
    usage: (after, limit) => {
      if (!opts.store) throw new Error("usage accounting unavailable: no durable session store");
      return usage.page(after, limit);
    },
    authRetry: (reply) => recovery.receive(reply),
    prompt: async (text: string, meta?: PromptMeta) => {
      // A resolved promise still yields. Mid-turn prompt and steer keep their
      // accepted order, so a prompt does not await once the session exists.
      if (!adapter) await ready();
      if (sessionError) throw sessionError;
      const { session } = adapter!;
      receipts.assertHealthy();
      assertRestorableExecution();
      const accepted = meta?.request_id ? receipts.accept(text, meta as typeof meta & { request_id: string; session_id: string }, "prompt") : undefined;
      if (accepted && !accepted.fresh) {
        sink.emit({ type: "accepted", request_id: meta!.request_id, rpc_id: meta?.rpc_id, receipt: accepted.receipt });
        if (accepted.receipt.state === "accepted" && !busy() && agent.hasQueuedMessages()) {
          await runTurn("", meta, () => agent.continue());
        }
        return;
      }
      const message = accepted ? hostMessage(accepted.message) : plainMessage(text);
      if (busy() || agent.hasQueuedMessages()) {
        if (retrying) heldFollowUps.push(message);
        else agent.followUp(message);
        pending.add(accepted ? accepted.receipt.request_id : message);
        sink.emit({ type: "accepted", request_id: meta?.request_id, rpc_id: meta?.rpc_id, receipt: accepted?.receipt });
        if (!busy()) await runTurn("", meta, () => agent.continue());
        return;
      }
      await runTurn(text, meta, () => message.role === "custom"
        ? session.promptCustomMessage(message as never)
        : session.prompt(text, { expandPromptTemplates: false, attribution: "user" }), accepted?.receipt);
    },
    steer: (text: string, meta?: PromptMeta) => {
      receipts.assertHealthy();
      assertRestorableExecution();
      const accepted = meta?.request_id ? receipts.accept(text, meta as typeof meta & { request_id: string; session_id: string }, "steer") : undefined;
      if (accepted && !accepted.fresh) {
        sink.emit({ type: "accepted", request_id: meta!.request_id, rpc_id: meta?.rpc_id, receipt: accepted.receipt });
        return;
      }
      const message = accepted ? hostMessage(accepted.message) : plainMessage(text);
      if (retrying) heldSteering.push(message);
      else agent.steer(message);
      pending.add(accepted ? accepted.receipt.request_id : message);
      sink.emit({ type: "accepted", request_id: meta?.request_id, rpc_id: meta?.rpc_id, receipt: accepted?.receipt });
    },
    cancelOpenReceipts,
    abort: () => {
      cancelOpenReceipts();
      const interruptedTurn = turnDone ?? Promise.resolve();
      cancelled = true;
      if (turn && !turn.closed) cancelledTurns.add(turn.turn_id);
      recovery.cancel();
      if (session) void session.abort().catch(err => sink.emit({ type: "error", text: err instanceof Error ? err.message : String(err) }));
      else agent.abort();
      finish("abort");
      // The queues survive abort; once the aborted run has unwound, run what
      // is queued as its own turn instead of leaving it for a later prompt.
      void agent.waitForIdle().then(async () => {
        await ready();
        await session?.waitForIdle();
        await interruptedTurn;
        await retryDone;
        if (!agent.hasQueuedMessages() || busy()) return;
        try {
          await runTurn("", { cause: "interrupt-deliver" }, () => agent.continue());
        } catch (err) {
          sink.emit({ type: "error", text: err instanceof Error ? err.message : String(err) });
        }
      });
    },
    switchModel: async (provider: string, modelId: string) => {
      const created = await ready();
      assertRestorableExecution();
      if (busy()) {
        throw new Error("switch refused: a turn is running");
      }
      const next = (opts.resolveModelOverride ?? resolveModel)(provider, modelId);
      // The SDK's model switch validates a credential for the target first.
      // There is no fit-compaction on switch: the next prompt's compaction
      // or overflow path owns an oversized transcript (SDK setModel contract).
      created.syncToken(next.provider);
      await created.session.setModel(next, "default", { persist: false });
    },
    setToken: (next: string) => {
      token = next;
    },
    setCwd: (next: string) => {
      if (next) cwd = next;
    },
    sandboxMode: () => sandbox?.mode,
    setSandbox: (next) => {
      sandbox = next;
      applyTools(lastHost);
    },
    snapshot: () => agent.state,
    boundToolNames: () => (agent.state.tools ?? []).map((tool) => tool.name),
    rebind: (emit, callTool) => {
      sink.emit = emit;
      sink.callTool = callTool;
    },
    setHostTools: (tools) => applyTools(tools),
    restored,
    setContext: (_preserve, _pins) => {},
    sdk: () => adapter,
  };
}

// hostMessage is a host request as the SDK carries it: a custom message whose
// details hold the durable request identity. convertOmpMessages turns it back
// into the ReceiptMessage the model sees.
function hostMessage(message: ReceiptMessage): AgentMessage {
  return {
    role: "custom",
    customType: "claudia-request",
    content: message.content,
    display: true,
    attribution: "user",
    details: { ...message.claudiaRequest },
    timestamp: message.timestamp,
  } as AgentMessage;
}

function plainMessage(text: string): AgentMessage {
  return { role: "user", content: text, attribution: "user", timestamp: Date.now() } as AgentMessage;
}

function requestIdOf(message: unknown): string | undefined {
  const m = message as { role?: string; customType?: string; details?: { request_id?: unknown }; claudiaRequest?: { request_id?: string } };
  if (m?.role === "custom" && m.customType === "claudia-request" && typeof m.details?.request_id === "string") return m.details.request_id;
  return m?.claudiaRequest?.request_id;
}

// Seed the SDK's in-memory session manager with the restored context: the
// kept messages, then the compaction that summarizes what came before them.
function seedSessionManager(manager: SessionManager, messages: readonly AgentMessage[]) {
  let summary: (AgentMessage & { summary?: string; shortSummary?: string; tokensBefore?: number }) | undefined;
  let first: string | undefined;
  for (const message of messages) {
    if (message.role === "compactionSummary") {
      summary = message as typeof summary;
      continue;
    }
    const id = manager.appendMessage(message as never);
    first ??= id;
  }
  if (summary && first) {
    manager.appendCompaction(summary.summary ?? "", summary.shortSummary, first, summary.tokensBefore ?? 0);
  }
}

// userText flattens a user message's content to its text.
function userText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .map((c) => (c && typeof c === "object" && (c as { type?: string }).type === "text" ? String((c as { text?: unknown }).text ?? "") : ""))
    .join("");
}

function resolveModel(provider: string, model: string) {
  if (!model) {
    throw new Error("load requires a model id");
  }
  const found = getBundledModel(provider as never, model);
  if (!found) {
    throw new Error(`unknown model ${provider}/${model}`);
  }
  return found;
}

// Cursor delivers a model-listed tool as an MCP exec frame on the open
// stream, and runs it here rather than in the agent loop. The lookup is
// live: the host can replace the tool list after the seat is built.
function bindListedMcp(handlers: CursorExecHandlers, toolsOf: () => AgentTool[]): void {
  const answer = (id: string, name: string, text: string, isError: boolean): ToolResultMessage => ({
    role: "toolResult",
    toolCallId: id,
    toolName: name,
    content: [{ type: "text", text }],
    isError,
    timestamp: Date.now(),
  });
  handlers.mcp = async (call: CursorMcpCall) => {
    const name = call.toolName || call.name;
    const id = call.toolCallId || crypto.randomUUID();
    const tool = toolsOf().find((candidate) => candidate.name === name);
    if (!tool) return answer(id, name, `Tool ${name} is not available`, true);
    const executed = await tool.execute(id, call.args ?? {});
    const text = executed.content.filter((block) => block.type === "text").map((block) => block.text).join("");
    return answer(id, name, text, executed.isError === true);
  };
  handlers.mcpApprovalPreflight = async (call) => toolsOf().some((candidate) => candidate.name === (call.toolName || call.name));
}

function jevonsTool(
  name: string,
  callTool: (callId: string, name: string, args: string) => Promise<string>,
  description?: string,
  schema?: Record<string, unknown>,
  bound: () => number = () => toolResultBound(0),
): AgentTool {
  return {
    name,
    label: name,
    description: description || "Jevons host tool; execute calls back into Go",
    parameters: schema && typeof schema === "object" ? schema : { type: "object" },
    // The signal is the turn's: an aborted turn stops waiting for a host
    // that may never answer (jevons 🎯T927).
    execute: async (toolCallId: string, params: unknown, signal?: AbortSignal) => {
      const result = await awaitHostCall(callTool(toolCallId, name, JSON.stringify(params ?? {})), signal);
      // Truncate only the conversation copy (🎯T154). The host already has
      // the full result; this does not rewrite what it logged.
      return { content: [{ type: "text", text: capToolText(result, bound(), name) }], details: {} };
    },
  } as AgentTool;
}
