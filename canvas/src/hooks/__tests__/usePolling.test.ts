// @vitest-environment jsdom
/**
 * usePolling: one call at a time, the next armed only after the previous
 * settles; nothing after unmount, a key change or `enabled` turning false;
 * refresh() supersedes a call in flight.
 */
import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { usePolling, type PollingOptions } from "../usePolling";

type Deferred<T> = { promise: Promise<T>; resolve: (v: T) => void; reject: (e: unknown) => void };
function deferred<T>(): Deferred<T> {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

async function flush() {
  await act(async () => { await Promise.resolve(); await Promise.resolve(); });
}

function setup(over: Partial<PollingOptions<string>> = {}) {
  const calls: Deferred<string>[] = [];
  const load = vi.fn(() => { const d = deferred<string>(); calls.push(d); return d.promise; });
  const apply = vi.fn();
  const onError = vi.fn();
  const nextDelay = vi.fn((r: string | undefined) => (r === "fast" ? 3000 : 10000));
  const props: PollingOptions<string> = { key: "ws-1", enabled: true, load, apply, onError, nextDelay, ...over };
  const hook = renderHook((p: PollingOptions<string>) => usePolling(p), { initialProps: props });
  return { hook, calls, load, apply, onError, nextDelay, props };
}

describe("usePolling", () => {
  beforeEach(() => { vi.useFakeTimers(); });
  afterEach(() => { vi.useRealTimers(); });

  it("loads on mount, then again only after the previous call settles", async () => {
    const { calls, load, apply } = setup();
    expect(load).toHaveBeenCalledTimes(1);
    // The call is still in flight: no amount of time starts a second one.
    await act(async () => { vi.advanceTimersByTime(60_000); });
    expect(load).toHaveBeenCalledTimes(1);
    calls[0].resolve("slow");
    await flush();
    expect(apply).toHaveBeenCalledWith("slow");
    await act(async () => { vi.advanceTimersByTime(9_999); });
    expect(load).toHaveBeenCalledTimes(1);
    await act(async () => { vi.advanceTimersByTime(1); });
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("takes the next delay from the last result", async () => {
    const { calls, load } = setup();
    calls[0].resolve("fast");
    await flush();
    await act(async () => { vi.advanceTimersByTime(3_000); });
    expect(load).toHaveBeenCalledTimes(2);
    calls[1].resolve("slow");
    await flush();
    await act(async () => { vi.advanceTimersByTime(3_000); });
    expect(load).toHaveBeenCalledTimes(2);
    await act(async () => { vi.advanceTimersByTime(7_000); });
    expect(load).toHaveBeenCalledTimes(3);
  });

  it("stops when the delay is null", async () => {
    const { calls, load } = setup({ nextDelay: () => null });
    calls[0].resolve("x");
    await flush();
    await act(async () => { vi.advanceTimersByTime(600_000); });
    expect(load).toHaveBeenCalledTimes(1);
  });

  it("stops on unmount and drops a result that lands after it", async () => {
    const { hook, calls, load, apply } = setup();
    hook.unmount();
    calls[0].resolve("late");
    await flush();
    await act(async () => { vi.advanceTimersByTime(600_000); });
    expect(apply).not.toHaveBeenCalled();
    expect(load).toHaveBeenCalledTimes(1);
  });

  it("clears an armed timer on unmount", async () => {
    const { hook, calls, load } = setup();
    calls[0].resolve("x");
    await flush();
    hook.unmount();
    await act(async () => { vi.advanceTimersByTime(600_000); });
    expect(load).toHaveBeenCalledTimes(1);
  });

  it("does nothing while disabled, and stops when disabled mid-flight", async () => {
    const off = setup({ enabled: false });
    expect(off.load).not.toHaveBeenCalled();

    const { hook, calls, load, apply, props } = setup();
    hook.rerender({ ...props, enabled: false });
    calls[0].resolve("late");
    await flush();
    await act(async () => { vi.advanceTimersByTime(600_000); });
    expect(apply).not.toHaveBeenCalled();
    expect(load).toHaveBeenCalledTimes(1);
  });

  it("refresh() during a call coalesces into one follow-up and drops the superseded result", async () => {
    const { hook, calls, load, apply } = setup();
    act(() => { hook.result.current.refresh(); hook.result.current.refresh(); });
    expect(load).toHaveBeenCalledTimes(1); // never two at once
    calls[0].resolve("before the action");
    await flush();
    expect(apply).not.toHaveBeenCalledWith("before the action");
    expect(load).toHaveBeenCalledTimes(2); // one follow-up for both refreshes
    calls[1].resolve("after the action");
    await flush();
    expect(apply).toHaveBeenCalledWith("after the action");
  });

  it("refresh() while idle loads now and re-arms from there", async () => {
    const { hook, calls, load } = setup();
    calls[0].resolve("slow");
    await flush();
    await act(async () => { vi.advanceTimersByTime(5_000); });
    act(() => { hook.result.current.refresh(); });
    expect(load).toHaveBeenCalledTimes(2);
    calls[1].resolve("slow");
    await flush();
    await act(async () => { vi.advanceTimersByTime(9_999); });
    expect(load).toHaveBeenCalledTimes(2); // the old 10 s timer was cleared
    await act(async () => { vi.advanceTimersByTime(1); });
    expect(load).toHaveBeenCalledTimes(3);
  });

  it("reports errors and keeps polling", async () => {
    const { calls, load, onError, nextDelay } = setup();
    calls[0].reject(new Error("boom"));
    await flush();
    expect(onError).toHaveBeenCalledWith(expect.objectContaining({ message: "boom" }));
    expect(nextDelay).toHaveBeenLastCalledWith(undefined, expect.any(Error));
    await act(async () => { vi.advanceTimersByTime(10_000); });
    expect(load).toHaveBeenCalledTimes(2);
  });

  it("a key change restarts the loop and drops the old call's result", async () => {
    const { hook, calls, load, apply, props } = setup();
    hook.rerender({ ...props, key: "ws-2" });
    expect(load).toHaveBeenCalledTimes(2);
    calls[0].resolve("old");
    calls[1].resolve("new");
    await flush();
    expect(apply).toHaveBeenCalledTimes(1);
    expect(apply).toHaveBeenCalledWith("new");
  });
});
