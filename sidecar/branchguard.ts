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

// Inside an alias, git accepts only the global options that leave the
// environment alone; any other one ends the command with "alias changes
// environment variables", so nothing runs.
const gitAliasOptions = new Set(["-c", "--config-env", "-p", "--paginate", "--exec-path"]);

// git ignores an alias that would hide one of its own commands. These are
// the commands the guards read, so an alias of the same name never stands in
// for them. Other shadowing aliases are expanded, which can only refuse a
// command git would not have run.
const gitGuardedCommands = new Set(["stash", "reset", "checkout", "switch", "restore", "checkout-index", "clean"]);

// git reports an alias loop and runs nothing; this bounds a chain that never
// repeats a name but keeps growing.
const gitAliasExpansionLimit = 64;

type GitInvocation = { sub: string; args: string[]; dir: string; probe: GitProbe };

// gitCommand reads one simple command as git does: global options, then the
// subcommand, with aliases defined on the command line by `-c alias.<name>=`
// or `--config-env alias.<name>=<VAR>` expanded. A `!` alias is a shell
// command, so its text is read as one. inherited carries the repository
// selectors an outer git exports to a shell alias.
function* gitCommand(words: string[], cwd: string, inherited: readonly string[] = []): Generator<GitInvocation> {
  const utility = commandUtility(words);
  if (!utility || path.basename(utility.argv[0] || "") !== "git") return;
  const env = (name: string) => utility.vars.get(name) ?? (utility.clearedEnv ? undefined : process.env[name]);
  const probeDir = utility.chdir ? joinDir(cwd, utility.chdir) : cwd;
  const globals: string[] = [...inherited];
  const aliases = new Map<string, string>();
  const expanded = new Set<string>();
  let argv = utility.argv.slice(1);
  let dir = probeDir;
  let inAlias = false;
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (!arg.startsWith("-")) {
      const name = arg.toLowerCase();
      const alias = gitGuardedCommands.has(arg) ? undefined : aliases.get(name);
      if (alias === undefined) {
        yield { sub: arg, args: argv.slice(i + 1), dir, probe: { dir: probeDir, globals } };
        return;
      }
      if (expanded.has(name) || expanded.size >= gitAliasExpansionLimit) return; // git: alias loop
      expanded.add(name);
      const rest = argv.slice(i + 1);
      if (alias.startsWith("!")) {
        // git runs the text with sh at the repository's top level, the
        // remaining words as its arguments and the outer selectors exported.
        const selectors = globals.filter((g, k) => g !== "-C" && globals[k - 1] !== "-C");
        yield* gitCommands(`${alias.slice(1)} ${rest.map(shellQuote).join(" ")}`, dir, selectors);
        return;
      }
      argv = [...splitAliasWords(alias), ...rest];
      i = -1;
      inAlias = true;
      continue;
    }
    const equals = arg.indexOf("=");
    const option = equals < 0 ? arg : arg.slice(0, equals);
    if (inAlias && !gitAliasOptions.has(option)) return;
    if (gitValueOptions.has(option)) {
      let value: string;
      if (equals < 0) {
        const next = argv[++i];
        if (next === undefined) return;
        value = next;
        if (option === "-C" && value !== "") dir = joinDir(dir, value);
        if (gitProbeOptions.has(option)) globals.push(option, value);
      } else {
        if (!option.startsWith("--") || option === "--shallow-file") return;
        value = arg.slice(equals + 1);
        if (gitProbeOptions.has(option)) globals.push(arg);
      }
      if (option === "-c") {
        const eq = value.indexOf("=");
        if (eq >= 0) defineAlias(aliases, value.slice(0, eq), value.slice(eq + 1));
      } else if (option === "--config-env") {
        // --config-env=<name>=<envvar>: the variable name follows the last
        // `=`. An alias whose variable is unset makes git refuse to run.
        const eq = value.lastIndexOf("=");
        if (eq > 0 && value.slice(0, eq).toLowerCase().startsWith("alias.")) {
          const variable = env(value.slice(eq + 1));
          if (variable === undefined) return;
          defineAlias(aliases, value.slice(0, eq), variable);
        }
      }
    } else if (gitFlags.has(arg)) {
      if (gitProbeOptions.has(arg)) globals.push(arg);
    } else if (!arg.startsWith("--exec-path=")) {
      // Git rejects a top-level `--` and attached/combined short options.
      // Options such as `--help` and bare `--exec-path` finish without a command.
      return;
    }
  }
}

// defineAlias records `alias.<name>` the way git's config parser keys it:
// section and name compare without case, and `alias.<sub>.<key>` is a
// subsection, not an alias. A later definition wins.
function defineAlias(aliases: Map<string, string>, key: string, value: string) {
  const lower = key.toLowerCase();
  if (!lower.startsWith("alias.")) return;
  const name = lower.slice("alias.".length);
  if (name && !name.includes(".")) aliases.set(name, value);
}

// splitAliasWords splits a non-shell alias as git's split_cmdline does:
// whitespace separates words, quotes group them, and a backslash outside
// single quotes escapes the next character.
function splitAliasWords(text: string): string[] {
  const words: string[] = [];
  let cur = "";
  let started = false;
  let quote = "";
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (quote) {
      if (c === quote) quote = "";
      else if (c === "\\" && quote === '"' && i + 1 < text.length) cur += text[++i];
      else cur += c;
    } else if (c === "'" || c === '"') {
      quote = c;
      started = true;
    } else if (c === "\\" && i + 1 < text.length) {
      cur += text[++i];
      started = true;
    } else if (/\s/.test(c)) {
      if (started) words.push(cur);
      cur = "";
      started = false;
    } else {
      cur += c;
      started = true;
    }
  }
  if (started) words.push(cur);
  return words;
}

