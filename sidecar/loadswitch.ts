// A load of a seat that is already loaded (🎯T195). The host sends one to
// re-attach a seat it holds — a broker grant, a reconnect — and to switch
// one: a switch names a request id and waits for the answer.
//
// Only an explicit switch may fail the load. A re-attach that names another
// model (the host's row lagging the seat, or naming the default the seat
// already resolved) switches when the seat can take it, and otherwise keeps
// the seat on its own model: failing it would kill a working seat mid-turn
// over a label.

export type Loaded = { provider: string; model: string };

export type LoadSwitch =
  | { ok: true; switched: boolean; refused?: string }
  | { ok: false; error: string };

export async function switchOnLoad(
  current: Loaded,
  want: { provider?: string; model?: string; request_id?: string },
  switchModel: (provider: string, model: string) => Promise<void>,
): Promise<LoadSwitch> {
  const provider = want.provider || current.provider;
  const model = want.model || current.model;
  if (provider === current.provider && model === current.model) return { ok: true, switched: false };
  try {
    await switchModel(provider, model);
  } catch (err) {
    const text = err instanceof Error ? err.message : String(err);
    if (want.request_id) return { ok: false, error: text };
    return { ok: true, switched: false, refused: text };
  }
  current.provider = provider;
  current.model = model;
  return { ok: true, switched: true };
}
