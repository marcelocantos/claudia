// Cursor exec handlers (Pimp 2026-10-13): Cursor's native tools (read, ls,
// grep, write, delete, shell and the modern pi_* frames) reach the host
// through the Agent's cursorExecHandlers, not through setTools. Without
// them every Cursor tool call answered "Tool not available" and a cursor
// seat could do nothing. These run the same operations as coding.ts under
// the same confinement: every path resolves inside the seat workdir, a
// shell runs there (or in a subdirectory of it), and the branch guard
// (🎯T164) still refuses a checkout of the shared clone.

import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import type { CursorExecHandlers, ToolResultMessage } from "@oh-my-pi/pi-ai";
import { guardBranchCheckout } from "./branchguard.ts";
import { runCommand, under } from "./coding.ts";
import { capToolText } from "./truncate.ts";

const maxOut = 200_000;
const shellDefaultMs = 60_000;
const shellMaxMs = 600_000;
const searchTimeoutMs = 15_000;

type Any = Record<string, any>;

function result(id: string | undefined, tool: string, text: string, isError: boolean, bound: number): ToolResultMessage {
  return {
    role: "toolResult",
    toolCallId: id || crypto.randomUUID(),
    toolName: tool,
    content: [{ type: "text", text: capToolText(text, bound, tool) }],
    isError,
    timestamp: Date.now(),
  } as ToolResultMessage;
}

const rangeRe = /^L?(\d+)(?:([-+]|\.\.)L?(\d+)?)?$/i;

// readSelector strips OMP's inline read selectors (`path:raw`,
// `path:N+K`, `path:N-M`, `path:N-`, and `:raw:N+K`) that the provider
// composes onto a read path, returning the file and a 1-based line window.
export function readSelector(p: string): { file: string; start?: number; count?: number; end?: number } {
  const chunks = p.split(":");
  let start: number | undefined, count: number | undefined, end: number | undefined;
  for (let i = 0; i < 2 && chunks.length > 1; i++) {
    const last = chunks[chunks.length - 1]!;
    if (last.toLowerCase() === "raw") {
      chunks.pop();
      continue;
    }
    const m = rangeRe.exec(last);
    if (!m || start !== undefined) break;
    chunks.pop();
    start = Number(m[1]);
    if (m[2] === "+" && m[3]) count = Number(m[3]);
    else if (m[2] && m[3]) end = Number(m[3]);
    else if (!m[2]) end = start;
  }
  return { file: chunks.join(":"), start, count, end };
}

function sliceLines(body: string, start?: number, count?: number, end?: number): string {
  if (start === undefined && count === undefined && end === undefined) return body;
  const lines = body.split("\n");
  const from = Math.max(0, (start ?? 1) - 1);
  const to = count !== undefined ? from + count : end !== undefined ? end : lines.length;
  return lines.slice(from, to).join("\n");
}

