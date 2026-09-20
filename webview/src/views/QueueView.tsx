// Queue tab: header, provider status, composer, active run, waiting list.
import { useEffect, useMemo, useState } from "react";
import type { ConductorState } from "../state/store";
import { activeRuns, waitingRuns, attentionRuns } from "../state/store";
import StatusGlyph from "../components/StatusGlyph";
import { fmtDuration, elapsedSince, fmtRelative } from "../components/format";
import { rpc, openClaudeSession, openCodexThread, openWorktree, openPlanner } from "../api/client";

function ProviderDot({ provider, state }: { provider: string; state: ConductorState }) {
  const q = state.snapshot?.quota?.[provider];
  const caps = state.snapshot?.providers?.[provider];
  const busyExternal = (state.snapshot?.external?.[provider] ?? []).some(
    (s) => s.kind === "external" && ["SessionStart", "UserPromptSubmit"].includes(s.lastEvent)
  );
  let cls = "ok";
  let title = `${provider}: ok`;
  if (!caps?.installed) {
    cls = "err";
    title = `${provider}: runtime not found`;
  } else if (q?.blocked) {
    cls = "warn";
    title = `${provider}: quota exhausted`;
  } else if (busyExternal) {
    cls = "warn";
    title = `${provider}: external session active`;
  }
  return (
    <span className="ac-row ac-small ac-muted" title={title}>
      <span style={{ textTransform: "capitalize" }}>{provider}</span>
      <span className={`ac-dot ${cls}`} aria-hidden="true" />
      <span className="ac-sr">{title}</span>
    </span>
  );
}

