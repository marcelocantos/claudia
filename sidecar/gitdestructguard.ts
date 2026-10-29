// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Literal Bash git pre-exec admission for the shared clone (🎯T240).
// The sidecar's Bash executes only after this check. This is deliberately not
// a general shell interpreter: dynamically generated commands remain a residual.
import path from "node:path";
import { segments, stripHeredocBodies, stripPrefixes, runGit, type GitRunner } from "./branchguard.ts";

type Hit = { dir: string; operation: string; paths: string[]; untrackedOnly: boolean; includeIgnored: boolean; cleanArgs: string[] };

export function destructiveGitCommands(command: string): Hit[] {
  const hits: Hit[] = [];
  let dir = "";
  for (const words of segments(stripHeredocBodies(command))) {
    if (words[0] === "cd") {
      dir = words[1] && words[1] !== "-"
        ? (path.isAbsolute(words[1]) ? words[1] : path.join(dir, words[1])) : "";
      continue;
    }
    const cmd = stripPrefixes(words);
    if (path.basename(cmd[0] || "") !== "git") continue;
    let at = 1, target = dir;
    while (at < cmd.length) {
      if (cmd[at] === "-C" && cmd[at + 1]) {
        const next = cmd[at + 1];
        target = path.isAbsolute(next) ? next : path.join(target, next); at += 2;
      } else if (cmd[at].startsWith("-")) at++;
      else break;
    }
    const sub = cmd[at++];
    const args = cmd.slice(at);
    let operation = "", paths: string[] = [], untrackedOnly = false, includeIgnored = false;
    let cleanArgs: string[] = [];
    const dash = args.indexOf("--");
    const positional = args.filter((a) => !a.startsWith("-"));
    switch (sub) {
      case "stash":
        if (!args.length || !["list", "show", "create"].includes(args[0])) operation = "stash";
        break;
      case "reset":
        if (args.some((a) => ["--hard", "--merge", "--keep"].some((flag) => a === flag || a.startsWith(flag + "=")))) operation = "reset";
        break;
      case "checkout":
        if (dash >= 0) { operation = "checkout paths"; paths = args.slice(dash + 1); }
        else if (positional.length === 1 && !args.some((a) => ["-b", "-B", "--orphan"].includes(a))) {
          // Could be a ref; resolve it in guardDestructiveGit before refusing.
          operation = "checkout path?"; paths = positional;
        }
        break;
      case "restore":
        if (!args.includes("--staged") || args.includes("--worktree")) {
          operation = "restore worktree";
          paths = dash >= 0 ? args.slice(dash + 1) : args.filter((a, i) => !a.startsWith("-") && args[i - 1] !== "--source");
        }
        break;
      case "checkout-index":
        if (args.some((a) => a === "--force" || /^-[^-]*f/.test(a))) { operation = "checkout-index -f"; paths = dash >= 0 ? args.slice(dash + 1) : positional; }
        break;
      case "clean":
        if (args.some((a) => a === "--force" || /^-[^-]*f/.test(a))) {
          operation = "clean -f"; untrackedOnly = true;
          includeIgnored = args.includes("-x") || args.includes("-X") || args.some((a) => /^-[^-]*[xX]/.test(a));
          // Preserve the actual options for a read-only git clean dry-run. In
          // particular -e/--exclude consumes an operand, not a pathspec.
          // Git itself applies excludes, ignore rules, and pathspecs.
          cleanArgs = args;
        }
        break;
    }
    if (operation) hits.push({ dir: target, operation, paths, untrackedOnly, includeIgnored, cleanArgs });
  }
  return hits;
}

// Porcelain -z preserves spaces and newlines in filenames. If status fails,
// deny with an explicit unknown path set rather than launching destructive git.
export async function guardDestructiveGit(cwd: string, command: string, git: GitRunner = runGit): Promise<void> {
  for (const hit of destructiveGitCommands(command)) {
    const dir = path.resolve(cwd, hit.dir || ".");
    const gitDir = await git(dir, ["rev-parse", "--path-format=absolute", "--git-dir"]);
    const common = await git(dir, ["rev-parse", "--path-format=absolute", "--git-common-dir"]);
    if (gitDir === null || common === null || gitDir !== common) continue;
    if (hit.operation === "checkout path?") {
      const ref = await git(dir, ["rev-parse", "--verify", "--quiet", `${hit.paths[0]}^{commit}`]);
      if (ref !== null) continue;
    }
    const repo = await git(dir, ["rev-parse", "--show-toplevel"]);
    // `git clean -n` runs Git's own selection without deleting anything.
    // Parsing -e's value as a pathspec makes status report no paths, while
    // simply listing status would falsely name excluded files. Its dry-run
    // output tells us exactly which untracked/ignored entries this clean
    // would remove. A failed dry-run remains a refusal, not permission.
    const cleanPreview = hit.operation === "clean -f" ? await git(dir, ["clean", "-n", ...hit.cleanArgs]) : null;
    const raw = hit.operation === "clean -f" ? null : await git(dir, ["status", "--porcelain=v1", "-z", "--untracked-files=all", ...(hit.includeIgnored ? ["--ignored"] : []), ...(hit.paths.length ? ["--", ...hit.paths] : [])]);
    const entries: string[] = [];
    if (raw !== null) {
      const fields = raw.split("\0");
      for (let i = 0; i < fields.length; i++) {
        const field = fields[i];
        if (!field || field.length < 4) continue;
        const xy = field.slice(0, 2);
        if (!hit.untrackedOnly || (xy === "??" || (hit.includeIgnored && xy === "!!"))) entries.push(field.slice(3));
        if (/[RC]/.test(xy)) i++; // -z rename/copy adds the original path.
      }
    }
    const cleanNames = cleanPreview?.split("\n").filter((line) => line.startsWith("Would remove "))
      .map((line) => line.slice("Would remove ".length)) ?? [];
    const names = hit.operation === "clean -f"
      ? cleanPreview === null ? "clean dry-run unavailable (paths unknown)" : cleanNames.length ? cleanNames.map((p) => JSON.stringify(p)).join(", ") : "none observed"
      : raw === null ? "status unavailable (paths unknown)" : entries.length ? entries.map((p) => JSON.stringify(p)).join(", ") : "none observed";
    throw new Error(`gitdestructguard: refusing git ${hit.operation} in shared clone ${repo || dir}; dirty paths command would touch: ${names}; command was not run. Work in your own linked worktree. 🎯T240`);
  }
}
