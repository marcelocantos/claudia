// Per-seat serialization for mutating sidecar commands (🎯T244). Without this,
// concurrent void handle(...) calls race on maintenance compaction and turns.

const tails = new Map<string, Promise<void>>();

/** Runs fn after every prior mutating command for this seat finishes. */
export function withSeatCommand<T>(seat: string, fn: () => Promise<T>): Promise<T> {
  if (!seat) return fn();
  const prev = tails.get(seat) ?? Promise.resolve();
  let release!: () => void;
  const slot = new Promise<void>((resolve) => {
    release = resolve;
  });
  const chained = prev.then(() => slot);
  tails.set(seat, chained);
  const run = prev.then(fn);
  void run.finally(() => {
    release();
    if (tails.get(seat) === chained) tails.delete(seat);
  });
  return run;
}
