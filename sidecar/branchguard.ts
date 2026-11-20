// Shared-clone branch guard for the sidecar's Bash tool (🎯T164).
//
// A provider=claude seat runs Claude Code, and jevons 🎯T955's PreToolUse
// hook refuses a `git checkout`/`git switch` that would move the shared
// clone's checked-out branch. A provider=anthropic seat runs here instead,
// never reads .claude/settings.json, and had no equivalent: the 2026-09-30
// incident (jv-t946-resume2 ran `git checkout master` in the shared clone
// while the live integration branch was steer-modes-stop-guards-seat-stops,
// then fast-forwarded the wrong branch) happened on exactly this path.
//
// This module is that hook's logic, harness-agnostic: detectBranchCheckout
// reads the command, decideBranchCheckout is the pure verdict, and
// guardBranchCheckout asks git which repository the command addresses and
// whether it is a shared clone (its own git dir is the common dir) or a
// linked worktree a seat owns. Only the shared clone's branch is protected:
// a seat switching branches inside its own worktree is its own business.

import path from "node:path";
import { runCommand } from "./process.ts";

export type BranchCheckout = {
  // ref is the branch or ref the command would move HEAD to.
  ref: string;
  // dir is the directory the git command addresses: the `-C` argument or
  // the most recent `cd` in an earlier segment, resolved against cwd.
  // Empty when the command runs where the shell starts.
  dir: string;
  // definite is true when the command's own shape is unambiguously a
  // switch (`git switch`, `-b`/`-B`/`-c`/`-C`). A bare `git checkout <arg>`
  // is not: <arg> may be a path, and the caller confirms it is a ref first.
  definite: boolean;
};

export type BranchDecision = {
  verdict: "allow" | "deny";
  // reason is a stable slug for tests and logs.
  reason: string;
  // message is the agent-facing refusal, non-empty on deny.
  message: string;
};

const gitTimeoutMs = 5_000;
const gitMaxBuffer = 64 * 1024;

// detectBranchCheckout reports the ref a `git checkout` / `git switch` in
// command would move HEAD to, or null when no segment of the command does
// that. Path restores (`git checkout -- file`, `git checkout <ref> -- file`)
// and other subcommands are not switches. The last switching segment wins,
// as in jevons treeguard.DetectBranchCheckout.
export function detectBranchCheckout(command: string): BranchCheckout | null {
  return findBranchCheckout(command)?.checkout ?? null;
}

function findBranchCheckout(command: string): { checkout: BranchCheckout; probe: GitProbe } | null {
  let found: { checkout: BranchCheckout; probe: GitProbe } | null = null;
  for (const { sub, args, dir, probe } of gitCommands(command)) {
    const ref = branchOperand(sub, args);
    if (!ref) continue;
    found = { checkout: { ref: ref.ref, dir, definite: ref.definite }, probe };
  }
  return found;
}

// branchOperand is the ref a checkout or switch would move HEAD to.
// Subcommand options follow parse-options: a short option that takes an
// operand consumes the rest of a combined cluster (`-qbother`, `-cnew`),
// and `--name=value` does not leave `value` looking like the branch.
// `--` starts pathspecs, so `checkout <ref> -- <path>` is not a switch,
// except when a create option already named the branch (`checkout -b other --`
// still moves HEAD).
function branchOperand(sub: string, args: string[]): { ref: string; definite: boolean } | null {
  if (sub !== "checkout" && sub !== "switch") return null;
  const createShort = sub === "switch" ? new Set(["c", "C"]) : new Set(["b", "B"]);
  // Flags with no operand. `-t` / `--track` takes an optional attached value
  // only (`-tbother` is `--track=bother`, not `-t -b other`).
  const flagShort = new Set(["q", "l", "m", "d", "f", "p", "2", "3"]);
  const requiredShort = new Set([...createShort, "U"]);
  const createLong = new Set(sub === "switch"
    ? ["--create", "--force-create", "--orphan"]
    : ["--orphan"]);
  const valueLong = new Set(["--conflict", "--unified", "--inter-hunk-context", "--pathspec-from-file"]);
  let createRef: string | null = null;
  const positionals: string[] = [];
  let paths = false;
  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    if (paths) {
      positionals.push(arg);
      continue;
    }
    if (arg === "--") {
      paths = true;
      continue;
    }
    if (arg.startsWith("--")) {
      const eq = arg.indexOf("=");
      const name = eq < 0 ? arg : arg.slice(0, eq);
      const inline = eq < 0 ? undefined : arg.slice(eq + 1);
      if (name.startsWith("--no-")) continue;
      if (createLong.has(name)) {
        const value = inline ?? args[++i];
        if (value === undefined) return null;
        createRef = value;
        continue;
      }
      if (valueLong.has(name)) {
        if (inline === undefined) i++;
        continue;
      }
      continue;
    }
    if (arg.startsWith("-") && arg !== "-") {
      let rest = arg.slice(1);
      while (rest) {
        const letter = rest[0];
        rest = rest.slice(1);
        if (flagShort.has(letter)) continue;
        if (letter === "t") break; // optional operand consumes the rest of the cluster
        if (requiredShort.has(letter)) {
          const value = rest || args[++i];
          if (!value) return null;
          if (createShort.has(letter)) createRef = value;
          break;
        }
        return null; // unknown short: this git rejects the command
      }
      continue;
    }
    positionals.push(arg);
  }
  if (createRef !== null) return { ref: createRef, definite: true };
  if (paths || positionals.length === 0) return null;
  return { ref: positionals[0], definite: sub === "switch" };
}

