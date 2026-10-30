// In-flight bound compactions keyed by host request_id (🎯T244 cancel/settle).

const inflight = new Map<string, { seat: string; abort: AbortController }>();

/** Registers a bound compact; abort is triggered by out-of-band compact_abort. */
export function registerBoundCompact(seat: string, requestId: string): AbortController {
  const abort = new AbortController();
  inflight.set(requestId, { seat, abort });
  return abort;
}

/** Out-of-band cancel for one compact request (must not pass through withSeatCommand). */
export function abortBoundCompact(seat: string, requestId: string): boolean {
  const entry = inflight.get(requestId);
  if (!entry || entry.seat !== seat) return false;
  entry.abort.abort();
  return true;
}

export function unregisterBoundCompact(seat: string, requestId: string): void {
  const entry = inflight.get(requestId);
  if (entry?.seat === seat) inflight.delete(requestId);
}
