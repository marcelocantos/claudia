// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// A pre-exec guard for signal commands in the sidecar's own shell tools.
// Provenance is deliberately process-local: a restarted broker cannot inherit
// an unverifiable PID claim. A PID is never ownership evidence by itself.
import path from "node:path";
import { runCommand } from "./process.ts";

export type ProcessRow = { pid: number; ppid: number; pgid: number; started: string; name: string; command: string };
export type ProcessSnapshot = () => Promise<ProcessRow[] | null>;

export const hostProcesses: ProcessSnapshot = async () => {
  let r;
  try { r = await runCommand("/bin/ps", ["-axo", "pid=,ppid=,pgid=,lstart=,comm=,command="],
    { timeoutMs: 5000, maxBuffer: 4 * 1024 * 1024 }); } catch { return null; }
  if (r.error || r.status !== 0) return null;
  const rows: ProcessRow[] = [];
  for (const line of r.stdout.split("\n")) {
    if (!line.trim()) continue;
    const m = /^\s*(\d+)\s+(\d+)\s+(\d+)\s+((?:\w{3}\s+){2}\d{1,2}\s+\d\d:\d\d:\d\d\s+\d{4})\s+(\S+)\s+(.*)$/.exec(line);
    if (!m) return null; // an incomplete table cannot authorize a signal
    rows.push({ pid: +m[1], ppid: +m[2], pgid: +m[3], started: m[4], name: m[5], command: m[6] });
  }
  return rows.length ? rows : null;
};

export class SeatProcessOwnership {
  constructor(readonly snapshot: ProcessSnapshot = hostProcesses) {}
  private groups = new Map<number, string>();
  // Called on the child-process launch path. Only a process identity obtained
  // from the OS (never model text, cwd or an environment variable) is trusted.
  async launched(pid: number, snapshot: ProcessSnapshot = this.snapshot): Promise<void> {
    let rows: ProcessRow[] | null;
    try { rows = await snapshot(); } catch { return; }
    const leader = rows?.find((r) => r.pid === pid && r.pgid === pid);
    if (leader) this.groups.set(pid, leader.started);
  }
  owns(row: ProcessRow, rows: ProcessRow[]): boolean {
    const start = this.groups.get(row.pgid);
    if (!start) return false;
    const leader = rows.find((r) => r.pid === row.pgid && r.pgid === row.pgid && r.started === start);
    // Only a still-running leader anchors group identity; refuse after it
    // exits rather than guessing about an orphaned group or reused PGID.
    return !!leader && (row.pid === leader.pid ? row.started === leader.started :
      Date.parse(row.started) >= Date.parse(leader.started));
  }
}

// Parse only the shell words needed for signal admission. Preserve quoted
// arguments as words, but do not execute expansion. Unknown syntax at a kill
// site is denied rather than interpreted as a PID.
export function shellCommands(input: string): string[][] {
  const out: string[][] = [];
  let words: string[] = [], word = "", active = false, quote = "";
  const push = () => { if (active) words.push(word); word = ""; active = false; };
  const end = () => { push(); if (words.length) out.push(words); words = []; };
  for (let i = 0; i < input.length; i++) {
    const c = input[i];
    if (quote) {
      if (c === quote) { quote = ""; continue; }
      if (c === "\\" && quote === '"' && i + 1 < input.length) { word += input[++i]; continue; }
      word += c; continue;
    }
    if (c === "'" || c === '"') { quote = c; active = true; continue; }
    if (c === "\\" && i + 1 < input.length) { word += input[++i]; active = true; continue; }
    if (/\s/.test(c)) { if (c === "\n") end(); else push(); continue; }
    if (";&|".includes(c)) { end(); if (input[i + 1] === c) i++; continue; }
    word += c; active = true;
  }
  end();
  return out;
}

