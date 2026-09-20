// Queue tab: header, pause, active run, composer, the queue itself.
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ConductorState } from "../state/store";
import { activeRuns, waitingRuns, attentionRuns, pausedRuns } from "../state/store";
import type { RunLike } from "../../../src/extension/protocol";
import StatusGlyph from "../components/StatusGlyph";
import { PauseButton, PauseBand } from "../components/PauseControl";
import { useNow } from "../components/ticker";
import Onboarding, { useSetupStatus } from "../components/Onboarding";
import { fmtDuration, elapsedSince, fmtRelative } from "../components/format";
import {
  rpc,
  confirm,
  openClaudeSession,
  openCodexThread,
  openWorktree,
  openPlanner,
  vscodeApi,
} from "../api/client";

function ProviderDot({ provider, state }: { provider: string; state: ConductorState }) {
  const q = state.snapshot?.quota?.[provider];
  const caps = state.snapshot?.providers?.[provider];
  const busyExternal = (state.snapshot?.external?.[provider] ?? []).some(
    (s) => s.kind === "external" && ["SessionStart", "UserPromptSubmit"].includes(s.lastEvent)
  );
  let cls = "ok";
  let title = `${provider}: ready`;
  if (!caps?.installed) {
    cls = "err";
    title = `${provider}: runtime not found`;
  } else if (q?.blocked) {
    cls = "warn";
    title = `${provider}: quota exhausted`;
  } else if (busyExternal) {
    cls = "warn";
    title = `${provider}: a session is running outside Agent Conductor`;
  }
  return (
    <span className="ac-row ac-gap-1" title={title}>
      <span className="ac-cap">{provider}</span>
      <span className={`ac-dot ${cls}`} aria-hidden="true" />
      <span className="ac-sr">{title}</span>
    </span>
  );
}

// ---- composer -------------------------------------------------------------

interface ComposerPrefs {
  provider: string;
  model: string;
  effort: string;
  permission: string;
  staged: boolean;
  attachSecrets: boolean;
}

const DEFAULT_PREFS: ComposerPrefs = {
  provider: "claude",
  model: "",
  effort: "high",
  permission: "bypass",
  staged: false,
  attachSecrets: false,
};

/**
 * Composer settings persist per workspace through the webview's own state, so
 * the user never re-picks provider/model/effort/permission.
 */
function loadPrefs(workspace: string): ComposerPrefs {
  try {
    const all = vscodeApi.getState()?.composer ?? {};
    return { ...DEFAULT_PREFS, ...(all[workspace] ?? {}) };
  } catch {
    return { ...DEFAULT_PREFS };
  }
}

function savePrefs(workspace: string, prefs: ComposerPrefs): void {
  try {
    const st = vscodeApi.getState() ?? {};
    vscodeApi.setState({ ...st, composer: { ...(st.composer ?? {}), [workspace]: prefs } });
  } catch {
    // webview state is a convenience, never a requirement
  }
}

