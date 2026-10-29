// Dated session log for the Oh My Pi sidecar (🎯T866.1).
// One global file per UTC day. The shim does not compress.

import { appendFileSync, existsSync, mkdirSync, readdirSync } from "node:fs";
import { isAbsolute, join } from "node:path";
import { homedir } from "node:os";
import type { UsageFact } from "./usage.ts";

// A diagnostic receipt is the durable request fact, not the message it
// accepted. Unknown fields are dropped so a snapshot or payload cannot
// ride along under another name.
export type SpoolReceipt = {
  request_id: string;
  session_id?: string;
  seat?: string;
  state: string;
  attempt_id?: string;
  turn_ids?: string[];
  reason?: string;
  accepted_at?: string;
  updated_at?: string;
};

const diagnosticStates = new Set(["accepted", "started", "completed", "cancelled", "failed", "unknown"]);
const diagnosticIDLimit = 128;
const diagnosticReasonLimit = 200;
const diagnosticTurnLimit = 32;

function diagnosticID(value: unknown): string | undefined {
  if (typeof value !== "string") return undefined;
  const id = value.trim();
  if (!id || id.length > diagnosticIDLimit) return undefined;
  return id;
}

function diagnosticText(value: unknown, limit: number): string | undefined {
  if (typeof value !== "string") return undefined;
  const text = value.trim();
  if (!text || text.length > limit) return undefined;
  return text;
}

// spoolReceipt keeps identity and state from a socket receipt. A receipt
// that is not that shape is omitted rather than stored whole.
export function spoolReceipt(value: unknown): SpoolReceipt | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
  const raw = value as Record<string, unknown>;
  const request_id = diagnosticID(raw.request_id);
  const state = diagnosticText(raw.state, diagnosticIDLimit);
  if (!request_id || !state || !diagnosticStates.has(state)) return undefined;
  const receipt: SpoolReceipt = { request_id, state };
  const session_id = diagnosticID(raw.session_id);
  const seat = diagnosticID(raw.seat);
  const attempt_id = diagnosticID(raw.attempt_id);
  const reason = diagnosticText(raw.reason, diagnosticReasonLimit);
  const accepted_at = diagnosticText(raw.accepted_at, diagnosticIDLimit);
  const updated_at = diagnosticText(raw.updated_at, diagnosticIDLimit);
  if (session_id) receipt.session_id = session_id;
  if (seat) receipt.seat = seat;
  if (attempt_id) receipt.attempt_id = attempt_id;
  if (reason) receipt.reason = reason;
  if (accepted_at) receipt.accepted_at = accepted_at;
  if (updated_at) receipt.updated_at = updated_at;
  if (Array.isArray(raw.turn_ids)) {
    const turn_ids: string[] = [];
    for (const id of raw.turn_ids) {
      const turn = diagnosticID(id);
      if (!turn) return undefined;
      turn_ids.push(turn);
      if (turn_ids.length > diagnosticTurnLimit) return undefined;
    }
    if (turn_ids.length > 0) receipt.turn_ids = turn_ids;
  }
  return receipt;
}

export type SpoolRecord = {
  ts: string;
  seat: string;
  type: string;
  stop_reason?: string;
  text?: string;
  call_id?: string;
  name?: string;
  provider?: string;
  model?: string;
  // Accepted by older callers. append does not persist it: the live socket
  // still carries the agent state, and the spool is not a second journal.
  snapshot?: unknown;
  usage_fact?: UsageFact;
  request_id?: string;
  receipt?: SpoolReceipt;
  turn_id?: string;
  session_id?: string;
  cause?: string;
  cause_detail?: string;
  started_at?: string;
  ended_at?: string;
  stop?: string;
  tool_calls?: number;
  deltas?: number;
  chars?: number;
  stop_token?: string;
  resume?: string;
};

const filePrefix = "events-";
const fileSuffix = ".log";

export function spoolDir(override?: string): string {
  if (override?.trim()) return override.trim();
  const env = process.env.CLAUDIA_OMP_SPOOL_DIR?.trim();
  if (env) return env;
  const stateHome = process.env.XDG_STATE_HOME?.trim();
  return stateHome && isAbsolute(stateHome)
    ? join(stateHome, "claudia", "spool")
    : join(homedir(), ".local", "state", "claudia", "spool");
}

