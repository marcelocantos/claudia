// Only one sidecar serves a socket (🎯T145). Before it touches the socket,
// a sidecar takes an exclusive flock on `<socket>.lock` and holds it for
// its whole life; the kernel drops it when the process dies, however it
// dies, so the lock is never stale. Only the holder may unlink a socket
// file and bind the path. A second sidecar cannot take the lock and exits
// without touching the socket the first one is serving.

import { dlopen, FFIType } from "bun:ffi";
import { openSync, writeFileSync } from "node:fs";

const LOCK_EX = 2;
const LOCK_NB = 4;

let held: number | undefined; // the lock's fd, open until exit

/** Takes the socket's lock; false when a live sidecar already holds it. */
export function claimSocket(sock: string): boolean {
  const libc = process.platform === "darwin" ? "libSystem.B.dylib" : "libc.so.6";
  const { symbols } = dlopen(libc, {
    flock: { args: [FFIType.i32, FFIType.i32], returns: FFIType.i32 },
  });
  const fd = openSync(sock + ".lock", "a", 0o600);
  if (symbols.flock(fd, LOCK_EX | LOCK_NB) !== 0) return false;
  held = fd;
  // The owner names itself, so a redundant start cannot overwrite it.
  writeFileSync(sock + ".pid", String(process.pid), { mode: 0o600 });
  return true;
}

export function holdsSocket(): boolean {
  return held !== undefined;
}
