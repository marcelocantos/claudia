// Host coding tools for OMP seats. These are ours, not pi-natives (🎯T864.3).
// Names follow the Claude CLI set so workers can actually edit a repo.

import fs from "node:fs";
import type { AgentTool } from "@oh-my-pi/pi-agent-core";
import { capToolText, toolResultBound, truncateToolResult } from "./truncate.ts";
import { recordReadView } from "./readguard.ts";

import { runCommand, runShell, under, writeFile, type SandboxPolicy } from "./execution.ts";
export { runCommand, realish, writableRoots, checkWrite, seatbeltProfile, under, type RunResult, type SandboxPolicy } from "./execution.ts";

const maxOut = 200_000;
const bashTimeoutMs = 60_000;
// A model may ask for longer (builds, test gates), up to the same ceiling the
// Cursor shell path allows. 1254 commands hit the fixed 60s bound by 2026-10-28.
const bashMaxTimeoutMs = 600_000;
const bashMinTimeoutMs = 1_000;
const searchTimeoutMs = 15_000;

export function codingTools(
  cwdOf: () => string,
  boundOf: () => number = () => toolResultBound(0),
  policyOf: () => SandboxPolicy | undefined = () => undefined,
): AgentTool[] {
  const policy = policyOf();
  if (policy?.mode === "read-only") {
    return [readTool(cwdOf, boundOf, policyOf), globTool(cwdOf, boundOf), grepTool(cwdOf, boundOf, policyOf)];
  }
  return [
    bashTool(cwdOf, boundOf, policyOf),
    readTool(cwdOf, boundOf, policyOf),
    writeTool(cwdOf, boundOf, policyOf),
    globTool(cwdOf, boundOf),
    grepTool(cwdOf, boundOf, policyOf),
  ];
}

function bashTool(cwdOf: () => string, boundOf: () => number, policyOf: () => SandboxPolicy | undefined): AgentTool {
  return {
    name: "Bash",
    label: "Bash",
    description: "Run a shell command in the seat working directory. Returns stdout and stderr.",
    parameters: {
      type: "object",
      properties: {
        command: { type: "string", description: "shell command" },
        timeout: {
          type: "number",
          description: "seconds before the command is killed (default 60, max 600); raise it for builds and test runs",
        },
      },
      required: ["command"],
    },
    execute: async (_id: string, params: unknown) => {
      const p = params as { command?: string; timeout?: number };
      const command = String(p?.command || "");
      if (!command) return textResult("missing command", boundOf(), "Bash");
      const timeoutMs =
        typeof p?.timeout === "number" && p.timeout > 0
          ? Math.min(Math.max(p.timeout * 1000, bashMinTimeoutMs), bashMaxTimeoutMs)
          : bashTimeoutMs;
      try {
        const r = await runShell(cwdOf, policyOf, { command, timeoutMs, maxBuffer: maxOut });
        const out = `${r.stdout || ""}${r.stderr || ""}`.slice(0, maxOut);
        const status = r.status ?? (r.error ? 1 : 0);
        const hint =
          r.error?.code === "ETIMEDOUT" && timeoutMs < bashMaxTimeoutMs
            ? `\n(killed at ${timeoutMs / 1000}s; pass timeout up to ${bashMaxTimeoutMs / 1000} seconds for a longer command)`
            : "";
        return textResult(status === 0 ? out || "(no output)" : `exit ${status}\n${out || r.error?.message || ""}${hint}`, boundOf(), "Bash");
      } catch (err) {
        return { ...textResult(String(err), boundOf(), "Bash"), isError: true };
      }
    },
  } as AgentTool;
}

