"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { usePolling } from "@/hooks/usePolling";
import { fetchLight } from "@/lib/api/light";
import { lightErrorSentence, lightInTransition, type LightInfo, type LightState } from "@/lib/light";

/** GET /light cadence while the Schedule tab is open, like its schedule
 *  list's 10 s poll; 3 s while a park or open is in progress. */
export const LIGHT_POLL_MS = 10_000;
export const LIGHT_TRANSITION_POLL_MS = 3_000;

export interface LightInfoState {
  /** The last GET /light answer; null until the first one (or when the agent
   *  is not a Light agent). */
  info: LightInfo | null;
  /** Why the last GET /light failed, null after a success. */
  error: string | null;
  /** Fetch now; supersedes a fetch in flight. */
  refresh: () => void;
  /** Adopt the state a management call answered (park → parking, …) at
   *  once, then confirm it with a fetch. */
  applyState: (state: LightState | null) => void;
}

/**
 * Polls GET /workspaces/{id}/light while `enabled` (the agent is a Light
 * agent). It refetches when the node's light_state moves on (a
 * WORKSPACE_UPDATED from another tab or core) and reports every answered
 * state through `onState`, so the caller can correct a node that missed an
 * event. Polling stops on unmount and when `enabled` turns false.
 */
export function useLightInfo(
  workspaceId: string,
  nodeLightState: LightState | null | undefined,
  enabled: boolean,
  onState?: (state: LightState | null) => void,
): LightInfoState {
  const [info, setInfo] = useState<LightInfo | null>(null);
  const [error, setError] = useState<string | null>(null);
  const infoRef = useRef(info);
  infoRef.current = info;
  const onStateRef = useRef(onState);
  onStateRef.current = onState;

  const poll = usePolling<LightInfo>({
    key: workspaceId,
    enabled,
    load: () => fetchLight(workspaceId),
    apply: (next) => {
      setInfo(next);
      setError(null);
      onStateRef.current?.(next.light_state);
    },
    onError: (e) => setError(lightErrorSentence(e)),
    nextDelay: (next) =>
      lightInTransition((next ?? infoRef.current)?.light_state) ? LIGHT_TRANSITION_POLL_MS : LIGHT_POLL_MS,
  });

  useEffect(() => {
    if (!enabled) {
      setInfo(null);
      setError(null);
    }
  }, [enabled]);

  // A node state change this hook has not seen yet (another tab parked the
  // agent, core finished a transition): fetch now instead of waiting up to
  // 10 s. Becoming a Light agent needs no refresh: polling starts with a fetch.
  const prevNodeState = useRef(nodeLightState);
  useEffect(() => {
    const prev = prevNodeState.current;
    prevNodeState.current = nodeLightState;
    if (prev === nodeLightState || prev == null || !enabled) return;
    if (nodeLightState !== infoRef.current?.light_state) poll.refresh();
  }, [nodeLightState, enabled, poll]);

  const applyState = useCallback(
    (state: LightState | null) => {
      setInfo((prev) => (prev ? { ...prev, light_state: state, light_state_at: new Date().toISOString() } : prev));
      onStateRef.current?.(state);
      poll.refresh();
    },
    [poll],
  );

  return { info, error, refresh: poll.refresh, applyState };
}