// decideBranchCheckout is the pure verdict: a switch away from the shared
// clone's checked-out branch is refused, naming both branches.
export function decideBranchCheckout(args: { currentBranch: string; targetRef: string; repo: string }): BranchDecision {
  if (args.targetRef === args.currentBranch) {
    return { verdict: "allow", reason: "same-branch", message: "" };
  }
  return {
    verdict: "deny",
    reason: "branch-switch-in-shared-clone",
    message:
      `branchguard: refusing to check out ${args.targetRef} in the shared clone ${args.repo}` +
      ` — its checked-out branch is the integration branch ${args.currentBranch}, which other seats` +
      ` are working in, and a seat must never switch it. Work in your own worktree` +
      ` (git worktree add) and land through the integrator instead; this command was not run.` +
      ` 🎯T164 (mirrors jevons 🎯T955)`,
  };
}

// GitRunner runs git in dir and resolves stdout, or null on a non-zero exit
// or a failure to run. Tests substitute it; the default spawns git.
export type GitRunner = (dir: string, args: string[], globals?: readonly string[]) => Promise<string | null>;

export const runGit: GitRunner = async (dir, args, globals = []) => {
  const r = await runCommand("git", ["-C", dir, ...globals, ...args], { timeoutMs: gitTimeoutMs, maxBuffer: gitMaxBuffer });
  if (r.error || r.status !== 0) return null;
  return args[0] === "status" ? r.stdout : r.stdout.trim();
};

// guardBranchCheckout is the pre-exec check the Bash tool runs: null when the
// guard has nothing to say (no switch in the command, not a repository, a
// linked worktree the seat owns, detached HEAD, already on the ref, or a
// bare `git checkout <arg>` whose <arg> is not a ref), otherwise the
// decision. The command has not run when this returns.
export async function guardBranchCheckout(cwd: string, command: string, git: GitRunner = runGit): Promise<BranchDecision | null> {
  const hit = findBranchCheckout(command);
  if (!hit) return null;
  const { checkout, probe } = hit;
  const dir = path.resolve(cwd, probe.dir);
  const query = (args: string[]) => git(dir, args, probe.globals);
  const gitDir = await query(["rev-parse", "--path-format=absolute", "--git-dir"]);
  const commonDir = await query(["rev-parse", "--path-format=absolute", "--git-common-dir"]);
  if (gitDir === null || commonDir === null) return null; // not a repository
  if (gitDir !== commonDir) return null; // a linked worktree: the seat's own
  const branch = await query(["symbolic-ref", "--quiet", "--short", "HEAD"]);
  if (branch === null || branch === "") return null; // detached HEAD: no branch to protect
  if (checkout.ref === branch) return null;
  if (!checkout.definite && (await query(["rev-parse", "--verify", "--quiet", `${checkout.ref}^{commit}`])) === null) {
    return null; // `git checkout <path>`: a restore, not a switch
  }
  const repo = (await query(["rev-parse", "--show-toplevel"])) || path.resolve(cwd, checkout.dir);
  return decideBranchCheckout({ currentBranch: branch, targetRef: checkout.ref, repo });
}

export type GitProbe = { dir: string; globals: string[] };

// Git's top-level parser has separate operands for these options. The long
// options also accept `=value`, except for `--shallow-file`. Keep the retired
// `--super-prefix` spellings for older Git.
const gitValueOptions = new Set([
  "-C", "-c", "--git-dir", "--work-tree", "--namespace", "--config-env",
  "--attr-source", "--shallow-file", "--super-prefix",
]);
const gitPathspecOptions = [
  "--literal-pathspecs", "--no-literal-pathspecs", "--glob-pathspecs", "--noglob-pathspecs", "--icase-pathspecs",
];
const gitFlags = new Set([
  "-p", "--paginate", "-P", "--no-pager", "--bare", "--no-replace-objects",
  "--no-lazy-fetch", "--no-optional-locks", "--no-advice", ...gitPathspecOptions,
]);
const gitProbeOptions = new Set([
  "-C", "--git-dir", "--work-tree", "--bare", "--namespace", "--attr-source",
  "--no-replace-objects", "--no-lazy-fetch", ...gitPathspecOptions,
]);

