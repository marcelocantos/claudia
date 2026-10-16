// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Seat execution authority shared by ordinary tools and provider-native frames.
// Adapters supply arguments and render results; they do not authorize operations.
import fs from "node:fs";
import path from "node:path";
import { guardBranchCheckout } from "./branchguard.ts";
import { runCommand, type RunResult } from "./process.ts";
export { runCommand, type RunResult } from "./process.ts";

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

// Resolve existing and dangling symlinks through the nearest existing
// ancestor. realpath alone treats a dangling link like an absent ordinary
// path, even though a later write would follow it outside the seat.
export function realish(p: string): string {
  let cur = path.resolve(p);
  const tail: string[] = [];
  const maxSymlinkRedirects = 40;
  let redirects = 0;
  for (;;) {
    try {
      if (fs.lstatSync(cur).isSymbolicLink()) {
        if (++redirects > maxSymlinkRedirects) throw new Error(`too many symbolic links: ${p}`);
        cur = path.resolve(path.dirname(cur), fs.readlinkSync(cur));
        continue;
      }
      return path.join(fs.realpathSync(cur), ...tail.reverse());
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err;
      const parent = path.dirname(cur);
      if (parent === cur) throw err;
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

// Unlink changes the directory entry, not the final symlink's destination.
// Ancestors still resolve normally so a symlinked parent cannot escape.
type PathAccess = "follow" | "entry";
function operationPath(p: string, access: PathAccess): string {
  const abs = path.resolve(p);
  return access === "entry"
    ? path.join(realish(path.dirname(abs)), path.basename(abs))
    : realish(abs);
}

// writableRoots is where a workspace-write seat may write: its working
// directory and the host's extra roots.
export function writableRoots(cwd: string, policy: SandboxPolicy): string[] {
  return [cwd || process.cwd(), ...(policy.writable_roots ?? [])].filter(Boolean);
}

// checkWrite refuses a write a workspace-write policy does not allow.
export function checkWrite(cwd: string, abs: string, policy: SandboxPolicy | undefined, access: PathAccess = "follow"): void {
  if (!policy) return;
  if (policy.mode === "read-only") throw new Error(`read-only seat: write refused: ${abs}`);
  const real = operationPath(abs, access);
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

// Resolve the actual path, including existing symlink ancestors. Explicit
// grants are usable through file tools as well as through the shell. A grant
// never overrides a protected root or read-only mode for mutations.
export function under(cwd: string, rel: string, policy?: SandboxPolicy, access: PathAccess = "follow"): string {
  const root = cwd || process.cwd();
  const abs = operationPath(path.resolve(root, rel), access);
  const roots = [root, ...(policy?.writable_roots ?? [])];
  if (!roots.some((r) => within(r, abs))) {
    throw new Error(`path escapes working directory (outside seat roots): ${rel}`);
  }
  return abs;
}

export function writeFile(cwd: string, name: string, content: string, policy?: SandboxPolicy): string {
  const abs = under(cwd, name, policy);
  checkWrite(cwd, abs, policy);
  fs.mkdirSync(path.dirname(abs), { recursive: true });
  fs.writeFileSync(abs, content, "utf8");
  return abs;
}

export function deleteFile(cwd: string, name: string, policy?: SandboxPolicy): string {
  const entry = under(cwd, name, policy, "entry");
  checkWrite(cwd, entry, policy, "entry");
  if (entry === realish(cwd || process.cwd())) throw new Error("refusing to delete the workdir");
  fs.rmSync(entry);
  return entry;
}

export function editFile(cwd: string, name: string, edits: { oldText: string; newText: string }[], policy?: SandboxPolicy): string {
  const abs = under(cwd, name, policy);
  checkWrite(cwd, abs, policy);
  let body = fs.readFileSync(abs, "utf8");
  for (const edit of edits) {
    const at = body.indexOf(edit.oldText);
    if (at < 0) throw new Error(`old text not found in ${name}`);
    body = body.slice(0, at) + edit.newText + body.slice(at + edit.oldText.length);
  }
  fs.writeFileSync(abs, body, "utf8");
  return abs;
}

export async function runShell(
  cwdOf: () => string,
  policyOf: () => SandboxPolicy | undefined,
  args: { command: string; workingDirectory?: string; timeoutMs: number; maxBuffer: number },
): Promise<RunResult> {
  const root = cwdOf() || process.cwd();
  const cwd = under(root, args.workingDirectory || ".", policyOf());
  if (policyOf()?.mode === "read-only") throw new Error("read-only seat: shell refused");
  const refusal = await guardBranchCheckout(cwd, args.command);
  if (refusal?.verdict === "deny") throw new Error(refusal.message);
  // Branch inspection yields to the event loop. Recheck mutable authority
  // immediately before launch so a queued old handle cannot retain its grant.
  const policy = policyOf();
  if (policy?.mode === "read-only") throw new Error("read-only seat: shell refused");
  if ((cwdOf() || process.cwd()) !== root) throw new Error("seat working directory changed before shell execution; retry");
  under(root, cwd, policy);
  const [bin, argv] = policy?.mode === "workspace-write"
    ? ["/usr/bin/sandbox-exec", ["-p", seatbeltProfile(root, policy), "/bin/bash", "-lc", args.command]]
    : ["/bin/bash", ["-lc", args.command]];
  return runCommand(bin, argv, { cwd, timeoutMs: args.timeoutMs, maxBuffer: args.maxBuffer, env: process.env });
}

