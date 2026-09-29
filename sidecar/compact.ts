// Context compaction for a long-lived sidecar seat (🎯T148, jevons 🎯T926).
//
// A seat's pi-agent-core conversation only grows. On 2026-09-29 the
// development overseer reached 1.05M tokens against a 1M window, and every
// turn after that was refused with `400 prompt is too long`. This module
// decides when a conversation must shrink and shrinks it: the older part
// becomes one summary message, the recent tail is kept verbatim.

import {
  estimateTranscriptTokens,
  findTranscriptUsageAnchor,
  generateSummary,
  renderCompactionSummaryContext,
  resolveThresholdTokens,
  DEFAULT_COMPACTION_SETTINGS,
  type AgentMessage,
  type Tokenizer,
} from "@oh-my-pi/pi-agent-core";
import type { Model } from "@oh-my-pi/pi-ai";
import { isContextOverflow } from "@oh-my-pi/pi-ai/error";

// ContextOverflow is the reason a turn_end carries when the provider refused
// the turn as too long and compaction could not bring it back under the
// window. The host treats it as terminal: sending the same seat another
// prompt cannot succeed, so it must not be retried as if it were transient.
export const ContextOverflow = "context_overflow";

// keepRecentTokens is the verbatim tail a compaction keeps (pi-agent-core's
// own default), capped at a quarter of the threshold for small windows.
const keepRecentTokens = DEFAULT_COMPACTION_SETTINGS.keepRecentTokens;

// thresholdTokens is the size past which a seat compacts before its next
// request: the window less pi-agent-core's reserve for the next prompt and
// response (15% of the window, at least 16k).
export function thresholdTokens(contextWindow: number): number {
  return resolveThresholdTokens(contextWindow, DEFAULT_COMPACTION_SETTINGS);
}

// contextTokens is the seat's conversation size as the next request would
// send it: the provider's last usage report for what it has already seen,
// plus a local count of everything after it. anchorFrom excludes usage
// reports from before the last compaction, which describe a prompt that is
// no longer sent. Without a usage anchor the system prompt and tool schemas
// are counted too.
export function contextTokens(
  messages: readonly AgentMessage[],
  tokenizer: Tokenizer,
  anchorFrom: number,
  prefix: string[],
): number {
  const total = estimateTranscriptTokens(messages, tokenizer, { anchorFromIndex: anchorFrom });
  if (findTranscriptUsageAnchor(messages, anchorFrom)) return total;
  return total + tokenizer.countTokens(prefix);
}

// isOverflow reports a turn the provider refused because the prompt was
// larger than the model's context window. Only a refusal counts: a turn that
// answered is not an overflow, whatever its usage says.
export function isOverflow(message: AgentMessage | undefined, contextWindow: number): boolean {
  if (!message || message.role !== "assistant") return false;
  if ((message as { stopReason?: string }).stopReason !== "error") return false;
  return isContextOverflow(message as never, contextWindow);
}

// providerLimit is the window a provider named in its refusal ("prompt is too
// long: 1058579 tokens > 1000000 maximum"), or 0. The catalog's window can
// overstate what a credential is allowed; the refusal is the provider's word.
export function providerLimit(errorMessage: string | undefined): number {
  const m = /(\d+)\s*tokens?\s*>\s*(\d+)\s*maximum/i.exec(errorMessage ?? "");
  return m ? Number(m[2]) : 0;
}

// cutIndex is where the verbatim tail begins: the earliest user or assistant
// message such that everything from it on fits in keep tokens. A tool result
// is never a cut point — it must follow its tool call. At least the last cut
// point is kept, so the newest request is never summarized away. Returns 0
// when there is nothing before the tail to fold.
export function cutIndex(messages: readonly AgentMessage[], tokenizer: Tokenizer, keep: number): number {
  let cut = -1;
  let tail = 0;
  for (let i = messages.length - 1; i >= 0; i--) {
    tail += tokenizer.countMessage(messages[i]);
    const role = messages[i].role;
    if (role !== "user" && role !== "assistant") continue;
    if (cut >= 0 && tail > keep) break;
    cut = i;
  }
  return cut < 0 ? 0 : cut;
}

export type Summarize = (head: AgentMessage[], reserveTokens: number) => Promise<string>;

// summarizeWith is the production summarizer: pi-agent-core's own, run on the
// seat's model and token. It windows a head larger than the model can read.
export function summarizeWith(model: () => Model, token: () => string): Summarize {
  return (head, reserveTokens) =>
    generateSummary(head, model(), reserveTokens, token(), undefined, undefined, undefined, { oneshotRetry: false });
}

export type Compaction = {
  messages: AgentMessage[];
  before: number;
  after: number;
  folded: number;
  // how is "summary", or "truncated" when the summarizer failed and the
  // head was dropped with a note rather than leaving the seat wedged.
  how: "summary" | "truncated";
};

// compact folds everything before the verbatim tail into one summary
// message. It returns null when there is nothing before the tail to fold.
export async function compact(args: {
  messages: AgentMessage[];
  tokenizer: Tokenizer;
  contextWindow: number;
  anchorFrom: number;
  prefix: string[];
  summarize: Summarize;
}): Promise<Compaction | null> {
  const { messages, tokenizer, contextWindow } = args;
  const threshold = thresholdTokens(contextWindow);
  const keep = Math.max(1, Math.min(keepRecentTokens, Math.floor(threshold / 4)));
  const cut = cutIndex(messages, tokenizer, keep);
  if (cut <= 0) return null;
  const before = contextTokens(messages, tokenizer, args.anchorFrom, args.prefix);
  const head = messages.slice(0, cut);
  const tail = messages.slice(cut);
  let summary = "";
  let how: Compaction["how"] = "summary";
  try {
    summary = (await args.summarize(head, contextWindow - threshold)).trim();
  } catch (err) {
    console.error("sidecar: compaction summary failed; dropping the head with a note", err);
  }
  if (!summary) {
    how = "truncated";
    summary =
      `${head.length} earlier messages of this conversation were removed to stay within the model's context window. ` +
      "A summary of them could not be produced. Ask the host for anything you need from before this point.";
  }
  const note: AgentMessage = {
    role: "user",
    content: [{ type: "text", text: renderCompactionSummaryContext(summary) }],
    timestamp: Date.now(),
  } as AgentMessage;
  const next = [note, ...tail];
  return {
    messages: next,
    before,
    after: contextTokens(next, tokenizer, next.length, args.prefix),
    folded: head.length,
    how,
  };
}
