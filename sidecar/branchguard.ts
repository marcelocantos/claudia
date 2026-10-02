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
import { runCommand } from "./coding.ts";

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
  let found: BranchCheckout | null = null;
  let dir = "";
  for (const words of segments(stripHeredocBodies(command))) {
    if (words.length === 0) continue;
    if (words[0] === "cd") {
      // `cd` moves every later segment; `cd` alone or `cd -` is unknowable,
      // so later segments are judged where the shell started.
      dir = words.length > 1 && words[1] !== "-" ? words[1] : "";
      continue;
    }
    const cmd = stripPrefixes(words);
    if (cmd.length < 2 || path.basename(cmd[0]) !== "git") continue;
    let sub = "";
    const rest: string[] = [];
    let createFlag = false;
    let segDir = dir;
    for (let i = 1; i < cmd.length; i++) {
      const a = cmd[i];
      if (sub === "") {
        if (a === "-C" && i + 1 < cmd.length) {
          segDir = dir ? path.join(dir, cmd[++i]) : cmd[++i];
          continue;
        }
        if (a.startsWith("-")) continue;
        sub = a;
        continue;
      }
      if (a === "-b" || a === "-B" || a === "-c" || a === "-C") createFlag = true;
      rest.push(a);
    }
    if (sub !== "checkout" && sub !== "switch") continue;
    if (rest.includes("--")) continue; // `checkout [<ref>] -- <path>`: a restore
    const pos = rest.filter((a) => !a.startsWith("-"));
    if (pos.length === 0) continue;
    found = { ref: pos[0], dir: segDir, definite: sub === "switch" || createFlag };
  }
  return found;
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
export type GitRunner = (dir: string, args: string[]) => Promise<string | null>;

export const runGit: GitRunner = async (dir, args) => {
  const r = await runCommand("git", ["-C", dir, ...args], { timeoutMs: gitTimeoutMs, maxBuffer: gitMaxBuffer });
  if (r.error || r.status !== 0) return null;
  return r.stdout.trim();
};

// guardBranchCheckout is the pre-exec check the Bash tool runs: null when the
// guard has nothing to say (no switch in the command, not a repository, a
// linked worktree the seat owns, detached HEAD, already on the ref, or a
// bare `git checkout <arg>` whose <arg> is not a ref), otherwise the
// decision. The command has not run when this returns.
export async function guardBranchCheckout(cwd: string, command: string, git: GitRunner = runGit): Promise<BranchDecision | null> {
  const hit = detectBranchCheckout(command);
  if (!hit) return null;
  const dir = path.resolve(cwd, hit.dir || ".");
  const gitDir = await git(dir, ["rev-parse", "--path-format=absolute", "--git-dir"]);
  const commonDir = await git(dir, ["rev-parse", "--path-format=absolute", "--git-common-dir"]);
  if (gitDir === null || commonDir === null) return null; // not a repository
  if (gitDir !== commonDir) return null; // a linked worktree: the seat's own
  const branch = await git(dir, ["symbolic-ref", "--quiet", "--short", "HEAD"]);
  if (branch === null || branch === "") return null; // detached HEAD: no branch to protect
  if (hit.ref === branch) return null;
  if (!hit.definite && (await git(dir, ["rev-parse", "--verify", "--quiet", `${hit.ref}^{commit}`])) === null) {
    return null; // `git checkout <path>`: a restore, not a switch
  }
  const repo = (await git(dir, ["rev-parse", "--show-toplevel"])) || dir;
  return decideBranchCheckout({ currentBranch: branch, targetRef: hit.ref, repo });
}

// --- lexing ----------------------------------------------------------------
//
// Not a shell: just enough to keep a quoted `;` from splitting a command, to
// drop redirections, and to split on `;`, `&&`, `||`, `|`, `&` and newlines.

const commandPrefixes = new Set(["env", "sudo", "nice", "nohup", "time", "command", "exec", "builtin"]);

function stripPrefixes(words: string[]): string[] {
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
function stripHeredocBodies(command: string): string {
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
function segments(command: string): string[][] {
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
