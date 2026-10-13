// Host coding tools for OMP seats. These are ours, not pi-natives (🎯T864.3).
// Names follow the Claude CLI set so workers can actually edit a repo.

import { spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { AgentTool } from "@oh-my-pi/pi-agent-core";
import { guardBranchCheckout } from "./branchguard.ts";
import { capToolText, toolResultBound } from "./truncate.ts";

const maxOut = 200_000;
const bashTimeoutMs = 60_000;
const searchTimeoutMs = 15_000;

// RunResult is what runCommand saw, shaped like a synchronous spawn's result
// so the tools read it the way they always did.
export type RunResult = {
  stdout: string;
  stderr: string;
  status: number | null;
  error?: NodeJS.ErrnoException;
};

// runCommand runs a child without blocking the event loop (🎯T161).
//
// The tools ran their children synchronously, and the sidecar is one Bun
// process serving every seat: one seat's 60 s build froze every other seat's
// prompt, load and adopt until it returned. Across a broker restart that was
// minutes of nothing — the spool went silent for 60-124 s at a time while
// loads queued behind Bash, and resumes that waited 2 s for an adopt reply
// gave up and relaunched. The bounds are unchanged: past timeoutMs, or past
// maxBuffer bytes on either stream, the child is killed and the result
// carries an error.
export function runCommand(
  command: string,
  args: string[],
  opts: { cwd?: string; timeoutMs: number; maxBuffer: number; env?: NodeJS.ProcessEnv },
): Promise<RunResult> {
  return new Promise((resolve) => {
    const out: Buffer[] = [];
    const errOut: Buffer[] = [];
    let outBytes = 0;
    let errBytes = 0;
    let error: NodeJS.ErrnoException | undefined;
    let settled = false;
    const finish = (status: number | null) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      resolve({
        stdout: Buffer.concat(out).toString("utf8"),
        stderr: Buffer.concat(errOut).toString("utf8"),
        status,
        error,
      });
    };
    const fail = (code: string, message: string) => {
      if (!error) {
        error = Object.assign(new Error(message), { code }) as NodeJS.ErrnoException;
      }
      child.kill("SIGTERM");
      // A grandchild holding the pipes open must not hold the seat.
      child.stdout?.destroy();
      child.stderr?.destroy();
      finish(null);
    };
    const child = spawn(command, args, { cwd: opts.cwd, env: opts.env, stdio: ["ignore", "pipe", "pipe"] });
    const timer = setTimeout(() => fail("ETIMEDOUT", `${command} timed out after ${opts.timeoutMs}ms`), opts.timeoutMs);
    const collect = (into: Buffer[], add: (n: number) => number) => (chunk: Buffer) => {
      if (settled) return;
      if (add(chunk.length) > opts.maxBuffer) {
        fail("ENOBUFS", `${command} output exceeded ${opts.maxBuffer} bytes`);
        return;
      }
      into.push(chunk);
    };
    child.stdout?.on("data", collect(out, (n) => (outBytes += n)));
    child.stderr?.on("data", collect(errOut, (n) => (errBytes += n)));
    child.on("error", (err: NodeJS.ErrnoException) => {
      error = err;
      finish(null);
    });
    child.on("close", (code) => finish(code));
  });
}

// SandboxPolicy is what the host asked this seat's own tools to be allowed
// (🎯T178: Codex sandbox settings mapped onto the sidecar). Absent means the
// tools run as they always have. read-only offers no Write and no Bash.
// workspace-write confines Write, and Bash's file writes (macOS seatbelt), to
// the seat's working directory plus writable_roots, and keeps everything under
// protected (a repo's git dirs, unless the host granted them) read-only.
export type SandboxPolicy = {
  mode: "read-only" | "workspace-write";
  writable_roots?: string[];
  protected?: string[];
};

export function codingTools(
  cwdOf: () => string,
  boundOf: () => number = () => toolResultBound(0),
  policyOf: () => SandboxPolicy | undefined = () => undefined,
): AgentTool[] {
  const policy = policyOf();
  if (policy?.mode === "read-only") {
    return [readTool(cwdOf, boundOf), globTool(cwdOf, boundOf), grepTool(cwdOf, boundOf)];
  }
  return [
    bashTool(cwdOf, boundOf, policyOf),
    readTool(cwdOf, boundOf),
    writeTool(cwdOf, boundOf, policyOf),
    globTool(cwdOf, boundOf),
    grepTool(cwdOf, boundOf),
  ];
}

// realish resolves symlinks through the nearest existing ancestor, so a path
// that does not exist yet is judged by where it would really land.
export function realish(p: string): string {
  let cur = path.resolve(p);
  const tail: string[] = [];
  for (;;) {
    try {
      return path.join(fs.realpathSync(cur), ...tail.reverse());
    } catch {
      const parent = path.dirname(cur);
      if (parent === cur) return path.resolve(p);
      tail.push(path.basename(cur));
      cur = parent;
    }
  }
}

function within(root: string, abs: string): boolean {
  const r = realish(root);
  const prefix = r.endsWith(path.sep) ? r : r + path.sep;
  return abs === r || abs.startsWith(prefix);
}

