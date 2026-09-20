// Batch detail: deterministic analysis, run cards, timeline, report.
import { useEffect, useState } from "react";
import { detailBatchId } from "../state/store";
import { rpc, openClaudeSession, openCodexThread, openWorktree, copyText } from "../api/client";
import StatusGlyph from "../components/StatusGlyph";
import { fmtDuration, elapsedSince } from "../components/format";

interface Detail {
  batch: any;
  rows: any[];
  runs: any[];
  worktrees: any[];
  events: Record<string, any[]>;
}

export default function BatchDetail() {

  const batchId = detailBatchId();
  const [detail, setDetail] = useState<Detail | null>(null);
  const [report, setReport] = useState<string | null>(null);

  useEffect(() => {
    if (!batchId) {
      return;
    }
    let alive = true;
    const load = () => {
      rpc<Detail>("getBatchDetail", { batchId }).then((d) => {
        if (alive) {
          setDetail(d);
        }
      }).catch(() => undefined);
    };
    load();
    const t = setInterval(load, 2500);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [batchId]);

  if (!batchId) {
    return <div className="ac-empty">Select a batch in the Batches tab.</div>;
  }
  if (!detail) {
    return <div className="ac-empty">loading…</div>;
  }

  const b = detail.batch;
  const runs = detail.runs ?? [];
  const done = runs.filter((r: any) => r.state === "COMPLETED" || r.state === "SKIPPED").length;
  const quotaWaits = runs.reduce((n: number, r: any) => n + (r.continuationCount ?? 0), 0);
  const failed = runs.filter((r: any) => ["FAILED", "NEEDS_ATTENTION"].includes(r.state)).length;
  const blocked = runs.filter((r: any) => r.state === "BLOCKED").length;

  return (
    <div className="ac-planner">
      <div className="ac-row between">
        <h1 style={{ fontSize: 16, margin: 0 }}>{b.title}</h1>
        <StatusGlyph state={b.status === "COMPLETED" ? "COMPLETED" : b.status === "ATTENTION" ? "NEEDS_ATTENTION" : "RUNNING"} />
      </div>
      <div className="ac-small ac-muted" style={{ margin: "4px 0 12px" }}>
        {done}/{runs.length} complete · {b.startedAt ? `elapsed ${fmtDuration(elapsedSince(b.startedAt))}` : "not started"}
        {` · continuations ${quotaWaits} · failed ${failed} · blocked ${blocked}`}
      </div>

      <div className="ac-row" style={{ gap: 6, marginBottom: 16, flexWrap: "wrap" }}>
        <button className="ac-btn ac-btn-sm" onClick={async () => {
          const r = await rpc<any>("batchReport", { batchId });
          copyText(r.report);
          setReport(r.report);
        }}>Copy Batch Report</button>
        {b.integrationBranch && (
          <>
            <button className="ac-btn ac-btn-sm" onClick={async () => {
              if (!confirm(`Merge ${b.integrationBranch} into your original branch ${b.baseBranch}? Your branch must be clean and unchanged.`)) {
                return;
              }
              try {
                await rpc("applyBatch", { batchId });
              } catch (e: any) {
                alert(`Apply failed: ${e.message}`);
              }
            }}>Apply Batch to Current Branch</button>
            <button className="ac-btn ac-btn-sm secondary" onClick={() => copyText(`git merge --no-ff --no-edit ${b.integrationBranch}`)}>
              Copy merge command
            </button>
          </>
        )}
        {b.integrationWorktreePath && (
          <button className="ac-btn ac-btn-sm secondary" onClick={() => openWorktree(b.integrationWorktreePath)}>
            Open integration worktree
          </button>
        )}
        {b.status === "COMPLETED" && !b.applied && (
          <button className="ac-btn ac-btn-sm secondary" onClick={() => void rpc("cleanupWorktrees", { batchId })}>
            Clean up worktrees
          </button>
        )}
      </div>
      {report && (
        <details className="ac-card">
          <summary className="ac-small" style={{ cursor: "pointer" }}>Report preview</summary>
          <pre className="ac-mono ac-small" style={{ whiteSpace: "pre-wrap" }}>{report}</pre>
        </details>
      )}

      {(detail.rows ?? []).map((row: any) => {
        const rowRuns = runs.filter((r: any) => r.rowId === row.id);
        return (
          <section key={row.id} style={{ marginBottom: 16 }} aria-label={`Row ${row.position + 1} (${row.mode})`}>
            <div className="ac-row" style={{ gap: 8, marginBottom: 6 }}>
              <span className="ac-rownum ac-num">{row.position + 1}</span>
              <span className="ac-small ac-muted">{row.mode}</span>
              <StatusGlyph state={row.status === "COMPLETED" ? "COMPLETED" : row.status === "RUNNING" ? "RUNNING" : row.status === "BLOCKED" ? "BLOCKED" : "QUEUED"} />
            </div>
            <div className="ac-cards">
              {rowRuns.map((r: any) => <RunCard key={r.id} run={r} events={detail.events?.[r.id] ?? []} />)}
            </div>
          </section>
        );
      })}
    </div>
  );
}

function RunCard({ run, events }: { run: any; events: any[] }) {
  const [showTimeline, setShowTimeline] = useState(false);
  const openNative = () =>
    run.provider === "claude"
      ? openClaudeSession(run.providerSessionId)
      : openCodexThread(run.providerSessionId, run.providerTitle || run.title);
  return (
    <article className="ac-card-edit" aria-label={`Run ${run.title}`}>
      <div className="ac-row between">
        <strong className="ac-trunc ac-grow">{run.title || run.promptPreview}</strong>
        <StatusGlyph state={run.state} pulse={run.state === "RUNNING"} />
      </div>
      <dl className="ac-kv" style={{ marginTop: 6 }}>
        <dt>Provider</dt><dd style={{ textTransform: "capitalize" }}>{run.provider} · {run.model || "default"} · {run.effort || "default effort"}</dd>
        {run.staged && <><dt>Stage</dt><dd>{run.stage || "PLAN → BUILD"}</dd></>}
        {run.continuationCount > 0 && <><dt>Continuations</dt><dd className="ac-num">{run.continuationCount}</dd></>}
        {run.branch && <><dt>Branch</dt><dd className="ac-mono ac-small">{run.branch}</dd></>}
        {run.providerSessionId && (
          <>
            <dt>Session</dt>
            <dd className="ac-mono ac-small">{run.providerSessionId.slice(0, 18)}…</dd>
          </>
        )}
        {run.finalOutcome && <><dt>Outcome</dt><dd>{run.finalOutcome}</dd></>}
        {run.errorMsg && <><dt>Issue</dt><dd>{run.errorMsg}</dd></>}
      </dl>
      <div className="ac-row" style={{ marginTop: 8, flexWrap: "wrap", gap: 6 }}>
        {run.providerSessionId && (
          <button className="ac-btn ac-btn-sm secondary" onClick={openNative}>
            {run.provider === "claude" ? "Open in Claude Code" : "Open in Codex"}
          </button>
        )}
        {run.worktreePath && <button className="ac-btn ac-btn-sm secondary" onClick={() => openWorktree(run.worktreePath)}>Open worktree</button>}
        <button className="ac-btn ac-btn-sm secondary" onClick={() => rpc("revealEffectivePrompt", { runId: run.id }).then((r: any) => {
          copyText(r.text, !!r.containsSecrets);
        })}>Copy effective prompt</button>
        {["FAILED", "NEEDS_ATTENTION", "ORPHANED", "BLOCKED"].includes(run.state) && (
          <>
            <button className="ac-btn ac-btn-sm" onClick={() => void rpc("continueRun", { runId: run.id })}>Continue</button>
            <button className="ac-btn ac-btn-sm secondary" onClick={() => void rpc("retryRun", { runId: run.id })}>Retry</button>
            <button className="ac-btn ac-btn-sm secondary" onClick={() => confirm("Skip this run?") && rpc("skipRun", { runId: run.id })}>Skip</button>
          </>
        )}
        {["RUNNING", "STARTING", "PREPARING"].includes(run.state) && (
          <button className="ac-btn ac-btn-sm danger" onClick={() => void rpc("cancelRun", { runId: run.id })}>Cancel</button>
        )}
        <button className="ac-btn ac-btn-sm secondary" onClick={() => setShowTimeline(!showTimeline)} aria-expanded={showTimeline}>
          Timeline ({events.length})
        </button>
      </div>
      {showTimeline && (
        <ul className="ac-timeline" style={{ marginTop: 8 }}>
          {events.map((e: any) => (
            <li key={e.id}>
              <time>{new Date(e.timestamp).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })}</time>
              <span>{e.type.replace(/_/g, " ").toLowerCase()}{e.data ? <span className="ac-muted"> {e.data}</span> : undefined}</span>
            </li>
          ))}
          {events.length === 0 && <li><span className="ac-muted">no events</span></li>}
        </ul>
      )}
    </article>
  );
}
