"use client";

import { useEffect, useMemo, useRef } from "react";

export interface PollingOptions<T> {
  /** What is polled. A change restarts the loop and drops a call in flight. */
  key: string;
  /** False stops the loop (and drops a call in flight). */
  enabled: boolean;
  load: () => Promise<T>;
  /** Receives each result the loop still wants: never after unmount, a key
   *  change, `enabled` turning false, or a refresh() that superseded it. */
  apply: (result: T) => void;
  onError?: (err: unknown) => void;
  /** Milliseconds until the next call once one settles; null stops. */
  nextDelay: (result: T | undefined, err: unknown) => number | null;
}

/**
 * Polls `load` with a chained timer: the next call is armed only after the
 * previous one settles, so two calls never overlap however slow the server
 * is (a fixed setInterval would stack them). `refresh()` loads now; when a
 * call is in flight it marks that call's result stale and runs exactly one
 * follow-up after it, so an action's answer is never overwritten by a read
 * that started before the action.
 */
export function usePolling<T>(opts: PollingOptions<T>): { refresh: () => void } {
  const optsRef = useRef(opts);
  optsRef.current = opts;
  const refreshRef = useRef<() => void>(() => {});

  const { key, enabled } = opts;
  useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    let inFlight = false;
    let again = false;
    let epoch = 0;
    let timer: ReturnType<typeof setTimeout> | null = null;

    const clearTimer = () => {
      if (timer !== null) {
        clearTimeout(timer);
        timer = null;
      }
    };

    const tick = async (): Promise<void> => {
      if (cancelled) return;
      if (inFlight) {
        again = true;
        return;
      }
      clearTimer();
      inFlight = true;
      const myEpoch = epoch;
      let result: T | undefined;
      let error: unknown = null;
      try {
        result = await optsRef.current.load();
      } catch (e) {
        error = e;
      }
      inFlight = false;
      if (cancelled) return;
      if (myEpoch === epoch) {
        if (error === null) optsRef.current.apply(result as T);
        else optsRef.current.onError?.(error);
      }
      if (again) {
        again = false;
        void tick();
        return;
      }
      const ms = optsRef.current.nextDelay(error === null ? result : undefined, error);
      if (ms !== null) timer = setTimeout(() => void tick(), ms);
    };

    refreshRef.current = () => {
      epoch++;
      void tick();
    };
    void tick();
    return () => {
      cancelled = true;
      clearTimer();
      refreshRef.current = () => {};
    };
  }, [key, enabled]);

  return useMemo(() => ({ refresh: () => refreshRef.current() }), []);
}
