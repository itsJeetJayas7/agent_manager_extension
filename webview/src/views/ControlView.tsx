// Control tab: a short always-visible status summary, then progressive
// disclosure. It used to be seven stacked accordions, all collapsed — a filing
// cabinet rather than a control surface. Pause has moved to the queue header.
import React, { Fragment, useCallback, useEffect, useState } from "react";
import type { ConductorState } from "../state/store";
import { rpc, confirm, notify, vscodeApi } from "../api/client";

/**
 * Every settings write is a server-side patch.
 *
 * The old code did `const s = await getSettings(); updateSettings({...s, x})`,
 * so two concurrent edits silently lost one, and a component that snapshotted
 * settings on mount wrote stale values back over everything else.
 */
function patch(settings: Record<string, unknown>): Promise<unknown> {
  return rpc("patchSettings", settings);
}

function Disclosure({
  title,
  children,
  defaultOpen = false,
}: {
  title: string;
  children: React.ReactNode;
  defaultOpen?: boolean;
}) {
  return (
    <details className="ac-disclosure" open={defaultOpen}>
      <summary>{title}</summary>
      <div className="ac-disclosure-body">{children}</div>
    </details>
  );
}

// ---- always-visible summary ------------------------------------------------

function QuotaBars({ state, provider }: { state: ConductorState; provider: string }) {
  const q = state.snapshot?.quota?.[provider];
  const windows = q?.windows ?? [];
  if (windows.length === 0) {
    return <div className="ac-xs ac-muted">No usage data yet — learned from the first run.</div>;
  }
  return (
    <>
      {windows.map((w) => {
        const pct = w.hasPercent ? Math.min(100, Math.round(w.usedPercent)) : null;
        const cls = w.exhausted ? "full" : pct !== null && pct > 80 ? "warn" : "";
        const reset = w.hasReset
          ? `resets ${new Date(w.resetsAt).toLocaleString([], {
              weekday: "short",
              hour: "numeric",
              minute: "2-digit",
            })}`
          : "reset time unknown";
        return (
          <div key={w.id} className="ac-quota" title={`${w.source} · confidence: ${w.confidence}`}>
            <span className="ac-xs ac-muted ac-trunc">{w.name || w.id}</span>
            {/* Honesty over polish: an unknown percentage is hatched, never
                rendered as a plausible-looking bar. */}
            <div
              className={`ac-bar ${pct === null ? "unknown" : cls}`}
              role={pct === null ? undefined : "progressbar"}
              aria-valuenow={pct ?? undefined}
              aria-valuemin={0}
              aria-valuemax={100}
              aria-label={`${provider} ${w.name || w.id} usage`}
            >
              {pct !== null && <div style={{ width: `${pct}%` }} />}
            </div>
            <span className="ac-xs ac-num ac-muted">
              {pct === null ? (w.exhausted ? "exhausted" : "unavailable") : `${pct}%`} · {reset}
            </span>
          </div>
        );
      })}
      {q?.missingReset && (
        <div className="ac-xs ac-muted ac-mt-1">
          A reset time is unknown. Set the weekly override below if you know it.
        </div>
      )}
    </>
  );
}

