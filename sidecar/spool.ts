// Dated session log for the Oh My Pi sidecar (🎯T866.1).
// One global file per UTC day. The shim does not compress.

import { appendFileSync, existsSync, mkdirSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { homedir } from "node:os";

export type SpoolRecord = {
  ts: string;
  seat: string;
  type: string;
  text?: string;
  call_id?: string;
  name?: string;
  provider?: string;
  model?: string;
  snapshot?: unknown;
};

const filePrefix = "events-";
const fileSuffix = ".log";

export function spoolDir(override?: string): string {
  if (override) return override;
  const env = process.env.JEVONS_SPOOL_DIR;
  if (env) return env;
  return join(homedir(), ".jevons", "spool");
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
    const line = JSON.stringify({ ...rec, ts, seat: rec.seat }) + "\n";
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
