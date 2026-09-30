import { describe, expect, test } from "bun:test";
import { HOST_CALL_ABORTED, HOST_CONNECTION_LOST, HostCalls, awaitHostCall } from "./hostcalls.ts";

// 🎯T149 / jevons 🎯T927: jevons-po's turn awaited a host tool result on a
// connection that closed; the seat was re-attached on a new one and the
// turn never ended.

describe("HostCalls", () => {
  test("a call answered on its connection resolves", async () => {
    const calls = new HostCalls();
    const answer = calls.open("c1");
    expect(calls.settle("c1", "ok")).toBe(true);
    expect(await answer).toBe("ok");
    expect(calls.size).toBe(0);
  });

  test("a connection that closes fails the calls it was answering", async () => {
    const calls = new HostCalls();
    const lost = calls.open("c1");
    const also = calls.open("c2");
    expect(calls.failAll(HOST_CONNECTION_LOST)).toBe(2);
    await expect(lost).rejects.toThrow(HOST_CONNECTION_LOST);
    await expect(also).rejects.toThrow(HOST_CONNECTION_LOST);
    // A late answer finds nothing waiting and changes nothing.
    expect(calls.settle("c1", "too late")).toBe(false);
  });

  test("closing with nothing outstanding fails nothing", () => {
    expect(new HostCalls().failAll(HOST_CONNECTION_LOST)).toBe(0);
  });

  test("rebinding one seat fails only that seat's calls", async () => {
    const calls = new HostCalls();
    const po = calls.open("c1", "po");
    const other = calls.open("c2", "worker");
    const untagged = calls.open("c3");
    expect(calls.failSeat("po", HOST_CONNECTION_LOST)).toBe(1);
    await expect(po).rejects.toThrow(HOST_CONNECTION_LOST);
    expect(calls.size).toBe(2);
    expect(calls.settle("c2", "still here")).toBe(true);
    expect(await other).toBe("still here");
    expect(calls.settle("c3", "untagged lives")).toBe(true);
    expect(await untagged).toBe("untagged lives");
  });

  test("failSeat on a seat with nothing outstanding fails nothing", () => {
    const calls = new HostCalls();
    calls.open("c1", "worker");
    expect(calls.failSeat("po", HOST_CONNECTION_LOST)).toBe(0);
    expect(calls.size).toBe(1);
  });
});

describe("awaitHostCall", () => {
  test("returns the answer when the turn is not aborted", async () => {
    const ctl = new AbortController();
    expect(await awaitHostCall(Promise.resolve("done"), ctl.signal)).toBe("done");
  });

  test("an aborted turn stops waiting for a host that never answers", async () => {
    const ctl = new AbortController();
    const never = new Promise<string>(() => {});
    const waiting = awaitHostCall(never, ctl.signal);
    ctl.abort();
    await expect(waiting).rejects.toThrow(HOST_CALL_ABORTED);
  });

  test("a turn aborted before the call is refused at once", async () => {
    const ctl = new AbortController();
    ctl.abort();
    await expect(awaitHostCall(new Promise<string>(() => {}), ctl.signal)).rejects.toThrow(HOST_CALL_ABORTED);
  });

  test("a lost connection surfaces through the wait", async () => {
    const calls = new HostCalls();
    const waiting = awaitHostCall(calls.open("c1"), new AbortController().signal);
    calls.failAll(HOST_CONNECTION_LOST);
    await expect(waiting).rejects.toThrow(HOST_CONNECTION_LOST);
  });
});