function Summary({ state }: { state: ConductorState }) {
  const d = state.snapshot?.daemon;
  return (
    <section className="ac-section" aria-label="Status">
      <div className="ac-card">
        <div className="ac-row between">
          <span className="ac-row ac-gap-1">
            <span className={`ac-dot ${state.daemonHealthy ? "ok" : "err"}`} aria-hidden="true" />
            <strong>{state.daemonHealthy ? "Daemon running" : "Daemon offline"}</strong>
          </span>
          <span className="ac-xs ac-muted ac-num">
            {d ? `v${d.version} · pid ${d.pid} · up ${d.uptime}` : "—"}
          </span>
        </div>
        <div className="ac-xs ac-muted ac-mt-1">
          {state.host || "Local"} · {state.snapshot?.host?.platform ?? "unknown platform"}
        </div>
      </div>

      {(["claude", "codex"] as const).map((p) => {
        const caps = state.snapshot?.providers?.[p];
        return (
          <div key={p} className="ac-card">
            <div className="ac-row between">
              <span className="ac-row ac-gap-1">
                <span className={`ac-dot ${caps?.installed ? "ok" : "err"}`} aria-hidden="true" />
                <strong className="ac-cap">{p}</strong>
              </span>
              <span className="ac-xs ac-muted">
                {caps?.installed ? caps.version || "installed" : "runtime not found"}
              </span>
            </div>
            <div className="ac-mt-2">
              <QuotaBars state={state} provider={p} />
            </div>
          </div>
        );
      })}

      <div className="ac-row ac-gap-1 wrap ac-mt-3">
        <button
          className="ac-btn ghost ac-btn-sm"
          onClick={() => vscodeApi.postMessage({ type: "cmd", command: "agent-conductor.diagnostics" })}
        >
          Diagnostics
        </button>
        <button
          className="ac-btn ghost ac-btn-sm"
          onClick={() => vscodeApi.postMessage({ type: "cmd", command: "agent-conductor.openLogs" })}
        >
          Logs
        </button>
        <button
          className="ac-btn ghost ac-btn-sm"
          onClick={() => vscodeApi.postMessage({ type: "cmd", command: "agent-conductor.installService" })}
        >
          Install background service
        </button>
      </div>
    </section>
  );
}

// ---- disclosures -----------------------------------------------------------

function ProvidersDetail({ state }: { state: ConductorState }) {
  return (
    <Disclosure title="Providers">
      {(["claude", "codex"] as const).map((p) => {
        const caps = state.snapshot?.providers?.[p];
        const ext = (state.snapshot?.external?.[p] ?? []).find(
          (s) => s.kind === "external" && ["SessionStart", "UserPromptSubmit"].includes(s.lastEvent)
        );
        return (
          <div key={p} className="ac-card">
            <strong className="ac-cap">{p}</strong>
            <dl className="ac-kv ac-mt-2">
              <dt>Runtime</dt>
              <dd>{caps?.installed ? caps.version || "installed" : "not found"}</dd>
              <dt>Auth</dt>
              <dd>
                {caps?.authState ?? "unknown"}
                {caps?.planLabel ? ` · ${caps.planLabel}` : ""}
              </dd>
              <dt>Models</dt>
              <dd>{caps?.models?.length ?? 0} available</dd>
              <dt>External session</dt>
              <dd>{ext ? `running in ${ext.cwd || ext.nativeId.slice(0, 10)}` : "idle"}</dd>
            </dl>
            {caps?.notes?.map((n, i) => (
              <div key={i} className="ac-xs ac-muted ac-mt-1">
                {n}
              </div>
            ))}
            {p === "codex" && caps?.authState === "api_key" && (
              <div className="ac-banner err ac-mt-2">
                API-key billing detected. Managed runs may spend beyond your ChatGPT plan.
              </div>
            )}
            {ext && (
              <button
                className="ac-btn ghost ac-btn-sm ac-mt-2"
                onClick={() => void rpc("clearStaleExternalSession", { provider: p, nativeId: ext.nativeId })}
              >
                Clear stale busy state
              </button>
            )}
          </div>
        );
      })}
      <button
        className="ac-btn ghost ac-btn-sm ac-mt-2"
        onClick={() => vscodeApi.postMessage({ type: "cmd", command: "agent-conductor.repairRuntime" })}
      >
        Repair hook integration
      </button>
    </Disclosure>
  );
}

