// Host coding tools for OMP seats. These are ours, not pi-natives (🎯T864.3).
// Names follow the Claude CLI set so workers can actually edit a repo.

import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { AgentTool } from "@oh-my-pi/pi-agent-core";

const maxOut = 200_000;
const bashTimeoutMs = 60_000;

export function codingTools(cwdOf: () => string): AgentTool[] {
  return [bashTool(cwdOf), readTool(cwdOf), writeTool(cwdOf), globTool(cwdOf), grepTool(cwdOf)];
}

function bashTool(cwdOf: () => string): AgentTool {
  return {
    name: "Bash",
    label: "Bash",
    description: "Run a shell command in the seat working directory. Returns stdout and stderr.",
    parameters: {
      type: "object",
      properties: { command: { type: "string", description: "shell command" } },
      required: ["command"],
    },
    execute: async (_id: string, params: unknown) => {
      const command = String((params as { command?: string })?.command || "");
      if (!command) return textResult("missing command");
      const cwd = cwdOf() || process.cwd();
      const r = spawnSync("/bin/bash", ["-lc", command], {
        cwd,
        encoding: "utf8",
        timeout: bashTimeoutMs,
        maxBuffer: maxOut,
        env: process.env,
      });
      const out = `${r.stdout || ""}${r.stderr || ""}`.slice(0, maxOut);
      const status = r.status ?? (r.error ? 1 : 0);
      return textResult(status === 0 ? out || "(no output)" : `exit ${status}\n${out || r.error?.message || ""}`);
    },
  } as AgentTool;
}

function readTool(cwdOf: () => string): AgentTool {
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
        const abs = under(cwdOf(), p);
        const body = fs.readFileSync(abs, "utf8");
        return textResult(body.length > maxOut ? body.slice(0, maxOut) + "\n…truncated" : body);
      } catch (err) {
        return textResult(String(err));
      }
    },
  } as AgentTool;
}

function writeTool(cwdOf: () => string): AgentTool {
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
        const abs = under(cwdOf(), String(p?.path || ""));
        fs.mkdirSync(path.dirname(abs), { recursive: true });
        fs.writeFileSync(abs, String(p?.content ?? ""), "utf8");
        return textResult("wrote " + abs);
      } catch (err) {
        return textResult(String(err));
      }
    },
  } as AgentTool;
}

function globTool(cwdOf: () => string): AgentTool {
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
      const r = spawnSync("/usr/bin/find", [cwd, "-name", pattern], {
        encoding: "utf8",
        timeout: 15_000,
        maxBuffer: maxOut,
      });
      const lines = (r.stdout || "").split("\n").filter(Boolean).slice(0, 500);
      return textResult(lines.join("\n") || "(no matches)");
    },
  } as AgentTool;
}

function grepTool(cwdOf: () => string): AgentTool {
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
      const target = p?.path ? under(cwd, p.path) : cwd;
      const rg = spawnSync("rg", ["-n", "--max-count", "50", String(p?.pattern || ""), target], {
        encoding: "utf8",
        timeout: 15_000,
        maxBuffer: maxOut,
      });
      if (rg.error && (rg.error as NodeJS.ErrnoException).code === "ENOENT") {
        const g = spawnSync("/usr/bin/grep", ["-R", "-n", String(p?.pattern || ""), target], {
          encoding: "utf8",
          timeout: 15_000,
          maxBuffer: maxOut,
        });
        return textResult((g.stdout || g.stderr || "").slice(0, maxOut) || "(no matches)");
      }
      return textResult((rg.stdout || rg.stderr || "").slice(0, maxOut) || "(no matches)");
    },
  } as AgentTool;
}

function under(cwd: string, rel: string): string {
  const root = path.resolve(cwd || process.cwd());
  const abs = path.resolve(root, rel);
  const prefix = root.endsWith(path.sep) ? root : root + path.sep;
  if (abs !== root && !abs.startsWith(prefix)) {
    throw new Error(`path escapes working directory: ${rel}`);
  }
  return abs;
}

function textResult(text: string) {
  return { content: [{ type: "text" as const, text }], details: {} };
}
