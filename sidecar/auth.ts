// Broker-only helper (🎯T864.2). Runs one provider's existing pi-ai
// login or refresh. Stdout is a Record JSON object. A failure does not
// fall through to an API key, models.yml, or ~/.omp/agent/agent.db.

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
const blob = process.argv[4] ?? "{}";

if (!cmd || !provider) {
  console.error("usage: bun auth.ts refresh|login <provider> [record-json]");
  process.exit(2);
}

type RecordOut = {
  refresh_token: string;
  access_token: string;
  expiry: string;
};

function toRecord(creds: Record<string, unknown>): RecordOut {
  const access = String(creds.access ?? creds.access_token ?? "");
  const refresh = String(creds.refresh ?? creds.refresh_token ?? "");
  const exp = creds.expires ?? creds.expiry;
  let expiry = new Date(Date.now() + 3600_000).toISOString();
  if (typeof exp === "number") {
    expiry = new Date(exp > 1e12 ? exp : exp * 1000).toISOString();
  } else if (typeof exp === "string" && exp) {
    const parsed = new Date(exp);
    if (!Number.isNaN(parsed.getTime())) expiry = parsed.toISOString();
  }
  return { refresh_token: refresh, access_token: access, expiry };
}

const existing = JSON.parse(blob) as Record<string, unknown>;

if (cmd === "refresh" && existing.refresh_token) {
  const next = await refreshOAuthToken(provider, {
    refresh: String(existing.refresh_token),
    access: String(existing.access_token ?? ""),
    expires: existing.expiry ? Date.parse(String(existing.expiry)) : 0,
  });
  if (!next) {
    console.error("refreshOAuthToken returned nothing");
    process.exit(1);
  }
  process.stdout.write(JSON.stringify(toRecord(next as Record<string, unknown>)));
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
  },
});
if (!creds) {
  console.error("pi-ai login returned nothing");
  process.exit(1);
}
process.stdout.write(JSON.stringify(toRecord(creds as Record<string, unknown>)));
