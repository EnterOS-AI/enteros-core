"use client";

import { useEffect } from "react";
import { connectSocket, disconnectSocket } from "@/store/socket";
import { MonitorPanel } from "@/components/monitor/MonitorPanel";
import s from "@/components/monitor/Monitor.module.css";

/**
 * Standalone OSS Monitor page, served at <slug>/monitor by the Go platform's
 * canvas reverse-proxy (router.go newCanvasProxy NoRoute → Next.js). Anyone
 * self-hosting molecule-core gets this dashboard; the control plane / app only
 * READ the same /monitor APIs it consumes. No mock data, no CP import.
 *
 * The page opens the shared ReconnectingSocket on mount so the embedded panels
 * (traffic chart, topology, HITL inbox) get live WS refresh, and closes it on
 * unmount — same lifecycle the canvas home page uses.
 */
export default function MonitorPage() {
  useEffect(() => {
    connectSocket();
    return () => {
      disconnectSocket();
    };
  }, []);

  return (
    <div className={`${s.monitor} ${s.standalone}`}>
      <div className={s.scroll}>
        <div className={s.inner}>
          <header className={s.head}>
            <h1>Monitor</h1>
            <p>
              Live agent-to-agent traffic, org topology, and the human-in-the-loop
              queue — read straight from this deployment&apos;s own data.
            </p>
          </header>
          <MonitorPanel />
        </div>
      </div>
    </div>
  );
}
