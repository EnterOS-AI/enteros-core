"use client";

import { useRef, useState } from "react";
import { openLight } from "@/lib/api/light";
import { lightErrorSentence, type LightState } from "@/lib/light";

export const LIGHT_CHAT_PARKED_TEXT =
  "This Light agent is parked: it runs its schedules while asleep. Open it to chat.";
export const LIGHT_CHAT_OPENING_TEXT = "Opening the agent. Chat is available once it is up.";

interface Props {
  workspaceId: string;
  /** "opening" shows progress; anything else the parked banner with Open. */
  state: LightState | null;
  /** POST /light/open answered this state. */
  onOpened: (state: LightState | null) => void;
}

/**
 * The Chat tab's banner for a Light agent that cannot chat right now: parked
 * or being parked (a message would answer 409 LIGHT_AGENT_PARKED / PARKING),
 * or opening. "Open" sends POST /light/open once; the banner goes away when
 * the node's light_state moves on.
 */
export function ChatLightBanner({ workspaceId, state, onOpened }: Props) {
  const [pending, setPending] = useState(false);
  const pendingRef = useRef(false);
  const [error, setError] = useState<string | null>(null);

  const open = async () => {
    if (pendingRef.current) return;
    pendingRef.current = true;
    setPending(true);
    setError(null);
    try {
      const r = await openLight(workspaceId);
      onOpened(r.light_state);
    } catch (e) {
      setError(lightErrorSentence(e));
    } finally {
      pendingRef.current = false;
      setPending(false);
    }
  };

  const opening = state === "opening";
  return (
    <div data-testid="chat-light-banner" className="px-3 py-2 bg-surface-sunken border-b border-line/40 shrink-0">
      <div role="status" aria-live="polite" className="flex items-center gap-2">
        <span className="text-[10px] text-ink-mid flex-1">{opening ? LIGHT_CHAT_OPENING_TEXT : LIGHT_CHAT_PARKED_TEXT}</span>
        {!opening && (
          <button
            type="button"
            onClick={() => void open()}
            disabled={pending}
            className="px-2 py-0.5 text-[10px] font-medium bg-accent/10 hover:bg-accent/20 text-accent rounded border border-accent/30 transition-colors shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:ring-offset-surface disabled:opacity-40 disabled:cursor-not-allowed"
          >
            {pending ? "Opening…" : "Open"}
          </button>
        )}
      </div>
      {error && (
        <p role="alert" aria-live="assertive" className="mt-1 text-[10px] text-bad">
          {error}
        </p>
      )}
    </div>
  );
}
