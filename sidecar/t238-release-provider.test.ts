// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0
// Release's sidecar half: delayed old stream, abort, rebind and provider switch.
import { afterEach, beforeEach, expect, test } from "bun:test";
import { launchAbortFixture, Host } from "./testdata/abort-fixture.ts";
import { connect } from "node:net";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";

let fixture: ReturnType<typeof launchAbortFixture>;
let host: Host;
beforeEach(async () => { fixture = launchAbortFixture(); host = await fixture.ready; }, 35_000);
afterEach(async () => { await fixture?.close(); }, 5_000);

test("release mid-xai turn stops it before anthropic reattach and next prompt", async () => {
  const seat = "release-race", session_id = "s1";
  host.send({ op: "load", seat, session_id, provider: "xai-oauth", model: "grok-4.3", token: "fixture", cwd: fixture.dir });
  const first = await host.next("xai ready", e => e.type === "ready" || e.type === "error", 5_000);
  expect(first).toMatchObject({ type: "ready", provider: "xai-oauth" });
  host.send({ op: "prompt", seat, session_id, request_id: "old", turn_id: "old-turn", text: "HOLD delayed xai" });
  await host.next("old started", e => e.type === "receipt" && e.request_id === "old" && e.receipt?.state === "started", 5_000);
  // The broker sends abort before closing its old connection on plain stop.
  host.send({ op: "abort", seat });
  const end = await host.next("old aborted", e => e.type === "turn_end" && e.turn_id === "old-turn", 5_000);
  expect(end.stop_reason).toBe("aborted");
  await host.next("old cancelled", e => e.type === "receipt" && e.request_id === "old" && e.receipt?.state === "cancelled", 5_000);
  host.send({ op: "load", seat, session_id, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: fixture.dir });
  const switched = await host.next("anthropic ready", e => e.type === "ready" && e.provider === "anthropic" || e.type === "error", 5_000);
  expect(switched).toMatchObject({ type: "ready", provider: "anthropic" });
  host.send({ op: "prompt", seat, session_id, request_id: "new", turn_id: "new-turn", text: "fresh turn" });
  const next = await host.next("new turn", e => e.type === "turn_end" && e.turn_id === "new-turn", 5_000);
  expect(next.stop_reason).not.toBe("aborted");
  expect(host.events.filter(e => e.type === "usage" && e.provider === "xai-oauth" && e.request_id === "new")).toEqual([]);
  expect(host.events.filter(e => e.type === "text" && e.turn_id === "old-turn" && e.text?.includes("never"))).toEqual([]);
}, 15_000);

test("busy provider switch refuses with typed identity, never ready", async () => {
  const seat = "busy", session_id = "s1";
  host.send({ op: "load", seat, session_id, provider: "xai-oauth", model: "grok-4.3", token: "fixture", cwd: fixture.dir });
  await host.next("xai ready", e => e.type === "ready", 5_000);
  host.send({ op: "prompt", seat, session_id, request_id: "old", turn_id: "old-turn", text: "HOLD old" });
  await host.next("old started", e => e.type === "receipt" && e.request_id === "old" && e.receipt?.state === "started", 5_000);
  host.send({ op: "load", seat, session_id, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: fixture.dir });
  const refused = await host.next("typed provider mismatch", e => e.reason === "seat_provider_mismatch", 5_000);
  expect(refused.type).toBe("seat_error");
  expect(host.events.filter(e => e.type === "ready" && e.provider === "anthropic")).toEqual([]);
}, 15_000);

// An actual released host closes its socket; no stale sidecar work may
// continue merely because the agent conversation was retained for resume.
test("explicit stop aborts once and closing host preserves the seat", async () => {
  const seat = "closed", session_id = "s2";
  host.send({ op: "load", seat, session_id, provider: "xai-oauth", model: "grok-4.3", token: "fixture", cwd: fixture.dir });
  await host.next("xai ready", e => e.type === "ready", 5_000);
  host.send({ op: "prompt", seat, session_id, request_id: "old", turn_id: "old-turn", text: "HOLD old" });
  await host.next("old started", e => e.type === "receipt" && e.request_id === "old" && e.receipt?.state === "started", 5_000);
  host.send({ op: "abort", seat, stop_turn: true });
  const stopped = await host.next("explicit stop", e => e.type === "turn_end" && e.turn_id === "old-turn", 5_000);
  expect(stopped.stop_reason).toBe("aborted");
  host.close();
  const spool = join(fixture.dir, "spool");
  const deadline = Date.now() + 5_000;
  let events = "";
  while (Date.now() < deadline) {
    events = readdirSync(spool).filter(n => n.startsWith("events-")).map(n => readFileSync(join(spool, n), "utf8")).join("\n");
    if (events.includes('"type":"turn_end"') && events.includes('"stop_reason":"aborted"')) break;
    await Bun.sleep(10);
  }
  expect(events.match(/"type":"turn_end"/g)?.length).toBe(1);
  expect(events).toContain('"stop_reason":"aborted"');
  const sock = connect(join(fixture.dir, "s.sock"));
  await new Promise<void>((resolve, reject) => { sock.once("connect", resolve); sock.once("error", reject); });
  const nextHost = new Host(sock);
  try {
    nextHost.send({ op: "load", seat, session_id, provider: "anthropic", model: "claude-sonnet-5", token: "fixture", cwd: fixture.dir });
    const ready = await nextHost.next("new provider", e => e.type === "ready" || e.type === "error", 5_000);
    expect(ready).toMatchObject({ type: "ready", how: "switched", provider: "anthropic" });
    nextHost.send({ op: "prompt", seat, session_id, request_id: "new", turn_id: "new-turn", text: "fresh" });
    await nextHost.next("new turn", e => e.type === "turn_end" && e.turn_id === "new-turn", 5_000);
    expect(nextHost.events.filter(e => e.type === "text" && e.turn_id === "old-turn")).toEqual([]);
  } finally { nextHost.close(); }
}, 15_000);

test("transient disconnect alone leaves turn running for adoption", async () => {
  const seat = "reattach", session_id = "s3";
  host.send({ op: "load", seat, session_id, provider: "xai-oauth", model: "grok-4.3", token: "fixture", cwd: fixture.dir });
  await host.next("xai ready", e => e.type === "ready", 5_000);
  host.send({ op: "prompt", seat, session_id, request_id: "old", turn_id: "old-turn", text: "HOLD old" });
  await host.next("old started", e => e.type === "receipt" && e.request_id === "old" && e.receipt?.state === "started", 5_000);
  host.close();
  const sock = connect(join(fixture.dir, "s.sock"));
  await new Promise<void>((resolve, reject) => { sock.once("connect", resolve); sock.once("error", reject); });
  const nextHost = new Host(sock);
  try {
    nextHost.send({ op: "adopt", seat, session_id, provider: "xai-oauth", model: "grok-4.3", token: "fixture", cwd: fixture.dir });
    const ready = await nextHost.next("adopt", e => e.type === "ready" || e.type === "error", 5_000);
    expect(ready).toMatchObject({ type: "ready", how: "adopted", provider: "xai-oauth", turn_id: "old-turn" });
    expect(nextHost.events.filter(e => e.type === "turn_end" && e.turn_id === "old-turn")).toEqual([]);
    nextHost.send({ op: "abort", seat });
    const end = await nextHost.next("abort", e => e.type === "turn_end" && e.turn_id === "old-turn", 5_000);
    expect(end.stop_reason).toBe("aborted");
  } finally { nextHost.close(); }
}, 15_000);