export function dayUTC(ts: Date | string): string {
  const d = typeof ts === "string" ? new Date(ts) : ts;
  return d.toISOString().slice(0, 10);
}

export function fileNameForDay(day: string): string {
  return `${filePrefix}${day}${fileSuffix}`;
}

export function parseDayFromName(name: string): string | undefined {
  if (!name.startsWith(filePrefix) || !name.endsWith(fileSuffix)) return undefined;
  const day = name.slice(filePrefix.length, -fileSuffix.length);
  return /^\d{4}-\d{2}-\d{2}$/.test(day) ? day : undefined;
}

export class Spool {
  readonly dir: string;
  private liveDay: string;
  private readonly closed = new Set<string>();

  constructor(dir: string, now: Date = new Date()) {
    this.dir = dir;
    mkdirSync(dir, { recursive: true, mode: 0o700 });
    this.liveDay = dayUTC(now);
    this.closeOlderThan(this.liveDay);
  }

  closeOlderThan(live: string): void {
    this.liveDay = live;
    if (!existsSync(this.dir)) return;
    for (const name of readdirSync(this.dir)) {
      const day = parseDayFromName(name);
      if (day && day < live) this.closed.add(day);
    }
  }

  isClosed(day: string): boolean {
    return this.closed.has(day) || day < this.liveDay;
  }

  pathForDay(day: string): string {
    return join(this.dir, fileNameForDay(day));
  }

  // Append one newline-delimited record. The file date is the event's
  // own timestamp unless that day is already closed — then the live
  // day gets the line and the record keeps the original ts.
  append(rec: SpoolRecord, now: Date = new Date()): string {
    if (!rec.seat) throw new Error("spool record must name its seat");
    const ts = rec.ts || now.toISOString();
    const eventDay = dayUTC(ts);
    this.roll(dayUTC(now));
    const fileDay = this.isClosed(eventDay) ? this.liveDay : eventDay;
    if (fileDay > this.liveDay) {
      this.closed.add(this.liveDay);
      this.liveDay = fileDay;
    }
    // type sits near the front so a search can name the line without
    // reading a payload (🎯T870). Agent snapshots are not persisted.
    const line = JSON.stringify({
      ts,
      seat: rec.seat,
      type: rec.type,
      stop_reason: rec.stop_reason,
      turn_id: rec.turn_id,
      session_id: rec.session_id,
      cause: rec.cause,
      cause_detail: rec.cause_detail,
      started_at: rec.started_at,
      ended_at: rec.ended_at,
      stop: rec.stop,
      tool_calls: rec.tool_calls,
      deltas: rec.deltas,
      chars: rec.chars,
      stop_token: rec.stop_token,
      resume: rec.resume,
      text: rec.text,
      call_id: rec.call_id,
      usage_fact: rec.usage_fact,
      request_id: rec.request_id,
      receipt: rec.receipt,
      name: rec.name,
      provider: rec.provider,
      model: rec.model,
    }) + "\n";
    const path = this.pathForDay(fileDay);
    appendFileSync(path, line);
    return path;
  }

  private roll(today: string): void {
    if (today > this.liveDay) {
      this.closed.add(this.liveDay);
      this.liveDay = today;
      this.closeOlderThan(today);
    }
  }
}

let defaultSpool: Spool | undefined;

export function defaultWriter(): Spool {
  if (!defaultSpool) defaultSpool = new Spool(spoolDir());
  return defaultSpool;
}

export function resetDefaultWriter(): void {
  defaultSpool = undefined;
}

if (import.meta.main) {
  const cmd = process.argv[2];
  const dir = process.argv[3];
  if (cmd === "append" && dir) {
    const rec = JSON.parse(process.argv[4] ?? "{}") as SpoolRecord;
    const now = process.argv[5] ? new Date(process.argv[5]) : new Date();
    const spool = new Spool(dir, now);
    process.stdout.write(spool.append(rec, now) + "\n");
  } else {
    console.error("usage: bun spool.ts append <dir> <record-json> [now-iso]");
    process.exit(2);
  }
}
