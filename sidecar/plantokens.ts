// One access token per plan (🎯T159). Every seat on a plan uses the plan's
// current token, so a refresh reaches them all at once. A per-seat copy went
// stale whenever the plan refreshed, and each stale seat found out only by
// being refused.

// PlanSeat is what a plan's token reaches on a seat.
export type PlanSeat = {
  provider: string;
  token: string;
  agent: { setToken: (token: string) => void };
};

export class PlanTokens {
  #tokens = new Map<string, string>();

  get(provider: string): string | undefined {
    return this.#tokens.get(provider);
  }

  // set makes token the plan's, for every seat on it that holds another.
  set(provider: string, token: string, seats: Iterable<PlanSeat>): number {
    if (!provider || !token) return 0;
    this.#tokens.set(provider, token);
    let moved = 0;
    for (const s of seats) {
      if (s.provider === provider && s.token !== token) {
        s.token = token;
        s.agent.setToken(token);
        moved++;
      }
    }
    return moved;
  }
}