function Scheduling({ state }: { state: ConductorState }) {
  const s = state.snapshot?.settings;
  const [weekly, setWeekly] = useState("");
  return (
    <Disclosure title="Scheduling &amp; limits">
      <div className="ac-inline-controls">
        <div>
          <label className="ac-label" htmlFor="ac-max-total">
            Concurrent runs
          </label>
          <input
            id="ac-max-total"
            className="ac-input"
            type="number"
            min={1}
            max={8}
            value={s?.maxTotalRuns ?? 2}
            onChange={(e) => void patch({ maxTotalRuns: Number(e.target.value) || 1 })}
          />
        </div>
        <div>
          <label className="ac-label" htmlFor="ac-verify-timeout">
            Verification timeout (s)
          </label>
          <input
            id="ac-verify-timeout"
            className="ac-input"
            type="number"
            min={30}
            step={30}
            value={s?.verificationTimeoutSec ?? 1800}
            onChange={(e) => void patch({ verificationTimeoutSec: Number(e.target.value) || 1800 })}
          />
        </div>
      </div>
      <label className="ac-check ac-mt-2">
        <input
          type="checkbox"
          checked={!!s?.fifoBlockOnExternal}
          onChange={(e) => void patch({ fifoBlockOnExternal: e.target.checked })}
        />
        <span>Hold the queue while an agent runs outside Agent Conductor</span>
      </label>

      <label className="ac-label ac-mt-3" htmlFor="ac-weekly">
        Claude weekly reset override (RFC3339, optional)
      </label>
      <div className="ac-row">
        <input
          id="ac-weekly"
          className="ac-input"
          placeholder={s?.claudeWeeklyResetUtc || "2026-09-21T02:00:00Z"}
          value={weekly}
          onChange={(e) => setWeekly(e.target.value)}
        />
        <button
          className="ac-btn ghost ac-btn-sm"
          onClick={() => {
            void patch({ claudeWeeklyResetUtc: weekly }).then(() => notify("Weekly reset override saved."));
          }}
        >
          Save
        </button>
      </div>
      <div className="ac-xs ac-muted ac-mt-1">
        Used only when the provider does not report a reset time. Provider data always wins.
      </div>
    </Disclosure>
  );
}

function AnchorSection({ state }: { state: ConductorState }) {
  const s = state.snapshot?.settings;
  const codexModels = state.snapshot?.providers?.codex?.models ?? [];
  const claudeModels = state.snapshot?.providers?.claude?.models ?? [];
  return (
    <Disclosure title="Window anchor">
      <div className="ac-small ac-muted">
        Opens each new quota window with one tiny request while the machine is idle, so the clock starts without
        spending real work on it.
      </div>
      <label className="ac-check ac-mt-2">
        <input
          type="checkbox"
          checked={!!s?.anchorCodexEnabled}
          onChange={(e) => void patch({ anchorCodexEnabled: e.target.checked })}
        />
        <span>Codex window anchor</span>
      </label>
      <label className="ac-check">
        <input
          type="checkbox"
          checked={!!s?.anchorClaudeEnabled}
          onChange={(e) => void patch({ anchorClaudeEnabled: e.target.checked })}
        />
        <span>
          Claude window anchor <span className="ac-glyph wait">experimental</span>
        </span>
      </label>
      <div className="ac-inline-controls ac-mt-2">
        <div>
          <label className="ac-label" htmlFor="ac-anchor-codex">
            Codex anchor model
          </label>
          <select
            id="ac-anchor-codex"
            className="ac-select"
            value={s?.anchorModelCodex ?? "auto"}
            onChange={(e) => void patch({ anchorModelCodex: e.target.value })}
          >
            <option value="auto">Auto — lightest available</option>
            {codexModels.map((m) => (
              <option key={m.id} value={m.id}>
                {m.name || m.id}
              </option>
            ))}
          </select>
        </div>
        <div>
          <label className="ac-label" htmlFor="ac-anchor-claude">
            Claude anchor model
          </label>
          <select
            id="ac-anchor-claude"
            className="ac-select"
            value={s?.anchorModelClaude ?? "auto"}
            onChange={(e) => void patch({ anchorModelClaude: e.target.value })}
          >
            <option value="auto">Auto — lightest available</option>
            {claudeModels.map((m) => (
              <option key={m.id} value={m.id}>
                {m.name || m.id}
              </option>
            ))}
          </select>
        </div>
      </div>
    </Disclosure>
  );
}

