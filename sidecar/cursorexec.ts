// Cursor exec handlers (Pimp 2026-10-13): Cursor's native tools (read, ls,
// grep, write, delete, shell and the modern pi_* frames) reach the host
// through the Agent's cursorExecHandlers, not through setTools. Without
// them every Cursor tool call answered "Tool not available" and a cursor
// seat could do nothing. These run the same operations as coding.ts under
// the shared execution policy: current seat roots, read-only/protected
// grants, shell sandbox and the branch guard (🎯T164).

import crypto from "node:crypto";
import fs from "node:fs";
import type { CursorExecHandlers, ToolResultMessage } from "@oh-my-pi/pi-ai";
import { runCommand, runShell, under, writeFile, deleteFile, editFile, type SandboxPolicy } from "./execution.ts";
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

export function cursorExecHandlers(cwdOf: () => string, boundOf: () => number, policyOf: () => SandboxPolicy | undefined = () => undefined): CursorExecHandlers {
  const root = () => cwdOf() || process.cwd();

  const read = (id: string | undefined, tool: string, p: string, offset?: number, limit?: number) => {
    try {
      const sel = readSelector(String(p || ""));
      const abs = under(root(), sel.file, policyOf());
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
      return list(id, "read", under(root(), p || ".", policyOf()), limit);
    } catch (err) {
      return result(id, "read", String(err), true, boundOf());
    }
  };
  const write = (id: string | undefined, tool: string, p: string, content: string) => {
    try {
      const abs = writeFile(root(), String(p || ""), content, policyOf());
      return result(id, tool, "wrote " + abs, false, boundOf());
    } catch (err) {
      return result(id, tool, String(err), true, boundOf());
    }
  };
  const shell = async (id: string | undefined, command: string, wd: string | undefined, timeoutMs: number) => {
    try {
      const r = await runShell(cwdOf, policyOf, {
        command,
        workingDirectory: wd,
        timeoutMs: Math.min(Math.max(timeoutMs, 1000), shellMaxMs),
        maxBuffer: maxOut,
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
      const target = under(root(), a.path || ".", policyOf());
      const args = ["--no-config", "--no-follow", "-n", "--max-count", "50"];
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
      const target = under(root(), p || ".", policyOf());
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
        const abs = deleteFile(root(), String(a.path || ""), policyOf());
        return result(a.toolCallId, "delete", "deleted " + abs, false, boundOf());
      } catch (err) {
        return result(a.toolCallId, "delete", String(err), true, boundOf());
      }
    },
    shell: async (a: Any) => shell(a.toolCallId, String(a.command || ""), a.workingDirectory, a.timeout && a.timeout > 0 ? a.timeout : shellDefaultMs),
    // This runner collects a bounded batch result. Do not advertise shellStream:
    // Cursor then expects output through callbacks and suppresses the batch
    // stdout/stderr. The pinned provider routes streamed frames to shell when
    // shellStream is absent, forwarding its result before the exit/close frames.
    piRead: async (c: Any) => read(c.toolCallId, "read", c.args.path, c.args.offset, c.args.limit),
    piBash: async (c: Any) => shell(c.toolCallId, String(c.args.command || ""), undefined, c.args.timeout && c.args.timeout > 0 ? c.args.timeout * 1000 : shellDefaultMs),
    piWrite: async (c: Any) => write(c.toolCallId, "write", c.args.path, String(c.args.content ?? "")),
    piEdit: async (c: Any) => {
      try {
        const abs = editFile(root(), String(c.args.path || ""), c.args.edits ?? [], policyOf());
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
