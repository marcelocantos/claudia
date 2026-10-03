// Test-only Bun preload. Production server.ts never imports this file.
// Catalog lookup is redirected to a disposable loopback HTTP fixture;
// the real Anthropic transport, seat, socket and broker pump still run.
import { mock } from "bun:test";
import * as catalog from "@oh-my-pi/pi-catalog/models";
const endpoint = new URL(process.env.T169_PROVIDER_URL!);
if (endpoint.protocol !== "http:" || endpoint.hostname !== "127.0.0.1") throw new Error("T169 requires loopback HTTP");
const fixture = {
  ...catalog.getBundledModel("anthropic", "claude-sonnet-4-5"),
  id: "t169-fixture", name: "T169 fixture", api: "anthropic-messages", provider: "anthropic",
  baseUrl: endpoint.origin, reasoning: false, input: ["text"], contextWindow: 200000, maxTokens: 1024,
  cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
};
mock.module("@oh-my-pi/pi-catalog/models", () => ({ ...catalog, getBundledModel: () => fixture }));
