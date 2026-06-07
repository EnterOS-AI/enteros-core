"use client";

import { useEffect, useRef, useState } from "react";
import { useChatHistory } from "@/components/tabs/chat/hooks/useChatHistory";
import { useChatSend } from "@/components/tabs/chat/hooks/useChatSend";
import { useChatSocket } from "@/components/tabs/chat/hooks/useChatSocket";
import { appendMessageDeduped } from "@/components/tabs/chat/types";
import s from "./Concierge.module.css";
import { IcChat, IcSend, IcHistory, IcDots } from "./icons";

interface Props {
  workspaceId: string;
  agentName: string;
  online: boolean;
  statusLabel: string;
}

/**
 * Concept-styled concierge chat wired to the real backend: loads
 * /workspaces/:id/chat-history, sends via /workspaces/:id/a2a (useChatSend),
 * and receives agent replies over the websocket (useChatSocket) — the same
 * plumbing the SidePanel ChatTab uses, rendered in the Org Concierge style.
 */
export function ConciergeChat({ workspaceId, agentName, online, statusLabel }: Props) {
  const containerRef = useRef<HTMLDivElement>(null);
  const bottomRef = useRef<HTMLDivElement>(null);
  const [input, setInput] = useState("");

  const history = useChatHistory(workspaceId, containerRef);
  const send = useChatSend(workspaceId, {
    getHistoryMessages: () => history.messages,
    onUserMessage: (msg) => history.setMessages((prev) => [...prev, msg]),
    onAgentMessage: (msg) => history.setMessages((prev) => appendMessageDeduped(prev, msg)),
  });
  const { sending, sendMessage, releaseSendGuards, sendingFromAPIRef } = send;

  useChatSocket(workspaceId, {
    onAgentMessage: (msg) => {
      history.setMessages((prev) => appendMessageDeduped(prev, msg));
      if (sendingFromAPIRef.current) releaseSendGuards();
    },
    onSendComplete: () => { if (sendingFromAPIRef.current) releaseSendGuards(); },
    onSendError: () => { if (sendingFromAPIRef.current) releaseSendGuards(); },
  });

  const count = history.messages.length;
  useEffect(() => {
    bottomRef.current?.scrollIntoView({ block: "end" });
  }, [count, sending]);

  const submit = () => {
    const text = input.trim();
    if (!text || sending) return;
    setInput("");
    void sendMessage(text);
  };

  const hasMessages = count > 0;

  return (
    <section className={s.chat}>
      <div className={s.chatHead}>
        <div className={s.chAv}><IcChat /></div>
        <div className={s.chMeta}>
          <div className={s.chTitle}>{agentName}</div>
          <div className={s.chSub}>
            <span className={s.sdot} style={{ background: online ? "var(--green)" : "var(--grey)" }} />
            {online ? "online" : statusLabel} · platform agent
          </div>
        </div>
        <div className={s.chTools}>
          <button className={s.iconPill} title="History"><IcHistory /></button>
          <button className={s.iconPill} title="More"><IcDots /></button>
        </div>
      </div>

      <div className={s.chatScroll} ref={containerRef}>
        {hasMessages ? (
          <div className={s.chatInner}>
            {history.messages.map((m) => (
              <div key={m.id} className={`${s.msg} ${m.role === "user" ? s.user : s.bot}`}>
                <div className={s.msgAv}>{m.role === "user" ? "HW" : <IcChat />}</div>
                <div className={s.bubbleWrap}>
                  <div className={s.bubble}>{m.content}</div>
                </div>
              </div>
            ))}
            {sending && (
              <div className={`${s.msg} ${s.bot}`}>
                <div className={s.msgAv}><IcChat /></div>
                <div className={s.bubbleWrap}><div className={s.bubble}>…</div></div>
              </div>
            )}
            <div ref={bottomRef} />
          </div>
        ) : (
          <div className={s.greetWrap}>
            <div className={s.greet}><span className={s.stamp}>✷</span> How can I help?</div>
          </div>
        )}
      </div>

      <div className={s.composer}>
        <div className={s.composerInner}>
          <div className={s.inputBox}>
            <div className={s.inputTop}>
              <textarea
                className={s.msgInput}
                rows={1}
                placeholder={online ? "Message your concierge" : `Agent is ${statusLabel} — messages may not be delivered`}
                value={input}
                onChange={(e) => setInput(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" && !e.shiftKey) {
                    e.preventDefault();
                    submit();
                  }
                }}
              />
              <button className={s.send} title="Send" onClick={submit} disabled={sending || !input.trim()}>
                <IcSend />
              </button>
            </div>
            <div className={s.inputBottom}>
              <span className={s.hint}><kbd>↵</kbd>&nbsp;send</span>
            </div>
          </div>
        </div>
      </div>
    </section>
  );
}