function Composer({ state }: { state: ConductorState }) {
  const workspace = state.workspace || "default";
  const [prefs, setPrefs] = useState<ComposerPrefs>(() => loadPrefs(workspace));
  const [prompt, setPrompt] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const hasSecret = !!state.snapshot?.hasSecretBlock;

  // Re-read when the workspace changes (a new window, a different folder).
  useEffect(() => setPrefs(loadPrefs(workspace)), [workspace]);

  const caps = state.snapshot?.providers?.[prefs.provider];
  const models = useMemo(() => (caps?.models ?? []).filter((m) => !m.hidden), [caps]);
  const efforts = models.find((m) => m.id === prefs.model)?.efforts ?? ["low", "medium", "high"];

  const update = useCallback(
    (patch: Partial<ComposerPrefs>) => {
      setPrefs((cur) => {
        const next = { ...cur, ...patch };
        savePrefs(workspace, next);
        return next;
      });
    },
    [workspace]
  );

  // Keep the model valid for the selected provider without clobbering a choice.
  useEffect(() => {
    if (models.length === 0) {
      return;
    }
    if (!models.some((m) => m.id === prefs.model)) {
      const preferred =
        prefs.provider === "claude" ? models.find((m) => m.id.includes("sonnet")) : undefined;
      update({ model: (preferred ?? models[0]).id });
    }
  }, [models, prefs.model, prefs.provider, update]);

  useEffect(() => {
    if (!hasSecret && prefs.attachSecrets) {
      update({ attachSecrets: false });
    }
  }, [hasSecret, prefs.attachSecrets, update]);

  const enqueue = async (runNext: boolean) => {
    if (!prompt.trim()) {
      setError("Paste a prompt first.");
      return;
    }
    setBusy(true);
    setError("");
    try {
      await rpc("enqueueRun", { prompt, ...prefs, runNext });
      setPrompt("");
    } catch (e: any) {
      setError(String(e?.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="ac-section" aria-label="Queue a run">
      <div className="ac-section-title">Queue a run</div>
      <label className="ac-sr" htmlFor="ac-prompt">
        Prompt
      </label>
      <textarea
        id="ac-prompt"
        className="ac-textarea"
        value={prompt}
        placeholder="Paste the task exactly as the agent should receive it. It is sent verbatim."
        onChange={(e) => setPrompt(e.target.value)}
        onKeyDown={(e) => {
          if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
            e.preventDefault();
            void enqueue(false);
          }
        }}
        aria-describedby="ac-prompt-help"
      />
      <div className="ac-affordance ac-mt-1" id="ac-prompt-help">
        <span>
          <span className="ac-kbd">⌘⏎</span> to queue
        </span>
        <span className="ac-sep" aria-hidden="true">
          ·
        </span>
        <span>{caps?.installed ? caps.version || "installed" : "runtime missing"}</span>
      </div>

      <div className="ac-row between ac-mt-3">
        <div className="ac-seg" role="group" aria-label="Provider">
          {["claude", "codex"].map((p) => (
            <button key={p} aria-pressed={prefs.provider === p} onClick={() => update({ provider: p })}>
              {p === "claude" ? "Claude" : "Codex"}
            </button>
          ))}
        </div>
      </div>

      <div className="ac-inline-controls ac-mt-2">
        <div>
          <label className="ac-label" htmlFor="ac-model">
            Model
          </label>
          <select
            id="ac-model"
            className="ac-select"
            value={prefs.model}
            onChange={(e) => update({ model: e.target.value })}
          >
            {models.length === 0 && <option value="">unavailable</option>}
            {models.map((m) => (
              <option key={m.id} value={m.id}>
                {m.name || m.id}
              </option>
            ))}
          </select>
        </div>
        <div>
          <label className="ac-label" htmlFor="ac-effort">
            Effort
          </label>
          <select
            id="ac-effort"
            className="ac-select"
            value={prefs.effort}
            onChange={(e) => update({ effort: e.target.value })}
          >
            {efforts.map((f) => (
              <option key={f} value={f}>
                {f}
              </option>
            ))}
          </select>
        </div>
        <div>
          <label className="ac-label" htmlFor="ac-perm">
            Permission
          </label>
          <select
            id="ac-perm"
            className="ac-select"
            value={prefs.permission}
            onChange={(e) => update({ permission: e.target.value })}
          >
            <option value="bypass">Bypass</option>
            <option value="acceptEdits">Accept edits</option>
            <option value="default">Ask</option>
            <option value="plan">Plan only</option>
          </select>
        </div>
      </div>

      <div className="ac-mt-2">
        <label className="ac-check">
          <input
            type="checkbox"
            checked={prefs.attachSecrets}
            disabled={!hasSecret}
            onChange={(e) => update({ attachSecrets: e.target.checked })}
          />
          <span>
            Attach Shared Testing &amp; Sudo Block
            {!hasSecret && <span className="ac-muted"> — none saved yet</span>}
          </span>
        </label>
        <label className="ac-check">
          <input type="checkbox" checked={prefs.staged} onChange={(e) => update({ staged: e.target.checked })} />
          <span>Staged Intelligence — strong plan, cheaper build</span>
        </label>
      </div>

      {error && (
        <div className="ac-banner err ac-mt-2" role="alert">
          {error}
        </div>
      )}

      <div className="ac-row ac-mt-3">
        <button className="ac-btn" disabled={busy} onClick={() => void enqueue(false)}>
          Queue run
        </button>
        <button className="ac-btn ghost" disabled={busy} onClick={() => void enqueue(true)}>
          Run next
        </button>
      </div>
    </section>
  );
}

// ---- the hero -------------------------------------------------------------

function stageLabel(run: RunLike): string | null {
  if (!run.staged) {
    return null;
  }
  const stage = (run.stage || "").toUpperCase();
  if (stage === "PLAN") {
    return "Plan → Build";
  }
  if (stage === "BUILD") {
    return "Plan ✓ → Build";
  }
  if (stage === "AUDIT") {
    return "Build ✓ → Audit";
  }
  return "Staged";
}

function ActiveRun({ run }: { run: RunLike }) {
  const now = useNow();
  const openLabel = run.provider === "claude" ? "Open in Claude Code" : "Open in Codex";
  const open = () =>
    run.provider === "claude"
      ? openClaudeSession(run.providerSessionId ?? "")
      : openCodexThread(run.providerSessionId ?? "", run.providerTitle || run.title);
  const stage = stageLabel(run);
  return (
    <article className="ac-hero" aria-label={`Active run: ${run.title || run.promptPreview}`}>
      <div className="ac-row between top">
        <span className="ac-hero-title ac-grow">{run.title || run.promptPreview}</span>
        <StatusGlyph state={run.state} pulse={run.state === "RUNNING" || run.state === "WAITING_FOR_PROVIDER"} />
      </div>
      <div className="ac-hero-meta">
        <span className="ac-hero-elapsed ac-num">{fmtDuration(elapsedSince(run.startedAt, now))}</span>
        <span className="ac-cap">{run.provider}</span>
        {run.model && <span>{run.model}</span>}
        {stage && (
          <span className="ac-stage">
            <b>{stage}</b>
          </span>
        )}
        {run.continuationCount > 0 && <span>{run.continuationCount} continuation{run.continuationCount === 1 ? "" : "s"}</span>}
      </div>
      {/* A hairline, not a progress bar: we do not know a percentage and will
          not invent one. It only signals that work is moving. */}
      {run.state === "RUNNING" && (
        <div className="ac-progress" aria-hidden="true">
          <div className="ac-pulse ac-w-full" />
        </div>
      )}
      <div className="ac-row ac-gap-1 ac-mt-3">
        {run.providerSessionId && (
          <button className="ac-btn ghost ac-btn-sm" onClick={open}>
            {openLabel}
          </button>
        )}
        {run.worktreePath && (
          <button className="ac-btn ghost ac-btn-sm" onClick={() => openWorktree(run.worktreePath!)}>
            Worktree
          </button>
        )}
        <button
          className="ac-btn ghost ac-btn-sm"
          onClick={async () => {
            if (
              await confirm("Cancel this run?", {
                detail: "The agent is asked to stop at a safe point. Work already written to disk is kept.",
                confirmLabel: "Cancel run",
              })
            ) {
              void rpc("cancelRun", { runId: run.id }).catch(() => undefined);
            }
          }}
        >
          Cancel
        </button>
      </div>
    </article>
  );
}

// ---- the queue ------------------------------------------------------------

function QueueRow({
  run,
  index,
  isNext,
  paused,
  onMove,
  dragState,
}: {
  run: RunLike;
  index: number;
  isNext: boolean;
  paused: boolean;
  onMove: (runId: string, beforeRunId: string) => void;
  dragState: { dragging: string | null; over: string | null; set: (d: string | null, o: string | null) => void };
}) {
  const when = () => {
    if (run.state === "WAITING_FOR_QUOTA") {
      return <span title="Resumes automatically when quota returns">◷ {fmtRelative(run.nextEligibleAt)}</span>;
    }
    if (isNext) {
      // The queue list shows the consequence of the pause, in plain words.
      return paused ? <span>starts when the queue resumes</span> : <span>next</span>;
    }
    return <StatusGlyph state={run.state} />;
  };
  return (
    <li
      className={`ac-qrow${isNext ? " next" : ""}${paused ? " dimmed" : ""}`}
      draggable
      data-dragging={dragState.dragging === run.id}
      data-dropbefore={dragState.over === run.id && dragState.dragging !== run.id}
      onDragStart={(e) => {
        dragState.set(run.id, null);
        e.dataTransfer.effectAllowed = "move";
        e.dataTransfer.setData("text/plain", run.id);
      }}
      onDragOver={(e) => {
        e.preventDefault();
        e.dataTransfer.dropEffect = "move";
        if (dragState.over !== run.id) {
          dragState.set(dragState.dragging, run.id);
        }
      }}
      onDrop={(e) => {
        e.preventDefault();
        const source = e.dataTransfer.getData("text/plain") || dragState.dragging;
        dragState.set(null, null);
        if (source && source !== run.id) {
          onMove(source, run.id);
        }
      }}
      onDragEnd={() => dragState.set(null, null)}
      tabIndex={0}
      onKeyDown={(e) => {
        // Keyboard equivalent of dragging.
        if (e.altKey && (e.key === "ArrowUp" || e.key === "ArrowDown")) {
          e.preventDefault();
          onMove(run.id, e.key === "ArrowUp" ? "__up__" : "__down__");
        }
      }}
      aria-label={`Position ${index + 1}: ${run.title || run.promptPreview}, ${run.state}. Alt plus arrow keys to reorder.`}
    >
      <span className="ac-qrow-index ac-num" aria-hidden="true">
        {index + 1}
      </span>
      <span className="ac-qrow-title">
        <span className="ac-trunc ac-block">
          {run.title || run.promptPreview}
        </span>
        <span className="ac-qrow-sub ac-cap">
          {run.provider}
          {run.model ? ` · ${run.model}` : ""}
        </span>
      </span>
      <span className="ac-qrow-when">{when()}</span>
      <span className="ac-qrow-actions">
        <button
          className="ac-icon-btn"
          title="Run next"
          aria-label={`Move ${run.title || "run"} to the front of the queue`}
          onClick={() => onMove(run.id, "__front__")}
        >
          ↑
        </button>
        <button
          className="ac-icon-btn"
          title="Cancel"
          aria-label={`Cancel ${run.title || "run"}`}
          onClick={async () => {
            if (await confirm("Remove this run from the queue?", { confirmLabel: "Remove" })) {
              void rpc("cancelRun", { runId: run.id }).catch(() => undefined);
            }
          }}
        >
          ✕
        </button>
      </span>
    </li>
  );
}

// ---- the view -------------------------------------------------------------

export default function QueueView({ state }: { state: ConductorState }) {
  const active = activeRuns(state);
  const waiting = waitingRuns(state);
  const attention = attentionRuns(state);
  const parked = pausedRuns(state);
  const pause = state.snapshot?.pause ?? null;
  const paused = !!pause?.paused;
  const setup = useSetupStatus();
  const [drag, setDrag] = useState<{ dragging: string | null; over: string | null }>({
    dragging: null,
    over: null,
  });
  const dragState = useRef({
    dragging: null as string | null,
    over: null as string | null,
    set: (d: string | null, o: string | null) => setDrag({ dragging: d, over: o }),
  });
  dragState.current.dragging = drag.dragging;
  dragState.current.over = drag.over;

  const move = useCallback(
    (runId: string, target: string) => {
      const order = waiting.map((r) => r.id);
      const from = order.indexOf(runId);
      if (target === "__front__") {
        const first = order.find((id) => id !== runId);
        void rpc("reorderQueue", { runId, beforeRunId: first ?? "" }).catch(() => undefined);
        return;
      }
      if (target === "__up__") {
        if (from <= 0) {
          return;
        }
        void rpc("reorderQueue", { runId, beforeRunId: order[from - 1] }).catch(() => undefined);
        return;
      }
      if (target === "__down__") {
        if (from < 0 || from >= order.length - 1) {
          return;
        }
        const after = order[from + 2];
        void rpc("reorderQueue", { runId, beforeRunId: after ?? "" }).catch(() => undefined);
        return;
      }
      void rpc("reorderQueue", { runId, beforeRunId: target }).catch(() => undefined);
    },
    [waiting]
  );

  return (
    <div>
      <header className="ac-header">
        <div className="ac-header-top">
          <div>
            <div className="ac-title">Agent Conductor</div>
            <div className="ac-subrow">
              <span>{state.host || "Local"}</span>
              <span className="ac-sep" aria-hidden="true">
                ·
              </span>
              <span className={state.daemonHealthy ? "" : "ac-glyph failed"}>
                {state.daemonHealthy ? `daemon ${state.snapshot?.daemon?.version ?? ""}` : "daemon offline"}
              </span>
            </div>
            <div className="ac-subrow ac-gap-3">
              <ProviderDot provider="claude" state={state} />
              <ProviderDot provider="codex" state={state} />
            </div>
          </div>
          <PauseButton pause={pause} />
        </div>
      </header>

      {setup && <Onboarding status={setup} />}

      {paused && pause && <PauseBand pause={pause} waitingCount={waiting.length} />}

      {!state.daemonHealthy && (
        <div className="ac-banner err ac-band-pad" role="status">
          <div>{state.daemonProblem?.detail ?? "The Agent Conductor daemon is not running."}</div>
          {state.daemonProblem?.remedy && (
            <div className="ac-mono ac-xs ac-mt-1 ac-wrap">{state.daemonProblem.remedy}</div>
          )}
          <div className="ac-xs ac-muted ac-mt-1">
            Runs only continue while the daemon is running — Command Palette → “Agent Conductor: Install
            Background Service”.
          </div>
        </div>
      )}

      {attention.length > 0 && (
        <section className="ac-section" aria-label="Needs attention">
          <div className="ac-section-title">Attention</div>
          <div className="ac-stack-2">
            {attention.slice(0, 4).map((r) => (
              <div key={r.id} className="ac-card">
                <div className="ac-row between">
                  <span className="ac-trunc ac-grow">{r.title || r.promptPreview}</span>
                  <StatusGlyph state={r.state} />
                </div>
                {r.errorMsg && <div className="ac-small ac-muted ac-mt-1 ac-wrap">{r.errorMsg}</div>}
                <div className="ac-row ac-gap-1 ac-mt-2 wrap">
                  {r.errorCode === "WORKSPACE_UNTRUSTED" && (
                    <button
                      className="ac-btn ac-btn-sm"
                      onClick={() =>
                        void rpc("trustWorkspace", { path: r.cwd }).then(() => rpc("continueRun", { runId: r.id }))
                      }
                    >
                      Trust workspace
                    </button>
                  )}
                  <button className="ac-btn ac-btn-sm" onClick={() => void rpc("continueRun", { runId: r.id })}>
                    Continue
                  </button>
                  <button className="ac-btn ghost ac-btn-sm" onClick={() => void rpc("retryRun", { runId: r.id })}>
                    Retry
                  </button>
                  <button
                    className="ac-btn ghost ac-btn-sm"
                    onClick={async () => {
                      if (
                        await confirm("Skip this run?", {
                          detail: "It will be marked SKIPPED and its batch continues without it.",
                          confirmLabel: "Skip run",
                        })
                      ) {
                        void rpc("skipRun", { runId: r.id });
                      }
                    }}
                  >
                    Skip
                  </button>
                </div>
              </div>
            ))}
          </div>
        </section>
      )}

      <section className="ac-section" aria-label="Active run">
        <div className="ac-section-title">Active</div>
        {active.length === 0 && parked.length === 0 && (
          <div className="ac-empty">
            <div className="ac-empty-title">Nothing is running.</div>
            <div className="ac-empty-hint">
              {paused ? "The queue is paused." : "Queue a task below and it starts straight away."}
            </div>
          </div>
        )}
        <div className="ac-stack-2">
          {active.map((r) => (
            <ActiveRun key={r.id} run={r} />
          ))}
        </div>
        {parked.length > 0 && (
          <div className="ac-stack-2 ac-mt-2">
            {parked.map((r) => (
              <div key={r.id} className="ac-card">
                <div className="ac-row between">
                  <span className="ac-trunc ac-grow">{r.title || r.promptPreview}</span>
                  <StatusGlyph state={r.state} />
                </div>
                <div className="ac-xs ac-muted ac-mt-1">
                  Stopped at a safe point. Continues in the same session when the queue resumes.
                </div>
              </div>
            ))}
          </div>
        )}
      </section>

      <Composer state={state} />

      <section aria-label="Queue">
        <div className="ac-section-title ac-list-head">
          Up next
        </div>
        {waiting.length === 0 ? (
          <div className="ac-empty">
            <div className="ac-empty-title">The queue is empty.</div>
            <div className="ac-empty-hint">Anything you queue runs one at a time, in order.</div>
          </div>
        ) : (
          <ul className="ac-qlist ac-reset-list">
            {waiting.map((r, i) => (
              <QueueRow
                key={r.id}
                run={r}
                index={i}
                isNext={i === 0}
                paused={paused}
                onMove={move}
                dragState={dragState.current}
              />
            ))}
          </ul>
        )}
      </section>

      <div className="ac-footer">
        <button className="ac-btn" onClick={() => document.getElementById("ac-prompt")?.focus()}>
          Queue a run
        </button>
        <button className="ac-btn ghost" onClick={() => openPlanner()}>
          Build a batch
        </button>
      </div>
    </div>
  );
}