function shellQuote(word: string): string {
  return `'${word.replaceAll("'", `'\\''`)}'`;
}

function joinDir(base: string, dir: string): string {
  return path.isAbsolute(dir) ? dir : path.join(base, dir);
}

// Both guards use the same Git invocation. The probe lets Git apply each
// `-C` in sequence and resolve repository selectors from the resulting cwd.
// Command-provided config and exec paths are excluded from the probe.
export function* gitCommands(command: string, start = "", inherited: readonly string[] = []): Generator<GitInvocation> {
  let cwd = start;
  for (const words of segments(stripHeredocBodies(command))) {
    if (words[0] === "cd") {
      cwd = words[1] && words[1] !== "-" ? joinDir(cwd, words[1]) : "";
      continue;
    }
    yield* gitCommand(words, cwd, inherited);
  }
}

// --- lexing ----------------------------------------------------------------
//
// Not a shell: just enough to keep a quoted `;` from splitting a command, to
// drop redirections, and to split on `;`, `&&`, `||`, `|`, `&` and newlines.

// Commands that run the rest of their words as another command, with the
// options each takes first. `values` are short options that consume an
// operand, attached or as the next word; `long` maps long options to the
// short option they spell. `noRun` options make the prefix print, list or
// edit instead of running the utility.
type PrefixSpec = { values: string; long?: Record<string, string>; noRun?: string };
const prefixSpecs: Record<string, PrefixSpec> = {
  env: { values: "uCPSLU", long: { "--unset": "u", "--chdir": "C", "--split-string": "S", "--ignore-environment": "i" } },
  sudo: { values: "ughpCDrtTU", long: { "--user": "u", "--group": "g", "--prompt": "p", "--chdir": "D" }, noRun: "eKlvV" },
  nice: { values: "n", long: { "--adjustment": "n" } },
  nohup: { values: "" },
  time: { values: "of", long: { "--output": "o", "--format": "f" } },
  command: { values: "", noRun: "vV" },
  exec: { values: "a" },
  builtin: { values: "" },
};
export const commandPrefixes = new Set(Object.keys(prefixSpecs));

type Utility = { argv: string[]; vars: Map<string, string>; chdir: string; clearedEnv: boolean };

function assignmentAt(word: string): number {
  const eq = word.indexOf("=");
  return eq > 0 && !word.startsWith("-") && !/[/. ]/.test(word.slice(0, eq)) ? eq : -1;
}

// commandUtility peels assignments and prefix commands (with their own
// options) off a simple command and returns the utility it runs, or null
// when the prefix does not run one. env -S splits its string into words in
// place; env -C and sudo -D change the directory the utility starts in.
// An unknown option is skipped as a flag: the utility after it is what the
// guards must see.
function commandUtility(input: string[]): Utility | null {
  const words = [...input];
  const vars = new Map<string, string>();
  let chdir = "";
  let clearedEnv = false;
  let i = 0;
  for (;;) {
    const word = words[i];
    if (word === undefined) return null;
    const eq = assignmentAt(word);
    if (eq > 0) {
      vars.set(word.slice(0, eq), word.slice(eq + 1)); // FOO=bar cmd …
      i++;
      continue;
    }
    const prefix = path.basename(word);
    const spec = prefixSpecs[prefix];
    if (!spec) break;
    for (i++; i < words.length; i++) {
      const arg = words[i];
      if (arg === "--") {
        i++;
        break;
      }
      if (prefix === "env" && arg === "-") {
        clearedEnv = true; // the historical spelling of -i
        continue;
      }
      if (prefix === "nice" && /^-\d+$/.test(arg)) continue; // nice -5
      const assign = prefix === "env" ? assignmentAt(arg) : -1;
      if (assign > 0) {
        vars.set(arg.slice(0, assign), arg.slice(assign + 1));
        continue;
      }
      if (!arg.startsWith("-") || arg === "-") break;
      let option = "";
      let value: string | undefined;
      if (arg.startsWith("--")) {
        const eqAt = arg.indexOf("=");
        option = spec.long?.[eqAt < 0 ? arg : arg.slice(0, eqAt)] ?? "";
        if (spec.values.includes(option) && option) value = eqAt < 0 ? words[++i] : arg.slice(eqAt + 1);
      } else {
        for (let k = 1; k < arg.length; k++) {
          const letter = arg[k];
          if (spec.noRun?.includes(letter)) return null;
          if (spec.values.includes(letter)) {
            option = letter;
            value = k + 1 < arg.length ? arg.slice(k + 1) : words[++i];
            break;
          }
          if (prefix === "env" && letter === "i") clearedEnv = true;
        }
      }
      if (option && spec.values.includes(option) && value === undefined) return null; // missing operand
      if (prefix === "env" && option === "i") clearedEnv = true;
      if (prefix === "env" && option === "u") vars.delete(value!);
      if (prefix === "env" && option === "S") words.splice(i + 1, 0, ...splitAliasWords(value!));
      if ((prefix === "env" && option === "C") || (prefix === "sudo" && option === "D")) chdir = joinDir(chdir, value!);
    }
  }
  return { argv: words.slice(i), vars, chdir, clearedEnv };
}

// stripPrefixes returns the words of the utility a simple command runs,
// without assignments and prefix commands, or no words when none runs.
export function stripPrefixes(words: string[]): string[] {
  return commandUtility(words)?.argv ?? [];
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