function Composer({ state }: { state: ConductorState }) {
  const [prompt, setPrompt] = useState("");
  const [provider, setProvider] = useState("claude");
  const caps = state.snapshot?.providers?.[provider];
  const models = useMemo(() => (caps?.models ?? []).filter((m) => !m.hidden), [caps]);
  const [model, setModel] = useState("");
  const efforts = models.find((m) => m.id === model)?.efforts ?? ["low", "medium", "high"];
  const [effort, setEffort] = useState("high");
  const [permission, setPermission] = useState("bypass");
  const [attachSecrets, setAttachSecrets] = useState(false);
  const [staged, setStaged] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [secretExists, setSecretExists] = useState<boolean | null>(null);

  useEffect(() => {
    if (!model && models.length > 0) {
      const preferred = provider === "claude" ? models.find((m) => m.id.includes("sonnet")) : models[0];
      setModel(preferred?.id ?? models[0].id);
    }
  }, [models, model, provider]);
  useEffect(() => {
    if (!models.find((m) => m.id === model) && models.length > 0) {
      setModel(models[0].id);
    }
  }, [models, model]);
  useEffect(() => {
    rpc("revealSecretBlock", {}).then(() => setSecretExists(true)).catch(() => setSecretExists(false));
  }, []);

  const enqueue = async (runNext: boolean) => {
    if (!prompt.trim()) {
      setError("Paste a prompt first.");
      return;
    }
    setBusy(true);
    setError("");
    try {
      await rpc("enqueueRun", {
        prompt,
        provider,
        model,
        effort,
        permission,
        attachSecrets,
        staged,
        runNext,
      });
      setPrompt("");
    } catch (e: any) {
      setError(String(e.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="ac-section" aria-label="Queue composer">
      <div className="ac-section-title">Queue run</div>
      <label className="ac-sr" htmlFor="ac-prompt">Prompt</label>
      <textarea
        id="ac-prompt"
        className="ac-textarea"
        value={prompt}
        placeholder="Paste the task exactly as the agent should receive it…"
        onChange={(e) => setPrompt(e.target.value)}
        onKeyDown={(e) => {
          if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
            e.preventDefault();
            void enqueue(false);
          }
        }}
        aria-describedby="ac-prompt-help"
      />
      <div className="ac-small ac-muted" id="ac-prompt-help" style={{ marginTop: 2 }}>
        ⌘⏎ queues · Enter inserts a newline
      </div>

      <div className="ac-row" style={{ marginTop: 8, justifyContent: "space-between" }}>
        <div className="ac-seg" role="group" aria-label="Provider">
          {["claude", "codex"].map((p) => (
            <button key={p} aria-pressed={provider === p} onClick={() => setProvider(p)}>
              {p === "claude" ? "Claude" : "Codex"}
            </button>
          ))}
        </div>
        <span className="ac-small ac-muted">
          {caps?.installed ? caps.version || "installed" : "runtime missing"}
        </span>
      </div>

      <div className="ac-row" style={{ marginTop: 8, gap: 8 }}>
        <div className="ac-grow">
          <label className="ac-label" htmlFor="ac-model">Model</label>
          <select id="ac-model" className="ac-select" value={model} onChange={(e) => setModel(e.target.value)}>
            {models.length === 0 && <option value="">(unavailable)</option>}
            {models.map((m) => (
              <option key={m.id} value={m.id}>{m.name || m.id}</option>
            ))}
          </select>
        </div>
        <div className="ac-grow">
          <label className="ac-label" htmlFor="ac-effort">Effort</label>
          <select id="ac-effort" className="ac-select" value={effort} onChange={(e) => setEffort(e.target.value)}>
            {efforts.map((f) => <option key={f} value={f}>{f}</option>)}
          </select>
        </div>
        <div className="ac-grow">
          <label className="ac-label" htmlFor="ac-perm">Permission</label>
          <select id="ac-perm" className="ac-select" value={permission} onChange={(e) => setPermission(e.target.value)}>
            <option value="bypass">Bypass</option>
            <option value="acceptEdits">Accept edits</option>
            <option value="default">Ask</option>
            <option value="plan">Plan only</option>
          </select>
        </div>
      </div>

      <label className="ac-check">
        <input type="checkbox" checked={attachSecrets} onChange={(e) => setAttachSecrets(e.target.checked)} />
        Attach Shared Testing &amp; Sudo Block
        {secretExists === false && <span className="ac-small ac-muted">(none saved yet)</span>}
      </label>
      <label className="ac-check">
        <input type="checkbox" checked={staged} onChange={(e) => setStaged(e.target.checked)} />
        Staged Intelligence (strong plan → cheaper build)
      </label>

      {error && <div className="ac-banner err" role="alert">{error}</div>}

      <div className="ac-row" style={{ marginTop: 8 }}>
        <button className="ac-btn" disabled={busy} onClick={() => void enqueue(false)}>+ Queue Run</button>
        <button className="ac-btn secondary" disabled={busy} onClick={() => void enqueue(true)}>Run Next</button>
      </div>
    </section>
  );
}

function ActiveRunCard({ run }: { run: any }) {
  const providerTitle = run.provider === "claude" ? "Open in Claude Code" : "Open in Codex";
  const open = () =>
    run.provider === "claude"
      ? openClaudeSession(run.providerSessionId)
      : openCodexThread(run.providerSessionId, run.providerTitle || run.title);
  return (
    <article className={`ac-card active ${run.state === "RUNNING" ? "ac-pulse" : ""}`} aria-label={`Active run: ${run.title}`}>
      <div className="ac-row between">
        <strong className="ac-trunc ac-grow">{run.title || run.promptPreview}</strong>
        <StatusGlyph state={run.state} pulse />
      </div>
      <div className="ac-small ac-muted ac-row" style={{ gap: 10, marginTop: 4 }}>
        <span style={{ textTransform: "capitalize" }}>{run.provider}</span>
        {run.model && <span>{run.model}</span>}
        {run.staged && <span>Plan → Build{run.stage ? ` · ${run.stage}` : ""}</span>}
        <span className="ac-num">{fmtDuration(elapsedSince(run.startedAt))}</span>
      </div>
      <div className="ac-row" style={{ marginTop: 6, gap: 6 }}>
        {run.providerSessionId && (
          <button className="ac-btn ac-btn-sm secondary" onClick={open}>{providerTitle}</button>
        )}
        {run.worktreePath && (
          <button className="ac-btn ac-btn-sm secondary" onClick={() => openWorktree(run.worktreePath)}>Worktree</button>
        )}
        {run.state === "RUNNING" && (
          <button className="ac-btn ac-btn-sm danger" onClick={() => void rpc("cancelRun", { runId: run.id }).catch(() => undefined)}>
            Cancel
          </button>
        )}
      </div>
    </article>
  );
}

function WaitingRow({ run, index }: { run: any; index: number }) {
  return (
    <div className="ac-qrow" role="listitem" aria-label={`${run.title || run.promptPreview}, ${run.state}`}>
      <span className="ac-num ac-muted">{index + 1}</span>
      <span className="ac-small" style={{ textTransform: "capitalize" }}>{run.provider}</span>
      <span className="ac-small ac-muted ac-trunc">{run.model || "—"}</span>
      <span className="ac-trunc ac-grow">{run.title || run.promptPreview}</span>
      {run.state === "WAITING_FOR_QUOTA" ? (
        <span className="ac-small ac-num ac-muted" title="Resumes automatically">◷ {fmtRelative(run.nextEligibleAt)}</span>
      ) : (
        <StatusGlyph state={run.state} />
      )}
    </div>
  );
}

export default function QueueView({ state }: { state: ConductorState }) {
  const active = activeRuns(state);
  const waiting = waitingRuns(state);
  const attention = attentionRuns(state);
  const paused = state.snapshot?.paused;

  return (
    <div>
      <header className="ac-header">
        <div className="ac-title">Agent Conductor</div>
        <div className="ac-subrow">
          <span>{state.host || "Local"}</span>
          <span aria-hidden="true">·</span>
          <span className={state.daemonHealthy ? "" : "ac-glyph failed"}>
            {state.daemonHealthy ? `daemon ${state.snapshot?.daemon?.version ?? ""}` : "daemon offline"}
          </span>
          {paused && <span className="ac-glyph wait">paused</span>}
        </div>
        <div className="ac-subrow" style={{ gap: 14 }}>
          <ProviderDot provider="claude" state={state} />
          <ProviderDot provider="codex" state={state} />
        </div>
      </header>

      {!state.daemonHealthy && (
        <div className="ac-banner" role="status">
          Daemon offline. Runs continue only when the daemon is running — use
          Command Palette → “Agent Conductor: Install Background Service”.
        </div>
      )}

      {attention.length > 0 && (
        <section className="ac-section" aria-label="Needs attention">
          <div className="ac-section-title">Attention</div>
          {attention.slice(0, 4).map((r) => (
            <div key={r.id} className="ac-card" style={{ marginBottom: 6 }}>
              <div className="ac-row between">
                <span className="ac-trunc ac-grow">{r.title || r.promptPreview}</span>
                <StatusGlyph state={r.state} />
              </div>
              {r.errorMsg && <div className="ac-small ac-muted">{r.errorMsg}</div>}
              <div className="ac-row" style={{ marginTop: 4, gap: 6 }}>
                {r.errorCode === "WORKSPACE_UNTRUSTED" && (
                  <button className="ac-btn ac-btn-sm" onClick={() => void rpc("trustWorkspace", { path: r.cwd }).then(() => rpc("continueRun", { runId: r.id }))}>
                    Trust Workspace
                  </button>
                )}
                <button className="ac-btn ac-btn-sm" onClick={() => void rpc("continueRun", { runId: r.id })}>Continue</button>
                <button className="ac-btn ac-btn-sm secondary" onClick={() => void rpc("retryRun", { runId: r.id })}>Retry</button>
                <button
                  className="ac-btn ac-btn-sm secondary"
                  onClick={() => {
                    if (confirm("Skip this run? It will be marked SKIPPED and the batch continues.")) {
                      void rpc("skipRun", { runId: r.id });
                    }
                  }}
                >
                  Skip
                </button>
              </div>
            </div>
          ))}
        </section>
      )}

      <section className="ac-section" aria-label="Active">
        <div className="ac-section-title">Active</div>
        {active.length === 0 && <div className="ac-empty">No managed run active.</div>}
        {active.map((r) => <ActiveRunCard key={r.id} run={r} />)}
      </section>

      <Composer state={state} />

      <section aria-label="Waiting">
        <div className="ac-section-title" style={{ padding: "0 12px" }}>Up next</div>
        <div role="list">
          {waiting.length === 0 && <div className="ac-empty">Queue is empty.</div>}
          {waiting.map((r, i) => <WaitingRow key={r.id} run={r} index={i} />)}
        </div>
      </section>

      <div className="ac-footer">
        <button className="ac-btn" onClick={() => document.getElementById("ac-prompt")?.focus()}>+ Queue Run</button>
        <button className="ac-btn secondary" onClick={() => openPlanner()}>Build Batch</button>
      </div>
    </div>
  );
}
