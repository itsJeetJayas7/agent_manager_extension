// Batches tab: compact batch list with filters and progress.
import { useState } from "react";
import type { ConductorState } from "../state/store";
import { runsOfBatch } from "../state/store";
import { openDetail } from "../api/client";
import { fmtDuration, elapsedSince } from "../components/format";

const FILTERS = ["Active", "Completed", "Attention", "All"] as const;
type Filter = (typeof FILTERS)[number];

function progressOf(state: ConductorState, batchId: string): { done: number; total: number } {
  const runs = runsOfBatch(state, batchId);
  const done = runs.filter((r) => r.state === "COMPLETED" || r.state === "SKIPPED").length;
  return { done, total: runs.length };
}

function statusOf(state: ConductorState, batchId: string, status: string): string {
  const runs = runsOfBatch(state, batchId);
  if (status === "COMPLETED") {
    return "Complete";
  }
  const anyActive = runs.some((r) => ["RUNNING", "STARTING", "PREPARING", "VERIFYING", "AUTO_RESUMING"].includes(r.state));
  if (anyActive) {
    return "Running";
  }
  if (runs.some((r) => ["FAILED", "NEEDS_ATTENTION", "AWAITING_INTEGRATION"].includes(r.state))) {
    return "Attention";
  }
  if (runs.some((r) => ["QUEUED", "WAITING_FOR_SLOT"].includes(r.state))) {
    return "Queued";
  }
  return status.toLowerCase();
}

export default function BatchesView({ state }: { state: ConductorState }) {
  const [filter, setFilter] = useState<Filter>("All");
  const batches = (state.snapshot?.batches ?? []).filter((b) => {
    if (b.implicit) {
      return false;
    }
    const st = statusOf(state, b.id, b.status);
    switch (filter) {
      case "Active":
        return st === "Running" || st === "Queued";
      case "Completed":
        return st === "Complete";
      case "Attention":
        return st === "Attention";
      default:
        return true;
    }
  });

  return (
    <div>
      <header className="ac-header">
        <div className="ac-title">Batches</div>
      </header>
      <div className="ac-tabs" role="tablist" aria-label="Batch filters">
        {FILTERS.map((f) => (
          <button
            key={f}
            role="tab"
            aria-selected={filter === f}
            tabIndex={filter === f ? 0 : -1}
            className="ac-tab"
            onClick={() => setFilter(f)}
          >
            {f}
          </button>
        ))}
      </div>
      {batches.length === 0 && (
        <div className="ac-empty">
          <div className="ac-empty-title">
            {filter === "All" ? "No batches yet." : `No ${filter.toLowerCase()} batches.`}
          </div>
          <div className="ac-empty-hint">A batch runs several tasks in order, or side by side in isolated worktrees.</div>
        </div>
      )}
      {/* <button role="listitem"> inside role="list" is invalid ARIA; a real
          <li> wrapping the control is both valid and simpler. */}
      <ul className="ac-section ac-reset-list ac-stack-2">
        {batches.map((b) => {
          const p = progressOf(state, b.id);
          const pct = p.total > 0 ? Math.round((p.done / p.total) * 100) : 0;
          const st = statusOf(state, b.id, b.status);
          return (
            <li key={b.id}>
              <button
                className="ac-card ac-batch-button"
                onClick={() => openDetail(b.id)}
                aria-label={`Batch ${b.title}, ${p.done} of ${p.total} complete, ${st}`}
              >
                <span className="ac-row between">
                  <span className="ac-trunc ac-grow">{b.title}</span>
                  <span className="ac-small ac-num ac-muted">
                    {p.done}/{p.total}
                  </span>
                </span>
                <span className="ac-progress" aria-hidden="true">
                  <span style={{ width: `${pct}%`, display: "block", height: "100%", background: "var(--ac-accent)" }} />
                </span>
                <span className="ac-small ac-muted ac-row between ac-mt-1">
                  <span>
                    {st}
                    {b.applied ? " · applied" : ""}
                  </span>
                  <span className="ac-num">{b.startedAt ? fmtDuration(elapsedSince(b.startedAt)) : "—"}</span>
                </span>
              </button>
            </li>
          );
        })}
      </ul>
    </div>
  );
}
