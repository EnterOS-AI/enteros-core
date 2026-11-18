"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import type { RunLightAction } from "./LightCard";

/**
 * One Light request at a time for a whole tab: while one is in flight,
 * `busy` names it (every action button disables on it) and a second `run`
 * returns null without sending. The ref makes the guard synchronous, so a
 * double click inside one tick cannot send two parks before React re-renders.
 */
export function useSingleFlight(): { busy: string | null; run: RunLightAction } {
  const [busy, setBusy] = useState<string | null>(null);
  const busyRef = useRef<string | null>(null);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const run = useCallback<RunLightAction>(async (name, call) => {
    if (busyRef.current !== null) return null;
    busyRef.current = name;
    setBusy(name);
    try {
      return { ok: true as const, value: await call() };
    } catch (error) {
      return { ok: false as const, error };
    } finally {
      busyRef.current = null;
      if (mounted.current) setBusy(null);
    }
  }, []);

  return { busy, run };
}
