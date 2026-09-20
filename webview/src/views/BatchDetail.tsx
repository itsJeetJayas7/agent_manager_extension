// Batch detail: deterministic analysis, run cards, timeline, report.
import { useEffect, useState } from "react";
import { detailBatchId } from "../state/store";
import { rpc, confirm, notify, openClaudeSession, openCodexThread, openWorktree, copyText } from "../api/client";
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
        <h1 className="ac-display ac-h1">{b.title}</h1>
        <StatusGlyph state={b.status === "COMPLETED" ? "COMPLETED" : b.status === "ATTENTION" ? "NEEDS_ATTENTION" : "RUNNING"} />
      </div>
      <div className="ac-small ac-muted ac-mt-1 ac-mb-3">
        {done}/{runs.length} complete · {b.startedAt ? `elapsed ${fmtDuration(elapsedSince(b.startedAt))}` : "not started"}
        {` · continuations ${quotaWaits} · failed ${failed} · blocked ${blocked}`}
      </div>

      <div className="ac-row ac-gap-1 wrap ac-mb-3">
        <button className="ac-btn ac-btn-sm" onClick={async () => {
          const r = await rpc<any>("batchReport", { batchId });
          copyText(r.report);
          setReport(r.report);
        }}>Copy Batch Report</button>
        {b.integrationBranch && (
          <>
            {/* window.confirm and window.alert do nothing inside a sandboxed
                webview, so this button silently merged — or silently failed. */}
            <button className="ac-btn ac-btn-sm" onClick={async () => {
              const okToApply = await confirm(
                `Merge ${b.integrationBranch} into ${b.baseBranch}?`,
                {
                  detail: "Your branch must be clean and unchanged since the batch started. A safety ref is taken first, so you can always get back.",
                  confirmLabel: "Merge",
                }
              );
              if (!okToApply) {
                return;
              }
              try {
                const r = await rpc<{ output: string }>("applyBatch", { batchId });
                notify(`Applied to ${b.baseBranch}. ${r.output.split("\n").pop() ?? ""}`);
              } catch (e: any) {
                notify(`Apply failed: ${e?.message ?? e}`, "error");
              }
            }}>Apply batch to current branch</button>
            <button className="ac-btn ghost ac-btn-sm" onClick={() => copyText(`git merge --no-ff --no-edit ${b.integrationBranch}`)}>
              Copy merge command
            </button>
          </>
        )}
        {b.integrationWorktreePath && (
          <button className="ac-btn ghost ac-btn-sm" onClick={() => openWorktree(b.integrationWorktreePath)}>
            Open integration worktree
          </button>
        )}
        {b.status === "COMPLETED" && !b.applied && (
          <button className="ac-btn ghost ac-btn-sm" onClick={async () => {
            if (await confirm("Clean up this batch's worktrees?", {
              detail: "Branches holding unmerged commits are kept, and you are told which.",
              confirmLabel: "Clean up",
            })) {
              try {
                await rpc("cleanupWorktrees", { batchId });
                notify("Worktrees cleaned up.");
              } catch (e: any) {
                notify(String(e?.message ?? e), "warning");
              }
            }
          }}>
            Clean up worktrees
          </button>
        )}
      </div>
      {report && (
        <details className="ac-card">
          <summary className="ac-small ac-clickable">Report preview</summary>
          <pre className="ac-mono ac-small ac-pre">{report}</pre>
        </details>
      )}

      {(detail.rows ?? []).map((row: any) => {
        const rowRuns = runs.filter((r: any) => r.rowId === row.id);
        return (
          <section key={row.id} className="ac-mb-3" aria-label={`Row ${row.position + 1} (${row.mode})`}>
            <div className="ac-row ac-mb-2">
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
      <dl className="ac-kv ac-mt-2">
        <dt>Provider</dt><dd className="ac-cap">{run.provider} · {run.model || "default"} · {run.effort || "default effort"}</dd>
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
      <div className="ac-row wrap ac-gap-1 ac-mt-2">
        {run.providerSessionId && (
          <button className="ac-btn ghost ac-btn-sm" onClick={openNative}>
            {run.provider === "claude" ? "Open in Claude Code" : "Open in Codex"}
          </button>
        )}
        {run.worktreePath && <button className="ac-btn ghost ac-btn-sm" onClick={() => openWorktree(run.worktreePath)}>Open worktree</button>}
        <button className="ac-btn ghost ac-btn-sm" onClick={() => rpc("revealEffectivePrompt", { runId: run.id }).then((r: any) => {
          copyText(r.text, !!r.containsSecrets);
        })}>Copy effective prompt</button>
        {["FAILED", "NEEDS_ATTENTION", "ORPHANED", "BLOCKED"].includes(run.state) && (
          <>
            <button className="ac-btn ac-btn-sm" onClick={() => void rpc("continueRun", { runId: run.id })}>Continue</button>
            <button className="ac-btn ghost ac-btn-sm" onClick={() => void rpc("retryRun", { runId: run.id })}>Retry</button>
            <button className="ac-btn ghost ac-btn-sm" onClick={async () => {
              if (await confirm("Skip this run?", { detail: "It is marked SKIPPED and the batch continues without it.", confirmLabel: "Skip run" })) {
                void rpc("skipRun", { runId: run.id });
              }
            }}>Skip</button>
          </>
        )}
        {["COMPLETED", "FAILED", "CANCELLED", "SKIPPED", "BLOCKED"].includes(run.state) && (
          <button
            className="ac-btn ghost ac-btn-sm"
            title="Queue a new run with the same prompt and settings"
            onClick={() =>
              void rpc("requeueRun", { runId: run.id })
                .then(() => notify("Queued again."))
                .catch((e: any) => notify(String(e?.message ?? e), "error"))
            }
          >
            Run it again
          </button>
        )}
        {["RUNNING", "STARTING", "PREPARING"].includes(run.state) && (
          <button className="ac-btn ac-btn-sm danger" onClick={() => void rpc("cancelRun", { runId: run.id })}>Cancel</button>
        )}
        <button className="ac-btn ghost ac-btn-sm" onClick={() => setShowTimeline(!showTimeline)} aria-expanded={showTimeline}>
          Timeline ({events.length})
        </button>
      </div>
      {showTimeline && (
        <ul className="ac-timeline ac-mt-2">
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
