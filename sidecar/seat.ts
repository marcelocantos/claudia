// Seat factory for the Oh My Pi sidecar (🎯T864.3).
// A prompt must call Agent.prompt on the pinned @oh-my-pi/pi-agent-core.

import { AuthRetry, authRefusal, type RetryReply } from "./authretry.ts";
import { kCursorExecResolved } from "@oh-my-pi/pi-ai/utils/block-symbols";

export function markRestoredCursorToolsResolved(messages: readonly unknown[]): void {
  for (const message of messages) {
    const m = message as { role?: string; provider?: string; stopReason?: string; content?: unknown };
    if (m?.role !== "assistant" || m.provider !== "cursor" || m.stopReason !== "stop" || !Array.isArray(m.content)) continue;
    for (const part of m.content as Array<Record<PropertyKey, unknown>>) {
      if (part?.type === "toolCall") part[kCursorExecResolved] = true;
    }
  }
}

// See the retry in the turn loop: one plain retry after a short pause, then
// compaction, then the turn ends hollow and the host sees it.
const hollowLengthRetries = 2;
const hollowLengthBackoffMs = 500;
export function hollowLengthStop(message: unknown): boolean {
  const m = message as { role?: string; stopReason?: string; content?: unknown; usage?: { totalTokens?: number; output?: number } } | undefined;
  if (!m || m.role !== "assistant" || m.stopReason !== "length") return false;
  if ((m.usage?.totalTokens ?? 0) !== 0 || (m.usage?.output ?? 0) !== 0) return false;
  const parts = Array.isArray(m.content) ? m.content : [];
  return parts.every((p: { type?: string; text?: string; thinking?: string }) =>
    p?.type !== "toolCall" && !(p?.text ?? "").trim() && !(p?.thinking ?? "").trim());
}
import { Agent, unpairedToolCallTail, type AgentMessage, type AgentTool } from "@oh-my-pi/pi-agent-core";
import { savedTranscriptEndsMidTurn } from "./transcript-end.ts";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import type { AssistantMessage, CursorExecHandlers, CursorMcpCall, Model, ToolResultMessage } from "@oh-my-pi/pi-ai";
import { classifyMessage, Flag, is as hasFlag } from "@oh-my-pi/pi-ai/error";
import crypto from "node:crypto";
import { cursorExecHandlers } from "./cursorexec.ts";
import { SeatProcessOwnership } from "./killguard.ts";
import { codingTools, type SandboxPolicy } from "./coding.ts";
import { capToolText, toolResultBound } from "./truncate.ts";
import { awaitHostCall } from "./hostcalls.ts";
import { ContextOverflow, isOverflow, History, Maintenance, seatConvertToLlm, type MaintenanceHost } from "./maintain.ts";
import { resetOpenAICodexHistoryAfterCompaction } from "@oh-my-pi/pi-ai/providers/openai-codex-responses";
import type { SeatStore } from "./store.ts";
import { ReceiptJournal, type RequestReceipt, type ReceiptCause, type ReceiptMessage, type ReceiptTerminalState } from "./receipts.ts";
import { UsageJournal, measuredStream, measuredCompletion, type UsageFact, type UsagePage } from "./usage.ts";
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

// EndedTurn identifies the turn an abort closed.
export type EndedTurn = { turn_id: string; request_id?: string };

// TurnError is a turn's own failure after it began: the host hears it as that
// turn's error, not as a reply to the request that opened it (whose waiter
// has already taken the acceptance).
export type TurnError = Error & { turn_id: string };

export function turnOf(err: unknown): string | undefined {
  const id = (err as { turn_id?: unknown } | null)?.turn_id;
  return typeof id === "string" && id.trim() ? id : undefined;
}
export type SeatCallTool = (callId: string, name: string, args: string) => Promise<string>;

