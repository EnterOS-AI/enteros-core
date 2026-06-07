"use client";

import { useCanvasStore, type TopView } from "@/store/canvas";

const TABS: { id: TopView; label: string }[] = [
  { id: "home", label: "Home" },
  { id: "map", label: "Map" },
];

/**
 * Top-level view switcher — toggles between the Org Concierge "Home" (chat
 * with the platform agent) and the "Map" (node-graph canvas). Fixed at the
 * top-left so it sits clear of the centred canvas Toolbar.
 */
export function TopViewTabs() {
  const topView = useCanvasStore((s) => s.topView);
  const setTopView = useCanvasStore((s) => s.setTopView);

  return (
    <div
      role="tablist"
      aria-label="Canvas view"
      className="fixed left-4 top-3 z-[60] flex items-center gap-0.5 rounded-xl border border-line/70 bg-surface-sunken/90 p-1 backdrop-blur-sm shadow-lg shadow-black/30"
    >
      {TABS.map((t) => {
        const active = topView === t.id;
        return (
          <button
            key={t.id}
            role="tab"
            aria-selected={active}
            onClick={() => setTopView(t.id)}
            className={`rounded-lg px-3.5 py-1.5 text-[12px] font-medium transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 ${
              active
                ? "bg-accent text-white shadow-sm"
                : "text-ink-mid hover:text-ink hover:bg-surface-card/60"
            }`}
          >
            {t.label}
          </button>
        );
      })}
    </div>
  );
}
