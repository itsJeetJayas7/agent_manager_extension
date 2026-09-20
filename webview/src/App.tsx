import React, { useEffect } from "react";
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
  const [tab, setTab] = React.useState<Tab>("QUEUE");

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

  return (
    <div style={{ display: "flex", flexDirection: "column", height: "100%" }}>
      <div className="ac-tabs" role="tablist" aria-label="Agent Conductor sections">
        {TABS.map((t) => (
          <button
            key={t}
            role="tab"
            aria-selected={tab === t}
            className="ac-tab"
            onClick={() => setTab(t)}
            onKeyDown={(e) => {
              if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
                const i = TABS.indexOf(tab);
                const next = (i + (e.key === "ArrowRight" ? 1 : TABS.length - 1)) % TABS.length;
                setTab(TABS[next]);
                (e.target as HTMLElement).blur();
                document.querySelectorAll<HTMLButtonElement>('[role="tab"]')[next]?.focus();
                e.preventDefault();
              }
            }}
          >
            {t}
          </button>
        ))}
      </div>
      <div className="ac-scroll" style={{ flex: 1 }} role="tabpanel" aria-label={tab}>
        {tab === "QUEUE" && <QueueView state={state} />}
        {tab === "BATCHES" && <BatchesView state={state} />}
        {tab === "CONTROL" && <ControlView state={state} />}
      </div>
    </div>
  );
}
