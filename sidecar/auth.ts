// Broker-only helper (🎯T864.2). Runs one provider's existing pi-ai
// login or refresh. Stdout is a Record JSON object. A failure does not
// fall through to an API key, models.yml, or ~/.omp/agent/agent.db.
// The existing record arrives on stdin (preferred) or argv[4].

import { getProviderDefinition, refreshOAuthToken } from "@oh-my-pi/pi-ai";

const banned = [
  "ANTHROPIC_API_KEY",
  "OPENAI_API_KEY",
  "XAI_API_KEY",
  "CURSOR_ACCESS_TOKEN",
];
for (const name of banned) delete process.env[name];

const cmd = process.argv[2];
const provider = process.argv[3];

if (!cmd || !provider) {
  console.error("usage: bun auth.ts refresh|login <provider>  (record JSON on stdin)");
  process.exit(2);
}

type RecordOut = {
  refresh_token: string;
  access_token: string;
  expiry: string;
};

function usable(token: string): boolean {
  return token !== "" && token !== "undefined";
}

function toRecord(creds: unknown): RecordOut {
  if (typeof creds === "string") {
    if (!usable(creds)) {
      console.error("pi-ai returned an empty string grant");
      process.exit(1);
    }
    // Cursor's api-key-shaped login returns the user key that
    // refreshCursorToken accepts as Bearer.
    return {
      refresh_token: creds,
      access_token: creds,
      expiry: new Date(Date.now() + 3600_000).toISOString(),
    };
  }
  const rec = (creds ?? {}) as Record<string, unknown>;
  const access = String(rec.access ?? rec.access_token ?? rec.accessToken ?? "");
  const refresh = String(rec.refresh ?? rec.refresh_token ?? rec.refreshToken ?? "");
  if (!usable(access) || !usable(refresh)) {
    console.error(`pi-ai omitted refreshable fields (keys: ${Object.keys(rec).join(",")})`);
    process.exit(1);
  }
  const exp = rec.expires ?? rec.expiry ?? rec.expiresAt;
  let expiry = new Date(Date.now() + 3600_000).toISOString();
  if (typeof exp === "number") {
    expiry = new Date(exp > 1e12 ? exp : exp * 1000).toISOString();
  } else if (typeof exp === "string" && exp) {
    const parsed = new Date(exp);
    if (!Number.isNaN(parsed.getTime())) expiry = parsed.toISOString();
  }
  return { refresh_token: refresh, access_token: access, expiry };
}

const stdin = await new Response(Bun.stdin).text();
const fromArg = process.argv[4] ?? "";
const existing = JSON.parse((stdin.trim() || fromArg.trim() || "{}")) as Record<string, unknown>;
const force = (process.env.OMP_FORCE_LOGIN ?? "").split(",").map((s) => s.trim()).filter(Boolean);
const refreshTok = String(existing.refresh_token ?? existing.refresh ?? "");

if (cmd === "refresh" && !force.includes(provider) && usable(refreshTok)) {
  const next = await refreshOAuthToken(provider, {
    refresh: refreshTok,
    access: String(existing.access_token ?? existing.access ?? ""),
    expires: existing.expiry ? Date.parse(String(existing.expiry)) : 0,
  });
  if (!next) {
    console.error("refreshOAuthToken returned nothing");
    process.exit(1);
  }
  process.stdout.write(JSON.stringify(toRecord(next)));
  process.exit(0);
}

const def = getProviderDefinition(provider);
if (!def?.login) {
  console.error(`pi-ai has no login for ${provider}`);
  process.exit(1);
}
const creds = await def.login({
  onAuth: ({ url }: { url: string }) => {
    console.error(url);
    if (url) {
      Bun.spawn(["open", url], { stdout: "ignore", stderr: "ignore" }).unref();
    }
  },
});
if (!creds) {
  console.error("pi-ai login returned nothing");
  process.exit(1);
}
process.stdout.write(JSON.stringify(toRecord(creds)));