function SecretBlock({ state }: { state: ConductorState }) {
  const [text, setText] = useState("");
  const [revealed, setRevealed] = useState<string | null>(null);
  const [ack, setAck] = useState(false);
  const exists = !!state.snapshot?.hasSecretBlock;
  return (
    <Disclosure title="Shared testing &amp; sudo block">
      <div className="ac-banner">
        This block is sent literally to Claude or Codex and becomes part of that provider's conversation history.
        It is encrypted at rest by the daemon and never written to logs.
      </div>
      <div className="ac-xs ac-muted ac-mt-2">
        {exists ? "A block is stored." : "No block stored."}
      </div>
      {!ack ? (
        <button className="ac-btn ghost ac-btn-sm ac-mt-2" onClick={() => setAck(true)}>
          I understand — let me edit it
        </button>
      ) : (
        <>
          <label className="ac-sr" htmlFor="ac-secret">
            Shared testing and sudo block
          </label>
          <textarea
            id="ac-secret"
            className="ac-textarea ac-textarea-md ac-mt-2"
            value={text}
            placeholder="Testing credentials, sudo password, usage instructions…"
            onChange={(e) => setText(e.target.value)}
          />
          <div className="ac-row ac-gap-1 wrap ac-mt-2">
            <button
              className="ac-btn ac-btn-sm"
              disabled={text.trim() === ""}
              onClick={() =>
                void rpc("updateSecretBlock", { text }).then(() => {
                  setText("");
                  notify("Shared block saved.");
                })
              }
            >
              Save
            </button>
            <button
              className="ac-btn ghost ac-btn-sm"
              onClick={async () => {
                const r = await rpc<any>("revealSecretBlock", {}).catch(() => null);
                setRevealed(r?.text ?? "(nothing stored)");
              }}
            >
              Reveal
            </button>
            {revealed !== null && (
              <button className="ac-btn ghost ac-btn-sm" onClick={() => setRevealed(null)}>
                Hide
              </button>
            )}
            <button
              className="ac-btn danger ac-btn-sm"
              disabled={!exists}
              onClick={async () => {
                if (
                  await confirm("Delete the shared testing & sudo block?", {
                    detail:
                      "It is removed from the database permanently and detached from every queued run. This cannot be undone.",
                    confirmLabel: "Delete permanently",
                  })
                ) {
                  await rpc("deleteSecretBlock", {});
                  setRevealed(null);
                  notify("Shared block deleted.");
                }
              }}
            >
              Delete
            </button>
          </div>
          {revealed !== null && (
            <pre className="ac-card ac-mono ac-small ac-pre ac-mt-2" aria-label="Revealed block">
              {revealed}
            </pre>
          )}
        </>
      )}
    </Disclosure>
  );
}

function Staged({ state }: { state: ConductorState }) {
  const s = state.snapshot?.settings;
  // Sourced from the live provider capabilities. The old code read
  // `s._codexModels`, a field that exists nowhere, so both Codex dropdowns
  // were permanently empty.
  const claudeIds = (state.snapshot?.providers?.claude?.models ?? []).map((m) => m.id);
  const codexIds = (state.snapshot?.providers?.codex?.models ?? []).map((m) => m.id);

  const field = (label: string, key: string, options: string[], value: string | undefined) => (
    <div>
      <label className="ac-label" htmlFor={`ac-${key}`}>
        {label}
      </label>
      <select
        id={`ac-${key}`}
        className="ac-select"
        value={value ?? ""}
        onChange={(e) => void patch({ [key]: e.target.value })}
      >
        <option value="">Provider default</option>
        {options.map((o) => (
          <option key={o} value={o}>
            {o}
          </option>
        ))}
      </select>
    </div>
  );

  return (
    <Disclosure title="Staged intelligence">
      <div className="ac-small ac-muted">Plan with a strong model, build with a cheaper one — same native conversation.</div>
      <label className="ac-check ac-mt-2">
        <input
          type="checkbox"
          checked={!!s?.auditEnabled}
          onChange={(e) => void patch({ auditEnabled: e.target.checked })}
        />
        <span>Final smart audit, one fix cycle</span>
      </label>
      <div className="ac-inline-controls ac-mt-2">
        {field("Claude plan model", "claudePlanModel", claudeIds, s?.claudePlanModel)}
        {field("Claude build model", "claudeBuildModel", claudeIds, s?.claudeBuildModel)}
      </div>
      <div className="ac-inline-controls ac-mt-2">
        {field("Codex plan model", "codexPlanModel", codexIds, s?.codexPlanModel)}
        {field("Codex build model", "codexBuildModel", codexIds, s?.codexBuildModel)}
      </div>
      {codexIds.length === 0 && (
        <div className="ac-xs ac-muted ac-mt-1">Codex models appear once the Codex runtime is reachable.</div>
      )}
    </Disclosure>
  );
}

interface UsageSummary {
  runs: number;
  completed: number;
  failed: number;
  activeMs: number;
  quotaWaitMs: number;
  continuations: number;
  byProviderActiveMs: Record<string, number>;
  recent: { runId: string; title: string; provider: string; activeMs: number; continuations: number }[];
  note: string;
}