// writableRoots is where a workspace-write seat may write: its working
// directory and the host's extra roots.
export function writableRoots(cwd: string, policy: SandboxPolicy): string[] {
  return [cwd || process.cwd(), ...(policy.writable_roots ?? [])].filter(Boolean);
}

// checkWrite refuses a write a workspace-write policy does not allow.
export function checkWrite(cwd: string, abs: string, policy: SandboxPolicy | undefined): void {
  if (!policy) return;
  if (policy.mode === "read-only") throw new Error(`read-only seat: write refused: ${abs}`);
  const real = realish(abs);
  for (const p of policy.protected ?? []) {
    if (within(p, real)) throw new Error(`write refused: ${abs} is under protected ${p} (git write not granted)`);
  }
  if (!writableRoots(cwd, policy).some((r) => within(r, real))) {
    throw new Error(`write refused: ${abs} is outside the seat's writable roots`);
  }
}

function sbplString(s: string): string {
  return JSON.stringify(s);
}

// seatbeltProfile confines file writes to the policy's roots plus temp dirs.
// Later rules win, so protected dirs are denied after the roots are allowed.
export function seatbeltProfile(cwd: string, policy: SandboxPolicy): string {
  const allow = [...writableRoots(cwd, policy), "/private/tmp", "/private/var/folders", process.env.TMPDIR ?? ""]
    .filter(Boolean)
    .map((r) => `(subpath ${sbplString(realish(r))})`);
  const lines = [
    "(version 1)",
    "(allow default)",
    "(deny file-write*)",
    `(allow file-write* ${allow.join(" ")} (regex #"^/dev/"))`,
  ];
  const prot = (policy.protected ?? []).map((p) => `(subpath ${sbplString(realish(p))})`);
  if (prot.length > 0) lines.push(`(deny file-write* ${prot.join(" ")})`);
  return lines.join("\n");
}

function bashTool(cwdOf: () => string, boundOf: () => number, policyOf: () => SandboxPolicy | undefined): AgentTool {
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
      if (!command) return textResult("missing command", boundOf(), "Bash");
      const cwd = cwdOf() || process.cwd();
      // 🎯T164: a switch of the shared clone's branch is refused before the
      // shell starts. The refusal is a failed tool call, not an exit status:
      // the command never ran, and the next command is judged on its own.
      const refusal = await guardBranchCheckout(cwd, command);
      if (refusal && refusal.verdict === "deny") {
        return { ...textResult(refusal.message, boundOf(), "Bash"), isError: true };
      }
      const policy = policyOf();
      const [bin, args] =
        policy?.mode === "workspace-write"
          ? ["/usr/bin/sandbox-exec", ["-p", seatbeltProfile(cwd, policy), "/bin/bash", "-lc", command]]
          : ["/bin/bash", ["-lc", command]];
      const r = await runCommand(bin, args, {
        cwd,
        timeoutMs: bashTimeoutMs,
        maxBuffer: maxOut,
        env: process.env,
      });
      const out = `${r.stdout || ""}${r.stderr || ""}`.slice(0, maxOut);
      const status = r.status ?? (r.error ? 1 : 0);
      return textResult(status === 0 ? out || "(no output)" : `exit ${status}\n${out || r.error?.message || ""}`, boundOf(), "Bash");
    },
  } as AgentTool;
}

function readTool(cwdOf: () => string, boundOf: () => number): AgentTool {
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
        return textResult(body, boundOf(), "Read");
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
        const abs = under(cwdOf(), String(p?.path || ""));
        checkWrite(cwdOf(), abs, policyOf());
        fs.mkdirSync(path.dirname(abs), { recursive: true });
        fs.writeFileSync(abs, String(p?.content ?? ""), "utf8");
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

function grepTool(cwdOf: () => string, boundOf: () => number): AgentTool {
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
      const rg = await runCommand("rg", ["-n", "--max-count", "50", String(p?.pattern || ""), target], {
        timeoutMs: searchTimeoutMs,
        maxBuffer: maxOut,
      });
      if (rg.error && rg.error.code === "ENOENT") {
        const g = await runCommand("/usr/bin/grep", ["-R", "-n", String(p?.pattern || ""), target], {
          timeoutMs: searchTimeoutMs,
          maxBuffer: maxOut,
        });
        return textResult((g.stdout || g.stderr || "").slice(0, maxOut) || "(no matches)", boundOf(), "Grep");
      }
      return textResult((rg.stdout || rg.stderr || "").slice(0, maxOut) || "(no matches)", boundOf(), "Grep");
    },
  } as AgentTool;
}

export function under(cwd: string, rel: string): string {
  const root = path.resolve(cwd || process.cwd());
  const abs = path.resolve(root, rel);
  const prefix = root.endsWith(path.sep) ? root : root + path.sep;
  if (abs !== root && !abs.startsWith(prefix)) {
    throw new Error(`path escapes working directory: ${rel}`);
  }
  return abs;
}

function textResult(text: string, bound: number, tool: string) {
  return { content: [{ type: "text" as const, text: capToolText(text, bound, tool) }], details: {} };
}
