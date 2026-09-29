// Durable conversation store for a sidecar seat (🎯T151).
//
// A seat's pi-agent-core conversation used to live only in the sidecar's
// memory, so a sidecar restart lost every seat's history. Each seat now
// keeps its engine entries (🎯T150: messages and compaction entries) in one
// append-only JSONL file per seat and session, like the Oh My Pi CLI's
// session files. A restart that reloads the same seat and session rebuilds
// the conversation from it, the latest compaction included, without
// recomputing any summary.
//
// A store that cannot be read is a hard error for that seat. Handing it a
// fresh, empty conversation instead would silently lose everything it knew.
// The one repair is a torn last line — the signature of a crash mid-append —
// which is dropped, with a warning, because nothing after it was written.

import { appendFileSync, existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import type { SessionEntry } from "@oh-my-pi/pi-agent-core";

// storeVersion is written in each file's header line.
const storeVersion = 1;

type Header = { type: "claudia_seat"; v: number; seat: string; session: string };

// safeName keeps a seat or session id usable as one path component.
function safeName(s: string): string {
  return s.replace(/[^A-Za-z0-9._-]/g, "_");
}

export class SeatStore {
  readonly path: string;
  #seat: string;
  #session: string;

  constructor(root: string, seat: string, session: string) {
    this.#seat = seat;
    this.#session = session;
    this.path = join(root, safeName(seat), `${safeName(session)}.jsonl`);
  }

  // load returns the stored entries, or null when this seat and session have
  // never been stored. It throws when the file cannot be trusted.
  load(): SessionEntry[] | null {
    if (!existsSync(this.path)) return null;
    const text = readFileSync(this.path, "utf8");
    const lines = text.split("\n");
    // A file that ends mid-line was cut short while appending.
    const torn = text.length > 0 && !text.endsWith("\n");
    const entries: SessionEntry[] = [];
    let header = false;
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      if (line === "") continue;
      let rec: unknown;
      try {
        rec = JSON.parse(line);
      } catch (err) {
        if (torn && i === lines.length - 1) {
          console.error(`sidecar: ${this.path}: dropping a torn last line (a crash while appending)`);
          this.rewrite(entries);
          return entries;
        }
        throw new Error(`conversation store ${this.path} line ${i + 1} is not JSON: ${String(err)}`);
      }
      if (!header) {
        const h = rec as Partial<Header>;
        if (h.type !== "claudia_seat" || h.v !== storeVersion) {
          throw new Error(`conversation store ${this.path} has no claudia_seat v${storeVersion} header`);
        }
        header = true;
        continue;
      }
      const e = rec as Partial<SessionEntry>;
      if (!e || typeof e !== "object" || typeof e.type !== "string" || typeof e.id !== "string") {
        throw new Error(`conversation store ${this.path} line ${i + 1} is not an entry`);
      }
      entries.push(e as SessionEntry);
    }
    if (!header) throw new Error(`conversation store ${this.path} is empty`);
    return entries;
  }

  // append adds entries to the end of the file, creating it with its header.
  append(entries: readonly SessionEntry[]): void {
    if (entries.length === 0) return;
    if (!existsSync(this.path)) {
      this.rewrite(entries);
      return;
    }
    appendFileSync(this.path, entries.map((e) => JSON.stringify(e) + "\n").join(""));
  }

  // rewrite replaces the whole file atomically (write, then rename).
  rewrite(entries: readonly SessionEntry[]): void {
    mkdirSync(dirname(this.path), { recursive: true });
    const header: Header = { type: "claudia_seat", v: storeVersion, seat: this.#seat, session: this.#session };
    const body = [header, ...entries].map((e) => JSON.stringify(e) + "\n").join("");
    const tmp = `${this.path}.tmp-${process.pid}`;
    writeFileSync(tmp, body);
    renameSync(tmp, this.path);
  }
}
