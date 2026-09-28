// One digest per prompt a sidecar seat accepts (🎯T870).
// The searchable line is this digest. The agent snapshot stays on
// turn_end, for resume, and is not this record.

export const CAUSES = [
  "owner",
  "restart-nudge",
  "sentinel",
  "impatience",
  "agent-forward",
  "rsi",
  "capacity",
  "steer",
] as const;

export type Cause = (typeof CAUSES)[number];

export const STOPS = ["end_turn", "abort", "error", "stop_token"] as const;
export type Stop = (typeof STOPS)[number];

export const RESUMES = ["adopted", "launched", "reminted"] as const;

// Longer tokens first so <|eot_id|> is not eaten as <|eot|>.
export const STOP_TOKENS = ["<|endoftext|>", "<|im_end|>", "<|eot_id|>", "<|eos|>", "<|eot|>"];

const causeSet = new Set<string>(CAUSES);
const resumeSet = new Set<string>(RESUMES);

export type PromptMeta = {
  turn_id?: string;
  session_id?: string;
  cause?: string;
  cause_detail?: string;
  resume?: string;
};

export type OpenTurn = {
  turn_id: string;
  seat: string;
  session_id: string;
  cause: Cause;
  cause_detail: string;
  resume?: string;
  started_at: string;
  tool_calls: number;
  deltas: number;
  chars: number;
  stop_token?: string;
  hold: string;
  closed: boolean;
};

export type TurnDigest = {
  type: "turn";
  ts: string;
  turn_id: string;
  session_id: string;
  cause: Cause;
  cause_detail: string;
  started_at: string;
  ended_at: string;
  stop: Stop;
  tool_calls: number;
  deltas: number;
  chars: number;
  stop_token?: string;
  resume?: string;
};

const detailMax = 160;

export function oneLine(text: string): string {
  let s = text.trim();
  const nl = s.search(/[\r\n]/);
  if (nl >= 0) s = s.slice(0, nl).trim();
  if (s.length > detailMax) s = s.slice(0, detailMax);
  return s;
}

export function classifyCause(text: string): { cause: Cause; detail: string } {
  const raw = text ?? "";
  const detail = oneLine(raw);
  if (raw.includes("The host restarted at") || raw.includes("[claudia] The host restarted")) {
    return { cause: "restart-nudge", detail };
  }
  if (/\[event:\s*sentinel\]/.test(raw)) return { cause: "sentinel", detail };
  if (/Impatience incident/i.test(raw)) return { cause: "impatience", detail };
  if (/\[Agent [^\]]+ responded\]/.test(raw)) return { cause: "agent-forward", detail };
  if (/\[event:\s*rsi(?:-coach)?\]/.test(raw)) return { cause: "rsi", detail };
  if (/\[event:\s*capacity\]/.test(raw)) return { cause: "capacity", detail };
  return { cause: "owner", detail };
}

export function beginTurn(args: {
  seat: string;
  text: string;
  meta?: PromptMeta;
  now?: Date;
}): OpenTurn {
  const meta = args.meta ?? {};
  const classified = classifyCause(args.text);
  const cause: Cause = meta.cause && causeSet.has(meta.cause) ? (meta.cause as Cause) : classified.cause;
  const detail = oneLine(meta.cause_detail || classified.detail);
  const resume = meta.resume && resumeSet.has(meta.resume) ? meta.resume : undefined;
  const now = (args.now ?? new Date()).toISOString();
  return {
    turn_id: meta.turn_id || crypto.randomUUID(),
    seat: args.seat,
    session_id: meta.session_id || crypto.randomUUID(),
    cause,
    cause_detail: detail,
    resume,
    started_at: now,
    tool_calls: 0,
    deltas: 0,
    chars: 0,
    hold: "",
    closed: false,
  };
}

