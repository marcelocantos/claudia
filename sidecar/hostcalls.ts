// Host tool calls in flight on one connection (jevons 🎯T927).
//
// A host tool (jevons_*, a seat's MCP servers) runs in the Go process that
// holds this seat's connection, and its result comes back on that connection.
// When the connection closes first — the host restarted, or lost its handle
// and re-attached on a new one — nothing will ever answer the call. Before
// this, the turn awaited that answer forever: every later prompt was queued
// behind a turn that could not end, and the seat went mute while it kept
// saying "accepted" (jevons-po, 2026-09-29, for more than two hours).
//
// So a connection's outstanding calls fail when it closes, and a call honours
// the turn's abort signal. The model sees a failed tool, not a hung one, and
// the turn ends like any other.

export const HOST_CONNECTION_LOST =
  "the host connection running this tool closed before it answered (the host restarted or re-attached). " +
  "The tool's outcome is unknown: check the state it would have changed before repeating it.";

export const HOST_CALL_ABORTED = "host tool call aborted with its turn; any result it produces is discarded";

type Waiter = { resolve: (result: string) => void; reject: (err: Error) => void };

export class HostCalls {
  private pending = new Map<string, Waiter>();

  // open registers a call before it is sent, so an answer can never arrive
  // ahead of its waiter.
  open(callId: string): Promise<string> {
    return new Promise<string>((resolve, reject) => {
      this.pending.set(callId, { resolve, reject });
    });
  }

  // settle delivers a result; false when nothing is waiting for it.
  settle(callId: string, result: string): boolean {
    const w = this.pending.get(callId);
    if (!w) return false;
    this.pending.delete(callId);
    w.resolve(result);
    return true;
  }

  // failAll rejects every outstanding call and reports how many there were.
  failAll(reason: string): number {
    const waiters = [...this.pending.values()];
    this.pending.clear();
    for (const w of waiters) w.reject(new Error(reason));
    return waiters.length;
  }

  get size(): number {
    return this.pending.size;
  }
}

// awaitHostCall waits for call unless signal aborts first.
export function awaitHostCall(call: Promise<string>, signal?: AbortSignal): Promise<string> {
  if (!signal) return call;
  if (signal.aborted) return Promise.reject(new Error(HOST_CALL_ABORTED));
  return new Promise<string>((resolve, reject) => {
    const onAbort = () => reject(new Error(HOST_CALL_ABORTED));
    signal.addEventListener("abort", onAbort, { once: true });
    call.then(
      (result) => {
        signal.removeEventListener("abort", onAbort);
        resolve(result);
      },
      (err: unknown) => {
        signal.removeEventListener("abort", onAbort);
        reject(err instanceof Error ? err : new Error(String(err)));
      },
    );
  });
}