export function cursorExecHandlers(cwdOf: () => string, boundOf: () => number): CursorExecHandlers {
  const root = () => cwdOf() || process.cwd();

  const read = (id: string | undefined, tool: string, p: string, offset?: number, limit?: number) => {
    try {
      const sel = readSelector(String(p || ""));
      const abs = under(root(), sel.file);
      if (fs.statSync(abs).isDirectory()) return list(id, tool, abs);
      const body = fs.readFileSync(abs, "utf8");
      const start = offset !== undefined ? Math.max(1, offset) : sel.start;
      const count = limit !== undefined ? limit : sel.count;
      return result(id, tool, sliceLines(body, start, count, limit !== undefined ? undefined : sel.end), false, boundOf());
    } catch (err) {
      return result(id, tool, String(err), true, boundOf());
    }
  };
  const list = (id: string | undefined, tool: string, abs: string, limit?: number) => {
    const names = fs.readdirSync(abs, { withFileTypes: true }).map((d) => (d.isDirectory() ? d.name + "/" : d.name)).sort();
    return result(id, tool, names.slice(0, limit && limit > 0 ? limit : 1000).join("\n") || "(empty)", false, boundOf());
  };
  const ls = (id: string | undefined, p: string | undefined, limit?: number) => {
    try {
      return list(id, "read", under(root(), p || "."), limit);
    } catch (err) {
      return result(id, "read", String(err), true, boundOf());
    }
  };
  const write = (id: string | undefined, tool: string, p: string, content: string) => {
    try {
      const abs = under(root(), String(p || ""));
      fs.mkdirSync(path.dirname(abs), { recursive: true });
      fs.writeFileSync(abs, content, "utf8");
      return result(id, tool, "wrote " + abs, false, boundOf());
    } catch (err) {
      return result(id, tool, String(err), true, boundOf());
    }
  };
  const shell = async (id: string | undefined, command: string, wd: string | undefined, timeoutMs: number) => {
    try {
      const cwd = wd ? under(root(), wd) : root();
      const refusal = await guardBranchCheckout(cwd, command);
      if (refusal && refusal.verdict === "deny") return result(id, "bash", refusal.message, true, boundOf());
      const r = await runCommand("/bin/bash", ["-lc", command], {
        cwd,
        timeoutMs: Math.min(Math.max(timeoutMs, 1000), shellMaxMs),
        maxBuffer: maxOut,
        env: process.env,
      });
      const out = `${r.stdout || ""}${r.stderr || ""}`.slice(0, maxOut);
      const status = r.status ?? (r.error ? 1 : 0);
      return result(id, "bash", status === 0 ? out || "(no output)" : `exit ${status}\n${out || r.error?.message || ""}`, status !== 0, boundOf());
    } catch (err) {
      return result(id, "bash", String(err), true, boundOf());
    }
  };
  const grep = async (id: string | undefined, a: { pattern: string; path?: string; glob?: string; ignoreCase?: boolean; literal?: boolean; limit?: number }) => {
    try {
      const target = under(root(), a.path || ".");
      const args = ["-n", "--max-count", "50"];
      if (a.ignoreCase) args.push("-i");
      if (a.literal) args.push("-F");
      if (a.glob) args.push("--glob", a.glob);
      args.push("--", a.pattern, target);
      const r = await runCommand("rg", args, { timeoutMs: searchTimeoutMs, maxBuffer: maxOut });
      let lines = (r.stdout || "").split("\n").filter(Boolean);
      if (a.limit && a.limit > 0) lines = lines.slice(0, a.limit);
      if (r.error && r.error.code === "ENOENT") return result(id, "grep", "rg not found", true, boundOf());
      return result(id, "grep", lines.join("\n") || "(no matches)", false, boundOf());
    } catch (err) {
      return result(id, "grep", String(err), true, boundOf());
    }
  };
  const find = async (id: string | undefined, pattern: string, p?: string, limit?: number) => {
    try {
      const target = under(root(), p || ".");
      const r = await runCommand("/usr/bin/find", [target, "-name", pattern || "*"], { timeoutMs: searchTimeoutMs, maxBuffer: maxOut });
      const lines = (r.stdout || "").split("\n").filter(Boolean).slice(0, limit && limit > 0 ? limit : 500);
      return result(id, "find", lines.join("\n") || "(no matches)", false, boundOf());
    } catch (err) {
      return result(id, "find", String(err), true, boundOf());
    }
  };

  return {
    read: async (a: Any) => read(a.toolCallId, "read", a.path, a.offset, a.limit),
    ls: async (a: Any) => ls(a.toolCallId, a.path),
    grep: async (a: Any) => grep(a.toolCallId, { pattern: a.pattern, path: a.path, glob: a.glob, ignoreCase: a.caseInsensitive, limit: a.headLimit }),
    write: async (a: Any) =>
      write(a.toolCallId, "write", a.path, a.fileBytes && a.fileBytes.length > 0 ? new TextDecoder().decode(a.fileBytes) : String(a.fileText ?? "")),
    delete: async (a: Any) => {
      try {
        const abs = under(root(), String(a.path || ""));
        if (abs === path.resolve(root())) throw new Error("refusing to delete the workdir");
        fs.rmSync(abs);
        return result(a.toolCallId, "delete", "deleted " + abs, false, boundOf());
      } catch (err) {
        return result(a.toolCallId, "delete", String(err), true, boundOf());
      }
    },
    shell: async (a: Any) => shell(a.toolCallId, String(a.command || ""), a.workingDirectory, a.timeout && a.timeout > 0 ? a.timeout : shellDefaultMs),
    shellStream: async (a: Any) => shell(a.toolCallId, String(a.command || ""), a.workingDirectory, a.timeout && a.timeout > 0 ? a.timeout : shellDefaultMs),
    piRead: async (c: Any) => read(c.toolCallId, "read", c.args.path, c.args.offset, c.args.limit),
    piBash: async (c: Any) => shell(c.toolCallId, String(c.args.command || ""), undefined, c.args.timeout && c.args.timeout > 0 ? c.args.timeout * 1000 : shellDefaultMs),
    piWrite: async (c: Any) => write(c.toolCallId, "write", c.args.path, String(c.args.content ?? "")),
    piEdit: async (c: Any) => {
      try {
        const abs = under(root(), String(c.args.path || ""));
        let body = fs.readFileSync(abs, "utf8");
        for (const e of (c.args.edits ?? []) as { oldText: string; newText: string }[]) {
          const at = body.indexOf(e.oldText);
          if (at < 0) throw new Error(`old text not found in ${c.args.path}`);
          body = body.slice(0, at) + e.newText + body.slice(at + e.oldText.length);
        }
        fs.writeFileSync(abs, body, "utf8");
        return result(c.toolCallId, "edit", "edited " + abs, false, boundOf());
      } catch (err) {
        return result(c.toolCallId, "edit", String(err), true, boundOf());
      }
    },
    piGrep: async (c: Any) => grep(c.toolCallId, c.args),
    piFind: async (c: Any) => find(c.toolCallId, c.args.pattern, c.args.path, c.args.limit),
    piLs: async (c: Any) => ls(c.toolCallId, c.args.path, c.args.limit),
  } as CursorExecHandlers;
}