function holdSuffix(buf: string): string {
  let best = "";
  for (const token of STOP_TOKENS) {
    const max = Math.min(token.length - 1, buf.length);
    for (let n = max; n > best.length; n--) {
      const suf = token.slice(0, n);
      if (buf.endsWith(suf)) {
        best = suf;
        break;
      }
    }
  }
  return best;
}

function stripComplete(buf: string, turn: OpenTurn): string {
  let changed = true;
  while (changed) {
    changed = false;
    for (const token of STOP_TOKENS) {
      const at = buf.indexOf(token);
      if (at >= 0) {
        if (!turn.stop_token) turn.stop_token = token;
        buf = buf.slice(0, at) + buf.slice(at + token.length);
        changed = true;
        break;
      }
    }
  }
  return buf;
}

// noteDelta counts owner-visible text. A stop token is recorded on the
// turn and is not part of the returned text.
export function noteDelta(turn: OpenTurn, delta: string): string {
  let buf = stripComplete(turn.hold + (delta ?? ""), turn);
  const hold = holdSuffix(buf);
  turn.hold = hold;
  const visible = hold ? buf.slice(0, buf.length - hold.length) : buf;
  if (visible) {
    turn.deltas++;
    turn.chars += visible.length;
  }
  return visible;
}

export function noteTool(turn: OpenTurn): void {
  turn.tool_calls++;
}

// flushHold releases an incomplete token prefix as ordinary text.
// A finished turn must emit this before the digest is written, or the
// tail never reaches the owner and is also missing from the counts.
export function flushHold(turn: OpenTurn): string {
  const rest = turn.hold;
  turn.hold = "";
  if (!rest) return "";
  turn.deltas++;
  turn.chars += rest.length;
  return rest;
}

export function closeTurn(turn: OpenTurn, stop: Stop, now: Date = new Date()): TurnDigest {
  if (turn.hold) flushHold(turn);
  const ended = now.toISOString();
  const reason: Stop = stop === "abort" || stop === "error"
    ? stop
    : turn.stop_token
      ? "stop_token"
      : stop;
  turn.closed = true;
  const digest: TurnDigest = {
    type: "turn",
    ts: ended,
    turn_id: turn.turn_id,
    session_id: turn.session_id,
    cause: turn.cause,
    cause_detail: turn.cause_detail,
    started_at: turn.started_at,
    ended_at: ended,
    stop: reason,
    tool_calls: turn.tool_calls,
    deltas: turn.deltas,
    chars: turn.chars,
  };
  if (turn.stop_token) digest.stop_token = turn.stop_token;
  if (turn.resume) digest.resume = turn.resume;
  return digest;
}

// turnRefusal is the provider's reason when a turn ended on a refused
// request (usage limit, rate limit, auth) rather than on an answer, and ""
// otherwise. pi-agent-core resolves prompt() normally in that case and
// keeps the reason only on the last assistant message, so without this the
// owner sees a turn that answered nothing (🎯T137). An aborted turn is not
// a refusal.
export function turnRefusal(state: { messages?: unknown[] } | undefined): string {
  const messages = state?.messages;
  const last = (messages && messages.length > 0 ? messages[messages.length - 1] : undefined) as
    | { role?: string; stopReason?: string; errorMessage?: string }
    | undefined;
  if (last?.role !== "assistant" || last.stopReason !== "error") return "";
  return (last.errorMessage ?? "").trim() || "the provider ended the turn with an error";
}

// stripStopTokens is the non-streaming form used when no turn is open.
export function stripStopTokens(delta: string): { visible: string; token?: string } {
  const scratch: OpenTurn = {
    turn_id: "",
    seat: "",
    session_id: "",
    cause: "owner",
    cause_detail: "",
    started_at: "",
    tool_calls: 0,
    deltas: 0,
    chars: 0,
    hold: "",
    closed: false,
  };
  const visible = noteDelta(scratch, delta) + scratch.hold;
  scratch.hold = "";
  return { visible, token: scratch.stop_token };
}
