// Broker recovery, sidecar receipt and attempt-token comparison (T169).
import { createHash } from "node:crypto";

export const tokenFingerprint = (token: string) => createHash("sha256").update(token).digest("hex");
export function authRefusal(error: string): boolean {
  const lower = error.toLowerCase();
  return ["could not be validated", "unauthenticated:bad-credentials", "invalid_token",
    "authentication_error", "401 oauth access token has expired"].some((s) => lower.includes(s));
}
export type RetryReply = { request_id?: string; turn_id?: string; token?: string; expires_at?: number };

export class AuthRetry {
  #pending?: { request: string; turn: string; failed: string; resolve: (token: string) => void };
  constructor(private emit: (event: { type: string; request_id: string; turn_id: string; failed_token: string; error: string }) => void, private timeout = 30_000) {}
  async recover(turn: string, failed: string, error: string): Promise<string> {
    if (!turn || !failed || !authRefusal(error) || this.#pending) return "";
    const request = crypto.randomUUID();
    let timer: ReturnType<typeof setTimeout>;
    try {
      return await new Promise<string>((resolve) => {
        this.#pending = { request, turn, failed, resolve };
        timer = setTimeout(() => this.cancel(), this.timeout);
        this.emit({ type: "auth_retry", request_id: request, turn_id: turn,
          failed_token: tokenFingerprint(failed), error });
      });
    } finally {
      clearTimeout(timer!);
      this.#pending = undefined;
    }
  }
  receive(reply: RetryReply): void {
    const p = this.#pending;
    if (!p || reply.request_id !== p.request || reply.turn_id !== p.turn) return;
    const valid = reply.token && reply.token !== p.failed && Number.isFinite(reply.expires_at) && reply.expires_at! > Date.now();
    p.resolve(valid ? reply.token! : "");
  }
  cancel(): void { this.#pending?.resolve(""); }
}
