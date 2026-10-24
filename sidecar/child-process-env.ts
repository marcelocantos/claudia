// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

// Host-supplied per-seat child environment overlay (🎯T218). Claudia merges
// these entries into spawned tool children only; the sidecar process env is
// never mutated.

export type ChildProcessEnv = Record<string, string>;

const NAME_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

export function validateChildProcessEnv(env: ChildProcessEnv | undefined): void {
  if (!env) return;
  for (const [key, val] of Object.entries(env)) {
    if (!key) throw new Error("child_process_env: empty variable name");
    if (key.includes("=")) throw new Error(`child_process_env: variable name ${JSON.stringify(key)} contains '='`);
    if (key.includes("\0") || val.includes("\0")) throw new Error(`child_process_env: variable ${JSON.stringify(key)} contains NUL`);
    if (!NAME_RE.test(key)) throw new Error(`child_process_env: invalid variable name ${JSON.stringify(key)}`);
  }
}

export function cloneChildProcessEnv(env: ChildProcessEnv | undefined): ChildProcessEnv {
  if (!env) return {};
  return { ...env };
}

/** Merge overlay onto the sidecar baseline without mutating process.env. */
export function spawnEnvForSeat(overlay: ChildProcessEnv | undefined): NodeJS.ProcessEnv {
  if (!overlay || Object.keys(overlay).length === 0) {
    return { ...process.env };
  }
  return { ...process.env, ...overlay };
}