function gitCommand(words: string[], cwd: string) {
  const cmd = stripPrefixes(words);
  if (path.basename(cmd[0] || "") !== "git") return null;
  const globals: string[] = [];
  let dir = cwd;
  for (let i = 1; i < cmd.length; i++) {
    const arg = cmd[i];
    if (!arg.startsWith("-")) return { sub: arg, args: cmd.slice(i + 1), dir, probe: { dir: cwd, globals } };
    const equals = arg.indexOf("=");
    const option = equals < 0 ? arg : arg.slice(0, equals);
    if (gitValueOptions.has(option)) {
      if (equals < 0) {
        const value = cmd[++i];
        if (value === undefined) return null;
        if (option === "-C" && value !== "") dir = path.isAbsolute(value) ? value : path.join(dir, value);
        if (gitProbeOptions.has(option)) globals.push(option, value);
      } else {
        if (!option.startsWith("--") || option === "--shallow-file") return null;
        if (gitProbeOptions.has(option)) globals.push(arg);
      }
    } else if (gitFlags.has(arg)) {
      if (gitProbeOptions.has(arg)) globals.push(arg);
    } else if (!arg.startsWith("--exec-path=")) {
      // Git rejects a top-level `--` and attached/combined short options.
      // Options such as `--help` and bare `--exec-path` finish without a command.
      return null;
    }
  }
  return null;
}

// Both guards use the same Git invocation. The probe lets Git apply each
// `-C` in sequence and resolve repository selectors from the resulting cwd.
// Command-provided config and exec paths are excluded from the probe.
export function* gitCommands(command: string) {
  let cwd = "";
  for (const words of segments(stripHeredocBodies(command))) {
    if (words[0] === "cd") {
      cwd = words[1] && words[1] !== "-"
        ? (path.isAbsolute(words[1]) ? words[1] : path.join(cwd, words[1])) : "";
      continue;
    }
    const parsed = gitCommand(words, cwd);
    if (parsed) yield parsed;
  }
}

// --- lexing ----------------------------------------------------------------
//
// Not a shell: just enough to keep a quoted `;` from splitting a command, to
// drop redirections, and to split on `;`, `&&`, `||`, `|`, `&` and newlines.

export const commandPrefixes = new Set(["env", "sudo", "nice", "nohup", "time", "command", "exec", "builtin"]);

export function stripPrefixes(words: string[]): string[] {
  let i = 0;
  while (i < words.length) {
    const w = words[i];
    const eq = w.indexOf("=");
    if (eq > 0 && !w.startsWith("-") && !/[/. ]/.test(w.slice(0, eq))) {
      i++; // FOO=bar cmd …
      continue;
    }
    if (commandPrefixes.has(path.basename(w))) {
      i++;
      continue;
    }
    break;
  }
  return words.slice(i);
}

const heredocStart = /<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?/;

// stripHeredocBodies drops heredoc bodies, which are data that would
// otherwise lex as commands.
export function stripHeredocBodies(command: string): string {
  const lines = command.split("\n");
  const code: string[] = [];
  for (let i = 0; i < lines.length; i++) {
    code.push(lines[i]);
    const m = heredocStart.exec(lines[i]);
    if (!m) continue;
    for (i++; i < lines.length && lines[i].trim() !== m[1]; i++) {
      // body
    }
  }
  return code.join("\n");
}

// segments splits command into simple commands, each a list of words with
// redirection targets removed.
export function segments(command: string): string[][] {
  const out: string[][] = [];
  let seg: string[] = [];
  let cur = "";
  let started = false;
  let redirNext = false;
  const flush = () => {
    if (!started) return;
    if (!redirNext) seg.push(cur);
    redirNext = false;
    cur = "";
    started = false;
  };
  const endSeg = () => {
    flush();
    if (seg.length > 0) out.push(seg);
    seg = [];
    redirNext = false;
  };
  for (let i = 0; i < command.length; i++) {
    const c = command[i];
    if (c === "'" || c === '"') {
      started = true;
      for (i++; i < command.length && command[i] !== c; i++) {
        if (command[i] === "\\" && c === '"' && i + 1 < command.length) i++;
        cur += command[i];
      }
      continue;
    }
    if (c === "\\") {
      if (i + 1 < command.length) {
        i++;
        started = true;
        cur += command[i];
      }
      continue;
    }
    if (c === " " || c === "\t") {
      flush();
      continue;
    }
    if (c === "&" && command[i + 1] === ">") {
      // `&>file` / `&>>file`
      flush();
      i++;
      if (command[i + 1] === ">") i++;
      redirNext = true;
      continue;
    }
    if (c === ";" || c === "\n" || c === "&" || c === "|") {
      if ((c === "&" || c === "|") && command[i + 1] === c) i++;
      endSeg();
      continue;
    }
    if (c === ">" || c === "<") {
      // A trailing digit on the current word is the descriptor: `2>err`.
      if (started && /^\d+$/.test(cur)) {
        cur = "";
        started = false;
      } else {
        flush();
      }
      if (command[i + 1] === ">" || command[i + 1] === "|" || command[i + 1] === "<") i++;
      if (command[i + 1] === "&") {
        // `2>&1`: a descriptor dup, not a file.
        i++;
        while (i + 1 < command.length && /\d/.test(command[i + 1])) i++;
        continue;
      }
      redirNext = true;
      continue;
    }
    started = true;
    cur += c;
  }
  endSeg();
  return out;
}