const prefixes = new Set(["env", "sudo", "nice", "nohup", "time", "command", "exec", "builtin"]);
function action(words: string[]): string[] {
  let i = 0;
  while (i < words.length) {
    if (/^[A-Za-z_][A-Za-z_0-9]*=/.test(words[i]) || prefixes.has(path.basename(words[i]))) { i++; continue; }
    break;
  }
  return words.slice(i);
}
function candidateRows(rows: ProcessRow[], needle: string, full: boolean): ProcessRow[] {
  if (!needle) return rows;
  try {
    const re = new RegExp(needle);
    return rows.filter((r) => re.test(full ? r.command : path.basename(r.name)));
  } catch { return rows; } // never infer that a malformed pattern matches nobody
}
function names(rows: ProcessRow[]): string {
  return rows.length ? rows.slice(0, 30).map((r) => `${r.pid} ${r.name} (${r.command})`).join("; ") +
    (rows.length > 30 ? `; and ${rows.length - 30} more` : "") : "none observed";
}

export async function guardKill(cwd: string, command: string, owner: SeatProcessOwnership,
  snapshot: ProcessSnapshot = owner.snapshot): Promise<void> {
  const commands = shellCommands(command).map(action);
  // Reject dynamic command names and nested command substitutions with signal
  // verbs: the outer command may itself be harmless but its expansion is not.
  if (commands.some((w) => /^\$/.test(w[0] || "")) || /\$\([^)]*\b(?:pkill|killall|kill)\b/.test(command)) {
    const rows = await snapshot();
    throw new Error(`killguard: refusing unresolved shell signal expansion before execution; candidate processes/PIDs: ${rows ? names(rows) : "snapshot unavailable"}; no signal sent`);
  }
  const dangerous = commands.filter((w) => ["pkill", "killall", "kill"].includes(path.basename(w[0] || "")));
  if (!dangerous.length) {
    // Nested shell/eval/alias execution is opaque to this tokenizer. Refuse
    // visible signal verbs inside those forms instead of claiming safety.
    if (/\b(?:pkill|killall|kill)\b/.test(command) &&
        !commands.every((w) => ["echo", "printf"].includes(path.basename(w[0] || "")))) {
      const rows = await snapshot();
      throw new Error(`killguard: refusing opaque shell signal command before execution; candidate processes/PIDs: ${rows ? names(rows) : "snapshot unavailable"}; no signal sent`);
    }
    return;
  }
  // No command is launched until the complete command line has been checked.
  const rows = await snapshot();
  if (!rows) throw new Error("killguard: refusing signal command: process snapshot unavailable; no signal sent");
  for (const w of dangerous) {
    const cmd = path.basename(w[0]);
    if (cmd === "pkill" || cmd === "killall") {
      const opts = w.slice(1);
      const pattern = opts.filter((x) => !x.startsWith("-")).at(-1) || "";
      const candidates = candidateRows(rows, pattern, cmd === "pkill" && opts.some((x) => x.includes("f")));
      throw new Error(`killguard: refusing ${cmd} before signal; candidate processes/PIDs (read-only snapshot): ${names(candidates)}; command was not run`);
    }
    const args = w.slice(1);
    // Only a literal positive decimal PID is authorizable. Flags such as
    // -TERM/-s, negative/group targets, substitutions, variables and PID 0
    // are denied; shell expansion cannot turn a verified PID into another.
    if (args.length !== 1 || !/^[1-9]\d*$/.test(args[0])) {
      const pgrep = /\bpgrep\b/.test(command);
      const match = /\bpgrep\s+([^)]*)/.exec(command);
      const parts = match?.[1].trim().split(/\s+/) ?? [];
      const candidates = pgrep ? candidateRows(rows, parts.filter((x) => !x.startsWith("-")).at(-1) || "", parts.some((x) => x.includes("f"))) : rows.filter((r) => args.includes(String(r.pid)));
      throw new Error(`killguard: refusing ${pgrep ? "kill $(pgrep …)" : "unresolved/group kill"} before signal; candidate processes/PIDs: ${names(candidates)}; command was not run`);
    }
    const pid = Number(args[0]);
    const row = rows.find((r) => r.pid === pid);
    if (!row || !owner.owns(row, rows)) {
      throw new Error(`killguard: refusing foreign, reused or unowned PID ${pid}; candidate processes/PIDs: ${names(row ? [row] : [])}; command was not run`);
    }
  }
}
