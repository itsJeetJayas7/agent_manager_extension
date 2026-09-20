import { useEffect, useRef, useState } from "react";
import { initStore, useConductor } from "./state/store";
import QueueView from "./views/QueueView";
import BatchesView from "./views/BatchesView";
import ControlView from "./views/ControlView";
import BatchPlanner from "./views/BatchPlanner";
import BatchDetail from "./views/BatchDetail";

const TABS = ["QUEUE", "BATCHES", "CONTROL"] as const;
type Tab = (typeof TABS)[number];

export default function App({ mode }: { mode: "sidebar" | "planner" | "detail" }) {
  useEffect(() => initStore(), []);
  const state = useConductor();
  const [tab, setTab] = useState<Tab>("QUEUE");
  const stripRef = useRef<HTMLDivElement>(null);

  if (mode === "planner") {
    return (
      <div className="ac-scroll">
        <BatchPlanner />
      </div>
    );
  }
  if (mode === "detail") {
    return (
      <div className="ac-scroll">
        <BatchDetail />
      </div>
    );
  }

  // Roving tabindex: exactly one tab is reachable with Tab, arrows move between
  // them. The previous implementation called .blur() then .focus() by hand.
  const focusTab = (next: Tab) => {
    setTab(next);
    requestAnimationFrame(() => {
      stripRef.current?.querySelector<HTMLButtonElement>(`[data-tab="${next}"]`)?.focus();
    });
  };

  return (
    <div className="ac-app">
      <div className="ac-tabs" role="tablist" aria-label="Agent Conductor sections" ref={stripRef}>
        {TABS.map((t) => (
          <button
            key={t}
            data-tab={t}
            role="tab"
            id={`ac-tab-${t}`}
            aria-selected={tab === t}
            aria-controls="ac-tabpanel"
            tabIndex={tab === t ? 0 : -1}
            className="ac-tab"
            onClick={() => setTab(t)}
            onKeyDown={(e) => {
              const i = TABS.indexOf(tab);
              if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
                e.preventDefault();
                focusTab(TABS[(i + (e.key === "ArrowRight" ? 1 : TABS.length - 1)) % TABS.length]);
              } else if (e.key === "Home") {
                e.preventDefault();
                focusTab(TABS[0]);
              } else if (e.key === "End") {
                e.preventDefault();
                focusTab(TABS[TABS.length - 1]);
              }
            }}
          >
            {t}
          </button>
        ))}
      </div>
      <div
        className="ac-scroll ac-grow"
        id="ac-tabpanel"
        role="tabpanel"
        aria-labelledby={`ac-tab-${tab}`}
        tabIndex={-1}
      >
        {tab === "QUEUE" && <QueueView state={state} />}
        {tab === "BATCHES" && <BatchesView state={state} />}
        {tab === "CONTROL" && <ControlView state={state} />}
      </div>
    </div>
  );
}