export type SeatAgent = {
  authRetry: (reply: RetryReply) => void;
  prompt: (text: string, meta?: PromptMeta) => Promise<void>;
  steer: (text: string, meta?: PromptMeta) => void;
  // abort ends the open turn and names it, so the host hears that turn end
  // as itself; undefined when no turn was open, and nothing ended.
  abort: () => EndedTurn | undefined;
  stopTurn: () => EndedTurn | undefined;
  // cancelOpenReceipts finishes every started receipt as cancelled.
  // Abort, drop, and process exit call it. Accepted inputs stay accepted.
  cancelOpenReceipts: () => void;
  receipt: (requestId: string) => RequestReceipt;
  usage: (after?: number, limit?: number) => UsagePage;
  // switchModel moves the seat to another provider or model between turns,
  // keeping its conversation (🎯T195). It rejects while a turn runs, for an
  // unknown model, and when the conversation cannot be made to fit.
  switchModel: (provider: string, model: string) => Promise<void>;
  // compactAtBound folds toward boundTokens between turns (🎯T244).
  compactAtBound: (
    boundTokens: number,
    opts?: { cancel?: AbortSignal },
  ) => Promise<import("./maintain.ts").BoundCompactResult>;
  setToken: (token: string) => void;
  setCwd: (cwd: string) => void;
  // setSandbox replaces the policy on the seat's own tools (🎯T178).
  setSandbox: (policy: SandboxPolicy | undefined) => void;
  setProtectedPaths: (rules: ProtectedPathRule[] | undefined) => void;
  // sandboxMode is the policy mode in force, echoed on ready (🎯T178).
  sandboxMode: () => string | undefined;
  snapshot: () => unknown;
  // Read the model-visible list after setTools, not the host offer.
  boundToolNames: () => string[];
  // openTurnID names the turn presently open on this seat, or undefined
  // when none is (🎯T202). The host reads it on ready (adopt, and load of
  // an already-running seat) so a reattach mid-turn can report in-flight
  // from its very first read, instead of waiting for a later
  // accepted/receipt event the sidecar emits only once per turn-start.
  openTurnID: () => string | undefined;
  // rebind points events and tool calls at the connection that just
  // loaded the seat. A broker restart dials a new socket; the Agent
  // stays, and the new socket has to hear it (🎯T868).
  rebind: (emit: SeatEmit, callTool: SeatCallTool) => void;
  // setHostTools replaces the host tools offered beside the coding tools.
  setHostTools: (tools: HostTool[] | undefined) => void;
  // restored says whether the seat resumed a stored conversation (🎯T151);
  // the host words its restart note by it (jevons 🎯T929).
  restored: boolean;
  // transcriptEndsMidTurn says whether that store ended inside a turn (🎯T214).
  transcriptEndsMidTurn: boolean;
  // setContext replaces the host's compaction steer (🎯T152).
  setContext: (preserve: string | undefined, pins: string[] | undefined) => void;
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
  // modelOverride replaces the catalog lookup, and maintenance the
  // compaction seams (🎯T150). Tests only: a mock model with a small window
  // and a fake summary.
  modelOverride?: Model;
  // resolveModelOverride replaces the catalog lookup a switch makes (tests).
  resolveModelOverride?: (provider: string, model: string) => Model;
  maintenance?: Pick<MaintenanceHost, "compactImpl" | "summaryOptions" | "nativeEligible">;
  // store keeps the conversation across a sidecar restart (🎯T151). A seat
  // loaded with a store that already holds its session resumes from it.
  store?: SeatStore;
  requireResume?: boolean;
  // preserve and pins steer compaction (🎯T152): what the summary must
  // keep, and facts carried verbatim after it. setContext replaces them.
  preserve?: string;
  pins?: string[];
  // sandbox limits the seat's own tools (🎯T178). Absent: unchanged.
  sandbox?: SandboxPolicy;
  protected_paths?: ProtectedPathRule[];
}): SeatAgent {
  // Resume admission belongs to the authoritative seat/session store. Validate
  // without repair before constructing an engine or changing any file.
  if (opts.requireResume) {
    const entries = opts.store?.load({ repairTail: false });
    const history = new History(opts.store);
    history.entries = entries ?? [];
    // Accepted input is durable before it reaches the engine's history.
    // Reuse the receipt journal's validation and recovery classification.
    const pending = new ReceiptJournal(history).pending();
    if (history.rebuild(opts.provider).length === 0 && pending.length === 0) {
      const storePath = opts.store?.path ?? "(no store path)";
      throw new Error(
        `existing conversation required for this seat and session; store ${storePath} has no message records; refusing to mint a replacement session`,
      );
    }
  }
  let sandbox = opts.sandbox;
  let protectedPaths = opts.protected_paths ?? [];
  const policyOf = (): SandboxPolicy | undefined => {
    if (sandbox) return { ...sandbox, protected_paths: protectedPaths };
    if (protectedPaths.length) return { protected_paths: protectedPaths };
    return undefined;
  };
  let token = opts.token;
  let cwd = opts.cwd;
  let preserve = opts.preserve;
  let pins = opts.pins;
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
  let usage: UsageJournal | undefined;
  let rootRequest: string | undefined;
  // The latest request started on this turn. Text emitted before a steer
  // already left with the earlier id. A consumer that attaches later reads
  // the id from the event itself; the previous process's map is not required.
  const answerRequestID = () => {
    if (!turn) return undefined;
    let id = "";
    for (const next of currentRequests) id = next;
    return id || rootRequest || undefined;
  };
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
  // The bound and the tool list close over `agent` and run only after it exists.
  const ownership = new SeatProcessOwnership();
  const execHandlers = cursorExecHandlers(
    () => cwd,
    () => toolResultBound(agent.state.model?.contextWindow ?? 0),
    policyOf,
    ownership,
  );
  bindListedMcp(execHandlers, () => agent.state.tools);
  const streamPersist = { fn: (_message?: ToolResultMessage) => {} };
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
    streamFn: measuredStream(() => usage, usageAttribution),
    // Cursor's native tools come back as exec frames, not tool calls; without
    // handlers each answered "Tool not available" (2026-10-13). Listed tools
    // arrive on that same channel as MCP frames. bindListedMcp runs the seat
    // tool; without it the provider answers tool-not-found and the host never
    // sees tool_call (2026-10-18 isolate).
    cursorExecHandlers: execHandlers,
    cursorOnToolResult: async (message) => {
      streamPersist.fn(message);
      return message;
    },
    getApiKey: async () => { attemptToken = replayToken || token; replayToken = ""; requestCount++; return attemptToken; },
    // A compaction summary must reach the provider; the Agent's default
    // converter drops roles it does not know (🎯T150).
    convertToLlm: seatConvertToLlm,
    // An unlisted name is not a tool. The host declares every tool the
    // model may call. There is no consumer-prefix fallback.
    resolveFallbackTool: (_name: string) => undefined,
  });
  const boundOf = () => toolResultBound(agent.state.model?.contextWindow ?? 0);
  const callBack = (id: string, toolName: string, args: string) => sink.callTool(id, toolName, args);
  // The sidecar's own tools (Bash, Read, Write, Glob, Grep). A host tool
  // reaches the host as tool_call; these ran unseen (claudia 🎯T171).
  let ownToolNames = new Set<string>();
  let lastHost: HostTool[] | undefined;
  const applyTools = (host: HostTool[] | undefined) => {
    lastHost = host;
    // Every tool the host offers is model-visible (T871.1): the host
    // discovered it from AgentDef.MCPServers and already decided which
    // server executes it. An unlisted name, including a consumer-prefixed
    // one, is not invented here.
    const hosted = (host ?? []).map((t) => jevonsTool(t.name, callBack, t.description, t.input_schema, boundOf));
    const own = codingTools(() => cwd, boundOf, policyOf, ownership);
    ownToolNames = new Set(own.map((t) => t.name));
    agent.setTools([...own, ...hosted]);
  };
  applyTools(opts.tools);

  // Context maintenance (🎯T150): the Oh My Pi CLI's compaction triggers,
  // on pi-agent-core's engine.
  const maint = new Maintenance({
    agent,
    token: () => token,
    emit: (ev) => sink.emit(ev),
    store: opts.store,
    preserve: () => preserve,
    pins: () => pins,
    ...opts.maintenance,
    nativeUsageObserver: model => usage?.observer("compaction", model, usageAttribution()),
    summaryOptions: {
      ...opts.maintenance?.summaryOptions,
      completeImpl: measuredCompletion(() => usage, usageAttribution, opts.maintenance?.summaryOptions?.completeImpl),
    },
  });
  // Resume the stored conversation. An unreadable store throws, so the load
  // fails loudly instead of starting the seat on an empty conversation.
  const restored = maint.restore();
  const receipts = new ReceiptJournal(maint.history);
  if (opts.store) usage = new UsageJournal(maint.history,
    fact => sink.emit({ type: "usage", usage_fact: fact }),
    text => sink.emit({ type: "usage_error", text }));
  // continue() re-executes an unpaired tool batch before its model gate.
  // After a process crash we cannot know whether those effects happened, so
  // they must not run again unseen. Pair each unfinished call with an error
  // result saying exactly that (🎯T213): nothing re-executes, the model is
  // told the outcome is unknown and inspects before repeating, and the seat
  // keeps its conversation. Refusing every turn instead stalled 47 seats,
  // the overseer among them, for an hour after a broker restart (2026-10-22).
  // Cursor runs its tools inside the stream and marks those calls resolved
  // with a symbol, which does not survive the store. Restored, every Cursor
  // reply that used a tool looked unfinished: it was paired as interrupted
  // and the broker nudged the seat, so each restart started a turn on every
  // parked Cursor seat (2026-10-28). A Cursor reply that ended with stop has
  // already run its tools.
  if (restored) markRestoredCursorToolsResolved(agent.state.messages);
  const interrupted = restored ? unpairedToolCallTail(agent.state.messages) : undefined;
  const pairedInterruptedToolBatch = interrupted !== undefined;
  if (interrupted) {
    for (const call of interrupted.content) {
      if (call.type !== "toolCall") continue;
      agent.appendMessage({
        role: "toolResult",
        toolCallId: call.id,
        toolName: call.name,
        content: [{ type: "text", text: INTERRUPTED_TOOL_RESULT }],
        isError: true,
        timestamp: Date.now(),
      } as ToolResultMessage);
    }
    maint.persist();
    console.error(`sidecar: ${opts.store?.path}: paired unfinished tool calls with interrupted results (🎯T213)`);
  }
  const uncertainToolRestore = restored && unpairedToolCallTail(agent.state.messages) !== undefined;
  const transcriptEndsMidTurn = restored
    ? savedTranscriptEndsMidTurn(agent.state.messages, pairedInterruptedToolBatch)
    : false;
  let boundCompactUncertain = false;
  const assertRestorableExecution = () => {
    if (boundCompactUncertain) throw Object.assign(
      new Error("bound compaction outcome uncertain; re-check before prompting"),
      { reason: "compact_uncertain" },
    );
    if (uncertainToolRestore) throw Object.assign(
      new Error("restored history has unfinished tool calls; inspect their effects before starting a fresh session"),
      { reason: "execution_uncertain" },
    );
  };
  if (restored) {
    console.error(
      `sidecar: resumed ${opts.store?.path}: ${maint.history.entries.length} entries, ${agent.state.messages.length} messages in context`,
    );
  }
  // Between tool calls, when the loop is about to call the model again: the
  // CLI's mid-turn pass. The live array is the loop's own, so a compaction is
  // spliced into it for the next request.
  // Cursor exec-channel tool results land on agent.state before the loop's
  // context copy catches up, so persist canonical state only (🎯T206).
  let syntheticAssistant: AssistantMessage | undefined;
  const persistMidStream = (freshToolResult?: ToolResultMessage) => {
    const streaming = agent.state.streamMessage;
    if (streaming?.role === "assistant" && syntheticAssistant) {
      maint.history.forget(syntheticAssistant);
      syntheticAssistant = undefined;
    }
    let inFlight: AgentMessage | undefined;
    const pending: AgentMessage[] = [...agent.state.messages];
    if (streaming?.role === "assistant") {
      pending.push(streaming as AgentMessage);
      inFlight = streaming as AgentMessage;
    } else if (freshToolResult) {
      const current = agent.state.model;
      syntheticAssistant = {
        role: "assistant",
        content: [{
          type: "toolCall",
          id: freshToolResult.toolCallId,
          name: freshToolResult.toolName,
          arguments: {},
        }],
        api: current.api,
        provider: current.provider,
        model: current.id,
        usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
        stopReason: "toolUse",
        timestamp: freshToolResult.timestamp,
      };
      pending.push(syntheticAssistant);
      inFlight = syntheticAssistant;
    }
    if (freshToolResult) pending.push(freshToolResult);
    maint.persistMidStream(pending, inFlight);
  };
  streamPersist.fn = (message) => { persistMidStream(message); };
  const persistBeforeNextModel = (messages?: readonly AgentMessage[]) => {
    maint.history.clearInFlightAssistant();
    if (syntheticAssistant) {
      maint.history.forget(syntheticAssistant);
      syntheticAssistant = undefined;
    }
    maint.persist(messages ?? agent.state.messages);
  };
  agent.setOnTurnEnd(async (messages, signal, context) => {
    // Every finished model call is durable before the next one (🎯T151).
    persistBeforeNextModel();
    if (signal?.aborted || !context?.willContinue || maint.compacting) return;
    const last = [...messages].reverse().find((m) => m.role === "assistant") as { stopReason?: string } | undefined;
    if (!last || last.stopReason === "aborted" || last.stopReason === "error") return;
    if (!maint.due(messages)) return;
    await maint.run("threshold", "mid_turn", messages);
    if (signal?.aborted) return;
    const compacted = agent.state.messages;
    if (compacted !== messages) messages.splice(0, messages.length, ...compacted);
  });
  // overflowRefusal is the last message when the provider refused the turn
  // as longer than the window, or undefined.
  const overflowRefusal = () => {
    const messages = agent.state.messages;
    const last = messages[messages.length - 1];
    return isOverflow(last, maint.window()) ? (last as { errorMessage?: string }) : undefined;
  };

  // closeAbortedTail answers an input the abort stopped before the model
  // answered it, in the shape pi-agent-core's own pre-call gate uses. Left
  // as the transcript's tail, the next continue() (the abort's queue drain)
  // sent that cancelled input to the model again, ahead of the message the
  // host had sent next (jevons J3-cancel-and-send, 2026-10-19).
  const resetCodexHistoryAfterAbort = () => {
    const state = (agent as { providerSessionState?: Map<string, unknown> }).providerSessionState;
    if (!state) return;
    // Local pairing is the load-bearing fix. Seats often carry no
    // providerSessionState (🎯T150 residual); this is a no-op then.
    resetOpenAICodexHistoryAfterCompaction({
      providerSessionState: state as never,
      sessionId: (agent as { sessionId?: string }).sessionId,
      compaction: { operationId: crypto.randomUUID(), trigger: "auto", reason: "context_limit", phase: "pre_turn", strategy: "memento" } as never,
    });
  };

  const closeAbortedToolCalls = (live?: AgentMessage[]) => {
    const added: ToolResultMessage[] = [];
    pairUnpairedToolCalls(agent.state.messages, m => {
      agent.appendMessage(m);
      added.push(m);
    }, ABORTED_TOOL_RESULT);
    if (added.length === 0) return 0;
    if (live && live !== agent.state.messages) live.push(...added);
    maint.persist();
    return added.length;
  };

  // closeAbortedTail answers an input the abort stopped before the model
  // answered it. Only an unanswered user message: an assistant toolCall
  // tail is closed by closeAbortedToolCalls. Closing a toolResult with an
  // empty aborted assistant was a bad next-request prefix (🎯T236).
  const closeAbortedTail = () => {
    const last = agent.state.messages.at(-1);
    if (last?.role !== "user") return;
    const current = agent.state.model;
    agent.appendMessage({
      role: "assistant",
      content: [{ type: "text", text: "" }],
      api: current.api,
      provider: current.provider,
      model: current.id,
      usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
      stopReason: "aborted",
      errorMessage: "aborted by the host before the model answered",
      timestamp: Date.now(),
    } as AssistantMessage);
  };

  const finish = (stop: Stop) => {
    if (!turn || turn.closed) return;
    const tail = flushHold(turn);
    if (tail) sink.emit({ type: "text", text: tail, turn_id: turn.turn_id, request_id: answerRequestID() });
    const digest = closeTurn(turn, stop);
    sink.emit(digest);
  };

  // Messages queued or steered behind a running turn that the model has
  // not yet taken. pi-agent-core emits message_start for each one as its run
  // loop takes it; the host is told, so an escalation stops there (T138).
  const pending = new Set<Parameters<typeof agent.steer>[0]>();
  let currentRequests = new Set<string>();
  const cancelledTurns = new Set<string>();
  let turnDone: Promise<void> | undefined;
  const heldSteering: Parameters<typeof agent.steer>[0][] = [];
  const heldFollowUps: Parameters<typeof agent.followUp>[0][] = [];
  // The turn identity the host gave a prompt that had to wait behind a
  // running turn. The turn that later takes it from the queue runs under
  // that identity, so the host hears the reply as the turn it opened; a
  // minted id was a turn the host never sent (jevons J3, 2026-10-19).
  const queuedTurnIDs = new WeakMap<object, string>();
  const queuedHeadTurnID = () => {
    const head = agent.peekSteeringQueue()[0] ?? agent.peekFollowUpQueue()[0];
    return head ? queuedTurnIDs.get(head) : undefined;
  };

  // Accepted inputs that never reached dispatch remain obligations after a
  // restart. Rebuild only that queue; a started/unknown request is never
  // automatically submitted again. Loading or querying does not run it.
  const recovered = receipts.pending();
  const recoveredIDs = new Set(recovered.map(item => item.receipt.request_id));
  if (recoveredIDs.size > 0) {
    agent.replaceMessages(agent.state.messages.filter(message => {
      if (!recoveredIDs.has((message as ReceiptMessage).claudiaRequest?.request_id)) return true;
      // Remove the undelivered input from canonical context too: a switch
      // or compaction rebuild must not reinsert it beside its queue copy.
      maint.history.forget(message);
      return false;
    }));
    maint.persist();
    for (const item of recovered) {
      pending.add(item.message);
      if (item.delivery === "steer") agent.steer(item.message);
      else agent.followUp(item.message);
    }
  }
  const settleRequests = (ids: Set<string>, state: ReceiptTerminalState, reason?: string, cause?: ReceiptCause) => {
    if (ids.size === 0) return;
    const started = [...ids].filter(id => receipts.query(id).state === "started");
    receipts.finish([...ids], state, reason, cause);
    for (const id of started) sink.emit({ type: "receipt", request_id: id, receipt: receipts.query(id) });
  };
  const cancelOpenReceipts = () => {
    const ids = receipts.startedIDs();
    if (ids.length === 0) return;
    receipts.finish(ids, "cancelled", "cancelled", "cancellation");
    for (const id of ids) sink.emit({ type: "receipt", request_id: id, receipt: receipts.query(id) });
  };
  agent.addBeforeModelCall((context) => {
    receipts.assertHealthy();
    closeAbortedToolCalls(context.messages as AgentMessage[]);
    // Listeners are observational and swallow exceptions in pi-agent-core.
    // This awaited gate is the fail-closed persistence boundary (🎯T206).
    persistBeforeNextModel(context.messages);
    // Input events are emitted after this gate on the first call. Inspect
    // the actual prepared input, not event timing or the pending FIFO.
    for (const message of context.messages) {
      const id = (message as ReceiptMessage).claudiaRequest?.request_id;
      if (!id) continue;
      const state = receipts.query(id).state;
      if (state === "accepted" || state === "started") currentRequests.add(id);
    }
    if (currentRequests.size === 0) return;
    const newlyStarted = [...currentRequests].filter(id => !receipts.query(id).turn_ids.includes(turn!.turn_id));
    receipts.start([...currentRequests], turn!.turn_id);
    for (const id of newlyStarted) sink.emit({ type: "receipt", request_id: id, turn_id: turn!.turn_id, receipt: receipts.query(id) });
  });

  agent.subscribe((event: {
    type?: string;
    assistantMessageEvent?: { type?: string; delta?: string };
    message?: { role?: string; content?: unknown; claudiaRequest?: { request_id: string } };
    toolCallId?: string;
    toolName?: string;
    args?: unknown;
  }) => {
    // 🎯T171: tell the host a sidecar tool started, so a seat running Bash
    // is not silent. Host tools are already announced by their tool_call.
    if (event.type === "tool_execution_start" && event.toolName && ownToolNames.has(event.toolName)) {
      sink.emit({ type: "tool_start", call_id: event.toolCallId ?? "", name: event.toolName, text: JSON.stringify(event.args ?? {}) });
    }
    if (event.type === "message_start" && event.message?.role === "user") {
      const text = userText(event.message.content);
      const id = event.message.claudiaRequest?.request_id;
      if (id) currentRequests.add(id);
      if (pending.delete(event.message as Parameters<typeof agent.steer>[0])) {
        sink.emit({ type: "absorbed", text, request_id: id, turn_id: turn?.turn_id });
      }
      return;
    }
    if (event.type === "message_end" && event.message?.role) {
      const role = event.message.role;
      if (role === "assistant" && Array.isArray(event.message.content)) {
        if (event.message.content.some((part: any) => part?.type === "toolCall" || (part?.type === "text" && part.text))) unsafe = true;
      }
      // Each accepted message unit is durable before the stream continues
      // (🎯T206): Cursor runs many exec frames inside one model request.
      if (role === "assistant" || role === "toolResult") persistBeforeNextModel();
    }
    // Tool declarations (including provider-executed tools) and any text
    // make replay unsafe, even if no visible digest delta was emitted.
    if (event.assistantMessageEvent?.type === "toolcall_start" ||
        event.assistantMessageEvent?.type === "text_delta" || event.type === "tool_execution_start") unsafe = true;
    if (event.type === "tool_execution_end") {
      persistMidStream();
    }
    if (event.type === "tool_execution_start" && turn && !turn.closed) {
      noteTool(turn);
      return;
    }
    if (event.assistantMessageEvent?.type === "toolcall_start" || event.assistantMessageEvent?.type === "toolcall_end") {
      persistMidStream();
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

  // runTurn brackets one turn: digest, acceptance, the run itself, any
  // follow-ups left after it, and turn_end.
  const executeTurn = async (text: string, meta: PromptMeta | undefined, run: () => Promise<void>, accepted?: RequestReceipt) => {
    assertRestorableExecution();
    const metaSession = meta?.session_id || sessionId;
    turn = beginTurn({
      seat: "",
      text,
      meta: { ...meta, session_id: metaSession || undefined },
    });
    sessionId = turn.session_id;
    const executionTurn = turn;
    const executionRequests = new Set<string>();
    currentRequests = executionRequests;
    rootRequest = meta?.request_id;
    attemptToken = "";
    replayToken = "";
    requestCount = 0;
    cancelled = false;
    unsafe = false;
    // A turn can think for a minute before its first token. Announce it
    // now so the host sees the prompt land (Jevons T887).
    if (accepted) sink.emit({ type: "accepted", request_id: accepted.request_id, rpc_id: meta?.rpc_id, receipt: accepted, turn_id: turn.turn_id });
    else if (!meta?.request_id) sink.emit({ type: "accepted", turn_id: turn.turn_id });
    let refusal = "";
    let overflow = false;
    let runInvoked = false;
    try {
      // Before the prompt, counting the prompt itself: the CLI's pre-prompt
      // pass, so the provider never sees a request past its window.
      if (maint.due(agent.state.messages, agent.tokenizer.countTokens(text))) {
        await maint.run("threshold", "pre_turn");
      }
      const before = new Set(agent.state.messages);
      runInvoked = true;
      await run();
      if (cancelled) {
        closeAbortedToolCalls();
        closeAbortedTail();
        // Pairing is the load-bearing local fix. Also drop a Codex
        // previous_response_id that still names the cancelled function call.
        resetCodexHistoryAfterAbort();
      }
      const failed = agent.state.messages.at(-1);
      const authError = turnRefusal(agent.state);
      if (failed && !before.has(failed) && authRefusal(authError) && requestCount === 1 &&
          attemptToken && !unsafe && !cancelled && !turn.closed && turn.tool_calls === 0 && turn.chars === 0) {
        // Keep the retry's conversation identical to the failed attempt.
        // continue() otherwise consumes queued steering before making a request.
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
            maint.history.forget(failed as never);
            await agent.continue();
          }
        } finally {
          agent.replaceQueues([...steering, ...agent.peekSteeringQueue(), ...heldSteering.splice(0)], [...followUps, ...agent.peekFollowUpQueue(), ...heldFollowUps.splice(0)]);
          retrying = false;
          releaseRetry();
        }
      }
      // A hollow length stop is the provider handing back an incomplete
      // response with no output and no usage (openai-codex, ~5% of calls on
      // 2026-10-27). One is usually transient: ask again. Two in a row at the
      // same size is the backend's way of saying the prompt no longer fits
      // (jv-auditor-po: four in a row at 224K input), so record that size as
      // the window and fold the conversation before asking again. Ending the
      // turn on it strands the seat mid-task until something nudges it.
      for (let i = 0; i < hollowLengthRetries && !cancelled && !turn.closed && hollowLengthStop(agent.state.messages.at(-1)); i++) {
        const hollow = agent.state.messages.at(-1);
        agent.popMessage();
        maint.history.forget(hollow as never);
        if (i > 0) {
          maint.noteObservedLimit(maint.storedTokens());
          if (!(await maint.run("overflow", "mid_turn"))) {
            agent.appendMessage(hollow as never);
            break;
          }
        } else {
          await new Promise(resolve => setTimeout(resolve, hollowLengthBackoffMs));
        }
        if (cancelled || turn.closed) {
          agent.appendMessage(hollow as never);
          break;
        }
        await agent.continue();
      }
      // A follow-up queued after the run's own last check would wait for
      // the next prompt. Drain it inside this turn (bounded).
      for (let i = 0; i < 8 && !cancelled && !turnRefusal(agent.state) && agent.hasQueuedMessages() && !agent.state.isStreaming; i++) {
        await agent.continue();
      }
      // Refused as too long anyway (the window was overstated, or one turn
      // outgrew the reserve): drop the refusal, fold the conversation, and
      // send it once more. Once, not in a loop: a second refusal means
      // compaction cannot help, and the turn ends as terminal.
      const refused = overflowRefusal();
      if (refused) {
        maint.noteProviderLimit(refused.errorMessage);
        agent.popMessage();
        maint.history.forget(refused as never);
        if (await maint.run("overflow", "mid_turn")) {
          await agent.continue();
        } else {
          agent.appendMessage(refused as never);
        }
        overflow = overflowRefusal() !== undefined;
      }
      // After the turn: the CLI's post-turn pass, so the next prompt starts
      // under the threshold. The turn has answered; nothing is continued.
      if (!overflow && !turnRefusal(agent.state) && maint.due(agent.state.messages)) {
        await maint.run("threshold", "post_turn");
        // A message that arrived while it compacted was queued behind this
        // turn: run it now, as the CLI drains its queue after a compaction.
        for (let i = 0; i < 8 && !cancelled && !turnRefusal(agent.state) && agent.hasQueuedMessages() && !agent.state.isStreaming; i++) {
          await agent.continue();
        }
      }
      refusal = turnRefusal(agent.state) || emptyAnswerAfterUnpairedTool(agent.state.messages);
      finish(refusal ? "error" : turn?.stop_token ? "stop_token" : "end_turn");
    } catch (err) {
      // Preparation (notably compaction) can fail before Agent.prompt gets
      // the newly accepted input. Keep it in the existing queue so the same
      // ID can resume in this process, not only after a sidecar restart.
      if (!runInvoked && accepted) {
        const waiting = receipts.pending().find(item => item.receipt.request_id === accepted.request_id);
        if (waiting) {
          agent.followUp(waiting.message);
          pending.add(waiting.message);
        }
      }
      settleRequests(executionRequests, cancelledTurns.has(executionTurn.turn_id) ? "cancelled" : "failed",
        cancelledTurns.has(executionTurn.turn_id) ? "cancelled" : "execution_error",
        cancelledTurns.has(executionTurn.turn_id) ? "cancellation" : "failure");
      cancelledTurns.delete(executionTurn.turn_id);
      finish("error");
      throw err;
    } finally {
      // The turn's messages are durable before the host hears it ended.
      receipts.assertHealthy();
      maint.persist();
    }
    const errorID = refusal ? classifyMessage(agent.state.messages.at(-1) as AssistantMessage) : undefined;
    const cancelledTurn = cancelledTurns.has(executionTurn.turn_id);
    const authFailed = authRefusal(refusal) || hasFlag(errorID, Flag.AuthFailed) || hasFlag(errorID, Flag.OAuthExpiry);
    const capacityFailed = hasFlag(errorID, Flag.UsageLimit);
    const failureReason = !refusal ? undefined : overflow ? ContextOverflow
      : authFailed ? "auth_unavailable"
      : capacityFailed ? "capacity_unavailable"
      : hasFlag(errorID, Flag.Timeout) ? "provider_timeout"
      : hasFlag(errorID, Flag.Transient) ? "provider_unavailable" : "provider_refusal";
    const failureCause: ReceiptCause | undefined = cancelledTurn ? "cancellation"
      : !refusal ? undefined
      : authFailed ? "auth"
      : capacityFailed ? "capacity"
      : "failure";
    settleRequests(executionRequests, cancelledTurn ? "cancelled" : refusal ? "failed" : "completed",
      cancelledTurn ? "cancelled" : failureReason, failureCause);
    cancelledTurns.delete(executionTurn.turn_id);
    if (cancelled) return;
    if (refusal) {
      sink.emit({ type: "turn_end", turn_id: turn.turn_id, request_id: answerRequestID(), error: refusal, reason: overflow ? ContextOverflow : undefined, snapshot: agent.state });
      return;
    }
    sink.emit({ type: "turn_end", turn_id: turn.turn_id, request_id: answerRequestID(), snapshot: agent.state });
  };

  // The engine can be idle while preparation or receipt cleanup is still
  // running. Serialize that whole interval, including after abort closed the
  // digest, so an earlier turn cannot overwrite the next turn's identity.
  //
  // An error after the turn began is that turn's (TurnError). An aborted
  // turn has already ended as itself, so what its unwinding throws is not a
  // second end: it is logged, not answered.
  const refuseCompactMutation = () => {
    if (turnDone || retrying || agent.state.isStreaming || (turn && !turn.closed) || maint.compacting || agent.hasQueuedMessages()) {
      throw Object.assign(new Error("compact refused: turn in flight"), { reason: "turn_in_flight" });
    }
  };

  // Bound compact between turns (🎯T244): refuse new prompt/steer that would
  // start while history is mutating. Mid-turn maintenance compaction still
  // queues prompt/steer onto the open turn (maintain.test.ts).
  const refuseDuringBoundCompact = () => {
    if (!maint.compacting) return;
    if (turnDone || retrying || agent.state.isStreaming || (turn && !turn.closed) || agent.hasQueuedMessages()) return;
    throw Object.assign(new Error("seat refused: bound compaction running"), { reason: "turn_in_flight" });
  };

  const runTurn = async (text: string, meta: PromptMeta | undefined, run: () => Promise<void>, accepted?: RequestReceipt) => {
    if (turnDone) throw new Error("a turn is still unwinding");
    let releaseTurn!: () => void;
    turnDone = new Promise<void>(resolve => { releaseTurn = resolve; });
    const previous = turn;
    try {
      await executeTurn(text, meta, run, accepted);
    } catch (err) {
      const begun = turn !== previous && turn ? turn.turn_id : undefined;
      if (!begun) throw err;
      if (cancelled) {
        console.error(`sidecar: aborted turn ${begun} unwound with an error: ${err instanceof Error ? err.message : String(err)}`);
        return;
      }
      const tagged = (err instanceof Error ? err : new Error(String(err))) as TurnError;
      tagged.turn_id = begun;
      throw tagged;
    } finally {
      turnDone = undefined;
      releaseTurn();
    }
  };

  const abortTurn = () => {

      cancelOpenReceipts();
      const interruptedTurn = turnDone ?? Promise.resolve();
      cancelled = true;
      const ended: EndedTurn | undefined = turn && !turn.closed ? { turn_id: turn.turn_id, request_id: answerRequestID() } : undefined;
      if (ended) cancelledTurns.add(ended.turn_id);
      recovery.cancel();
      maint.abort();
      agent.abort();
      finish("abort");
      // An interrupt is how a waiting message gets taken now (T138). The
      // queues survive abort; once the aborted run has unwound, run what is
      // queued as its own turn instead of leaving it for a later prompt.
      void agent.waitForIdle().then(async () => {
        await interruptedTurn;
        await retryDone;
        if (!agent.hasQueuedMessages() || agent.state.isStreaming || turnDone) return;
        try {
          // continue() from a toolResult tail re-asks the model about the
          // cancelled tool, then absorbs the next prompt in the same turn
          // (🎯T236). Take the queued user as a fresh prompt instead.
          const last = agent.state.messages.at(-1);
          if (last?.role === "toolResult") {
            const steering = [...agent.peekSteeringQueue()];
            const followUps = [...agent.peekFollowUpQueue()];
            const next = steering[0] ?? followUps[0];
            if (!next) return;
            agent.replaceQueues(steering.length ? steering.slice(1) : steering, steering.length ? followUps : followUps.slice(1));
            await runTurn("", { cause: "interrupt-deliver", turn_id: queuedTurnIDs.get(next) ?? queuedHeadTurnID() }, () => agent.prompt(next));
            return;
          }
          await runTurn("", { cause: "interrupt-deliver", turn_id: queuedHeadTurnID() }, () => agent.continue());
        } catch (err) {
          // A turn that began fails as itself. One that never began belongs
          // to no turn: it is the seat's, not an answer (seat_error).
          const turnID = turnOf(err);
          sink.emit({ type: turnID ? "error" : "seat_error", turn_id: turnID, text: err instanceof Error ? err.message : String(err) });
        }
      });
      return ended;
  };

  return {
    receipt: (requestId) => receipts.query(requestId),
    usage: (after, limit) => {
      if (!usage) throw new Error("usage accounting unavailable: no durable session store");
      return usage.page(after, limit);
    },
    authRetry: (reply) => recovery.receive(reply),
    prompt: async (text: string, meta?: PromptMeta) => {
      receipts.assertHealthy();
      assertRestorableExecution();
      const accepted = meta?.request_id ? receipts.accept(text, meta as typeof meta & { request_id: string; session_id: string }, "prompt") : undefined;
      if (accepted && !accepted.fresh) {
        sink.emit({ type: "accepted", request_id: meta!.request_id, rpc_id: meta?.rpc_id, receipt: accepted.receipt });
        if (accepted.receipt.state === "accepted" && !turnDone && !retrying && !agent.state.isStreaming && agent.hasQueuedMessages()) {
          await runTurn("", { ...meta, turn_id: queuedHeadTurnID() ?? meta?.turn_id }, () => agent.continue());
        }
        return;
      }
      const message = accepted?.message ?? { role: "user" as const, content: text, timestamp: Date.now() };
      // A prompt that arrives mid-turn is queued, not refused. The running
      // turn answers it before it ends. Refusing lost the message: the
      // refusal reached the owner as the seat's own reply and nothing retried
      // (2026-09-28, owner messages to a busy product owner). Checking the
      // agent too covers the gap after this turn closes while pi-agent-core
      // is still finishing, where prompt() throws AgentBusyError.
      if (turnDone || retrying || agent.state.isStreaming || agent.hasQueuedMessages()) {
        const enqueue = retrying ? (m: Parameters<typeof agent.followUp>[0]) => heldFollowUps.push(m) : (m: Parameters<typeof agent.followUp>[0]) => agent.followUp(m);
        if (meta?.turn_id) queuedTurnIDs.set(message, meta.turn_id);
        enqueue(message);
        pending.add(message);
        // Say at once that it was accepted: a host that waits for a first
        // streamed token would call a queued prompt undelivered.
        sink.emit({ type: "accepted", request_id: meta?.request_id, rpc_id: meta?.rpc_id, receipt: accepted?.receipt });
        // A terminal refusal leaves accepted inputs queued. A later prompt
        // joins behind them rather than jumping ahead via agent.prompt().
        // The turn opens on the oldest of them, under its identity.
        if (!turnDone && !retrying && !agent.state.isStreaming) {
          refuseDuringBoundCompact();
          await runTurn("", { ...meta, turn_id: queuedHeadTurnID() ?? meta?.turn_id }, () => agent.continue());
        }
        return;
      }
      refuseDuringBoundCompact();
      await runTurn(text, meta, () => agent.prompt(message), accepted?.receipt);
    },
    steer: (text: string, meta?: PromptMeta) => {
      receipts.assertHealthy();
      assertRestorableExecution();
      refuseDuringBoundCompact();
      const accepted = meta?.request_id ? receipts.accept(text, meta as typeof meta & { request_id: string; session_id: string }, "steer") : undefined;
      if (accepted && !accepted.fresh) {
        sink.emit({ type: "accepted", request_id: meta!.request_id, rpc_id: meta?.rpc_id, receipt: accepted.receipt });
        return;
      }
      const enqueue = retrying ? (m: Parameters<typeof agent.steer>[0]) => heldSteering.push(m) : (m: Parameters<typeof agent.steer>[0]) => agent.steer(m);
      const message = accepted?.message ?? { role: "user" as const, content: text, timestamp: Date.now() };
      enqueue(message);
      pending.add(message);
      sink.emit({ type: "accepted", request_id: meta?.request_id, rpc_id: meta?.rpc_id, receipt: accepted?.receipt });
    },
    cancelOpenReceipts,
    abort: abortTurn,
    stopTurn: () => {
      const ended = abortTurn();
      // A released host owns no queued work. The ordinary interrupt path
      // delivers queues; a stop must not start another old-provider request.
      agent.replaceQueues([], []);
      heldSteering.length = 0;
      heldFollowUps.length = 0;
      return ended;
    },
    switchModel: async (provider: string, modelId: string) => {
      assertRestorableExecution();
      if (turnDone || agent.state.isStreaming) {
        throw Object.assign(new Error("switch refused: a turn is running"), { reason: "turn_in_flight" });
      }
      if (maint.compacting) {
        throw Object.assign(new Error("switch refused: compaction running"), { reason: "turn_in_flight" });
      }
      await maint.switchModel((opts.resolveModelOverride ?? resolveModel)(provider, modelId));
    },
    compactAtBound: async (boundTokens: number, opts?: { cancel?: AbortSignal }) => {
      assertRestorableExecution();
      refuseCompactMutation();
      try {
        const out = await maint.compactToBound(boundTokens, opts);
        boundCompactUncertain = false;
        return out;
      } catch (err) {
        if (err instanceof Error && "reason" in err && err.reason === "compact_uncertain") boundCompactUncertain = true;
        throw err;
      }
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
    setProtectedPaths: (next) => {
      protectedPaths = next ?? [];
      applyTools(lastHost);
    },
    snapshot: () => agent.state,
    boundToolNames: () => (agent.state.tools ?? []).map((tool) => tool.name),
    openTurnID: () => (turn && !turn.closed ? turn.turn_id : undefined),
    rebind: (emit, callTool) => {
      sink.emit = emit;
      sink.callTool = callTool;
    },
    setHostTools: (tools) => applyTools(tools),
    restored,
    transcriptEndsMidTurn,
    setContext: (nextPreserve, nextPins) => {
      preserve = nextPreserve;
      pins = nextPins;
    },
  };
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
// INTERRUPTED_TOOL_RESULT answers a tool call whose run was cut off by a
// broker or sidecar restart. Its effects may or may not have happened.
export const INTERRUPTED_TOOL_RESULT =
  "This tool call was interrupted by a broker or sidecar restart before its result was recorded. " +
  "Its effects are unknown: it may or may not have run. Inspect the current state before repeating it.";

// ABORTED_TOOL_RESULT closes a tool call the host cancelled mid-flight (🎯T236).
export const ABORTED_TOOL_RESULT =
  "This tool call was aborted by the host before it finished. Its effects may or may not have happened. Inspect the current state before repeating it.";

export function pairUnpairedToolCalls(
  messages: readonly AgentMessage[],
  append: (m: ToolResultMessage) => void,
  text: string,
): number {
  const interrupted = unpairedToolCallTail(messages as AgentMessage[]);
  if (!interrupted) return 0;
  let n = 0;
  for (const call of interrupted.content) {
    if (call.type !== "toolCall") continue;
    append({
      role: "toolResult",
      toolCallId: call.id,
      toolName: call.name,
      content: [{ type: "text", text }],
      isError: true,
      timestamp: Date.now(),
    } as ToolResultMessage);
    n++;
  }
  return n;
}

// emptyAnswerAfterUnpairedTool is the live J3 shape: openai-codex returned
// empty stop_reason end_turn / stop after an unanswered function call, and
// turnRefusal missed it because stopReason was not "error".
export function emptyAnswerAfterUnpairedTool(messages: readonly unknown[]): string {
  if (!messages.length) return "";
  const last = messages[messages.length - 1] as {
    role?: string; stopReason?: string; errorMessage?: string; content?: unknown;
  };
  if (last?.role !== "assistant" || last.stopReason === "error" || last.stopReason === "aborted") return "";
  const parts = Array.isArray(last.content) ? last.content as Array<{ type?: string; text?: string }> : [];
  const empty = parts.every(p => p?.type !== "toolCall" && !(p?.text ?? "").trim());
  if (!empty) return "";
  if (!unpairedToolCallTail(messages.slice(0, -1) as AgentMessage[])) return "";
  return (last.errorMessage ?? "").trim() || "provider rejected an unanswered function call";
}

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