function readTool(cwdOf: () => string, boundOf: () => number, policyOf: () => SandboxPolicy | undefined): AgentTool {
  return {
    name: "Read",
    label: "Read",
    description: "Read a UTF-8 file relative to the seat working directory.",
    parameters: {
      type: "object",
      properties: { path: { type: "string" } },
      required: ["path"],
    },
    execute: async (_id: string, params: unknown) => {
      const p = String((params as { path?: string })?.path || "");
      try {
        const abs = under(cwdOf(), p, policyOf());
        const body = fs.readFileSync(abs, "utf8");
        const bound = boundOf();
        const capped = truncateToolResult(body, bound, "Read");
        recordReadView(abs, body, capped.text);
        return textResult(capped.text, bound, "Read");
      } catch (err) {
        return textResult(String(err), boundOf(), "Read");
      }
    },
  } as AgentTool;
}

function writeTool(cwdOf: () => string, boundOf: () => number, policyOf: () => SandboxPolicy | undefined): AgentTool {
  return {
    name: "Write",
    label: "Write",
    description: "Write a UTF-8 file relative to the seat working directory. Creates parent dirs.",
    parameters: {
      type: "object",
      properties: {
        path: { type: "string" },
        content: { type: "string" },
      },
      required: ["path", "content"],
    },
    execute: async (_id: string, params: unknown) => {
      const p = params as { path?: string; content?: string };
      try {
        const abs = writeFile(cwdOf(), String(p?.path || ""), String(p?.content ?? ""), policyOf());
        return textResult("wrote " + abs, boundOf(), "Write");
      } catch (err) {
        return textResult(String(err), boundOf(), "Write");
      }
    },
  } as AgentTool;
}

function globTool(cwdOf: () => string, boundOf: () => number): AgentTool {
  return {
    name: "Glob",
    label: "Glob",
    description: "List files under the seat working directory matching a glob pattern.",
    parameters: {
      type: "object",
      properties: { pattern: { type: "string" } },
      required: ["pattern"],
    },
    execute: async (_id: string, params: unknown) => {
      const pattern = String((params as { pattern?: string })?.pattern || "*");
      const cwd = cwdOf() || process.cwd();
      const r = await runCommand("/usr/bin/find", [cwd, "-name", pattern], {
        timeoutMs: searchTimeoutMs,
        maxBuffer: maxOut,
      });
      const lines = (r.stdout || "").split("\n").filter(Boolean).slice(0, 500);
      return textResult(lines.join("\n") || "(no matches)", boundOf(), "Glob");
    },
  } as AgentTool;
}

function grepTool(cwdOf: () => string, boundOf: () => number, policyOf: () => SandboxPolicy | undefined): AgentTool {
  return {
    name: "Grep",
    label: "Grep",
    description: "Search file contents under the seat working directory (ripgrep if present).",
    parameters: {
      type: "object",
      properties: {
        pattern: { type: "string" },
        path: { type: "string" },
      },
      required: ["pattern"],
    },
    execute: async (_id: string, params: unknown) => {
      const p = params as { pattern?: string; path?: string };
      const cwd = cwdOf() || process.cwd();
      const target = p?.path ? under(cwd, p.path, policyOf()) : cwd;
      const rg = await runCommand("rg", ["--no-config", "--no-follow", "-n", "--max-count", "50", "--", String(p?.pattern || ""), target], {
        timeoutMs: searchTimeoutMs,
        maxBuffer: maxOut,
      });
      if (rg.error && rg.error.code === "ENOENT") {
        const g = await runCommand("/usr/bin/grep", ["-r", "-n", "--", String(p?.pattern || ""), target], {
          timeoutMs: searchTimeoutMs,
          maxBuffer: maxOut,
        });
        return textResult((g.stdout || g.stderr || "").slice(0, maxOut) || "(no matches)", boundOf(), "Grep");
      }
      return textResult((rg.stdout || rg.stderr || "").slice(0, maxOut) || "(no matches)", boundOf(), "Grep");
    },
  } as AgentTool;
}

function textResult(text: string, bound: number, tool: string) {
  return { content: [{ type: "text" as const, text: capToolText(text, bound, tool) }], details: {} };
}