function fmtHours(ms: number): string {
  if (ms < 60_000) {
    return `${Math.round(ms / 1000)}s`;
  }
  const m = Math.round(ms / 60_000);
  return m < 60 ? `${m}m` : `${Math.floor(m / 60)}h ${m % 60}m`;
}

/** §7.6 — only what is measured. No invented spend. */
function Usage() {
  const [data, setData] = useState<UsageSummary | null>(null);
  const [error, setError] = useState("");
  const load = useCallback(() => {
    rpc<UsageSummary>("usageSummary", { windowDays: 7 })
      .then(setData)
      .catch((e: any) => setError(String(e?.message ?? e)));
  }, []);
  useEffect(load, [load]);

  return (
    <Disclosure title="This week">
      {error && <div className="ac-banner err">{error}</div>}
      {!data && !error && <div className="ac-xs ac-muted">loading…</div>}
      {data && (
        <>
          <dl className="ac-kv">
            <dt>Runs</dt>
            <dd className="ac-num">
              {data.runs} · {data.completed} completed · {data.failed} needed you
            </dd>
            <dt>Agent time</dt>
            <dd className="ac-num">{fmtHours(data.activeMs)}</dd>
            <dt>Waiting on quota</dt>
            <dd className="ac-num">{fmtHours(data.quotaWaitMs)}</dd>
            <dt>Continuations</dt>
            <dd className="ac-num">{data.continuations}</dd>
            {Object.entries(data.byProviderActiveMs ?? {}).map(([p, ms]) => (
              <Fragment key={p}>
                <dt className="ac-cap">{p}</dt>
                <dd className="ac-num">{fmtHours(ms)}</dd>
              </Fragment>
            ))}
          </dl>
          {data.recent.length > 0 && (
            <>
              <div className="ac-eyebrow ac-mt-3 ac-mb-2">Longest runs</div>
              <ul className="ac-checklist">
                {data.recent.slice(0, 5).map((r) => (
                  <li key={r.runId}>
                    <span className="ac-grow ac-trunc">{r.title || r.runId}</span>
                    <span className="ac-xs ac-muted ac-num">{fmtHours(r.activeMs)}</span>
                  </li>
                ))}
              </ul>
            </>
          )}
          <div className="ac-xs ac-muted ac-mt-3">{data.note}</div>
          <button className="ac-btn ghost ac-btn-sm ac-mt-2" onClick={load}>
            Refresh
          </button>
        </>
      )}
    </Disclosure>
  );
}

function Maintenance() {
  const [busy, setBusy] = useState(false);
  return (
    <Disclosure title="Maintenance">
      <div className="ac-small ac-muted">
        Parallel rows require a clean Git repository. Agent Conductor never stashes or discards your changes: runs
        execute in isolated worktrees and merge into an internal integration branch, and your branch changes only when
        you explicitly apply a batch.
      </div>
      <button
        className="ac-btn ghost ac-btn-sm ac-mt-3"
        disabled={busy}
        onClick={async () => {
          setBusy(true);
          try {
            const r = await rpc<{ prunedEvents: number }>("compact", {});
            notify(`Database compacted — ${r.prunedEvents} old event(s) removed.`);
          } catch (e: any) {
            notify(String(e?.message ?? e), "error");
          } finally {
            setBusy(false);
          }
        }}
      >
        Compact database
      </button>
      <button
        className="ac-btn danger ac-btn-sm ac-mt-2"
        onClick={() => vscodeApi.postMessage({ type: "cmd", command: "agent-conductor.uninstallService" })}
      >
        Uninstall background service &amp; hooks
      </button>
    </Disclosure>
  );
}

export default function ControlView({ state }: { state: ConductorState }) {
  return (
    <div>
      <header className="ac-header">
        <div className="ac-title">Control</div>
        <div className="ac-subrow">Everything Agent Conductor knows about this machine.</div>
      </header>
      <Summary state={state} />
      <ProvidersDetail state={state} />
      <Scheduling state={state} />
      <AnchorSection state={state} />
      <SecretBlock state={state} />
      <Staged state={state} />
      <Usage />
      <Maintenance />
    </div>
  );
}
