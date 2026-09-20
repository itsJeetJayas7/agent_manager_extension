// Control tab: runtime, providers, usage, anchor, secrets, staged,
// parallel, verification, maintenance.
import React, { useEffect, useState } from "react";
import type { ConductorState } from "../state/store";
import { rpc } from "../api/client";
import { vscodeApi } from "../api/client";

function Section({ title, children, defaultOpen = false }: { title: string; children: React.ReactNode; defaultOpen?: boolean }) {
  const [open, setOpen] = useState(defaultOpen);
  return (
    <section className="ac-section" style={{ paddingTop: 8, paddingBottom: 8 }}>
      <button
        className="ac-tab"
        style={{ width: "100%", textAlign: "left", borderBottom: "none", padding: "4px 0" }}
        aria-expanded={open}
        onClick={() => setOpen(!open)}
      >
        {title}
      </button>
      {open && <div style={{ paddingTop: 8 }}>{children}</div>}
    </section>
  );
}

function Runtime({ state }: { state: ConductorState }) {
  const d = state.snapshot?.daemon;
  return (
    <Section title="Runtime" defaultOpen>
      <dl className="ac-kv">
        <dt>Daemon</dt>
        <dd>{state.daemonHealthy ? `Running · v${d?.version ?? "?"}` : "Offline"}</dd>
        <dt>PID</dt><dd className="ac-num">{d?.pid ?? "—"}</dd>
        <dt>Uptime</dt><dd>{d?.uptime ?? "—"}</dd>
        <dt>Host</dt><dd>{state.host || "Local"}</dd>
        <dt>Platform</dt><dd>{state.snapshot?.host?.platform ?? "—"}</dd>
      </dl>
      <div className="ac-row" style={{ marginTop: 8, flexWrap: "wrap", gap: 6 }}>
        <button className="ac-btn ac-btn-sm" onClick={() => vscodeApi.postMessage({ type: "cmd", command: "agent-conductor.openLogs" })}>View Logs</button>
        <button className="ac-btn ac-btn-sm secondary" onClick={() => vscodeApi.postMessage({ type: "cmd", command: "agent-conductor.diagnostics" })}>Run Diagnostics</button>
        <button className="ac-btn ac-btn-sm secondary" onClick={() => vscodeApi.postMessage({ type: "cmd", command: "agent-conductor.installService" })}>Install Service</button>
        <button className="ac-btn ac-btn-sm danger" onClick={() => vscodeApi.postMessage({ type: "cmd", command: "agent-conductor.uninstallService" })}>Uninstall Service</button>
      </div>
      <div className="ac-row" style={{ marginTop: 8, gap: 6 }}>
        {state.snapshot?.paused ? (
          <button className="ac-btn ac-btn-sm" onClick={() => void rpc("resumeScheduler", {})}>Resume Queue</button>
        ) : (
          <button className="ac-btn ac-btn-sm secondary" onClick={() => void rpc("pauseScheduler", {})}>Pause Queue</button>
        )}
      </div>
    </Section>
  );
}

function Providers({ state }: { state: ConductorState }) {
  return (
    <Section title="Providers">
      {(["claude", "codex"] as const).map((p) => {
        const caps = state.snapshot?.providers?.[p];
        const ext = (state.snapshot?.external?.[p] ?? []).find(
          (s) => s.kind === "external" && ["SessionStart", "UserPromptSubmit"].includes(s.lastEvent)
        );
        return (
          <div key={p} className="ac-card">
            <div className="ac-row between">
              <strong style={{ textTransform: "capitalize" }}>{p}</strong>
              <span className={`ac-dot ${caps?.installed ? "ok" : "err"}`} aria-hidden="true" />
            </div>
            <dl className="ac-kv" style={{ marginTop: 4 }}>
              <dt>Installed</dt><dd>{caps?.installed ? caps.version || "yes" : "runtime not found"}</dd>
              <dt>Auth</dt><dd>{caps?.authState ?? "unknown"}{caps?.planLabel ? ` · ${caps.planLabel}` : ""}</dd>
              <dt>Models</dt><dd>{caps?.models?.length ?? 0} discovered</dd>
            </dl>
            {caps?.notes?.map((n, i) => <div key={i} className="ac-small ac-muted">{n}</div>)}
            {p === "codex" && caps?.authState === "api_key" && (
              <div className="ac-banner err">API-key billing detected. Managed runs may spend beyond your ChatGPT plan limits.</div>
            )}
            <div className="ac-small ac-muted" style={{ marginTop: 4 }}>
              External session: {ext ? `● Running (${ext.nativeId.slice(0, 10)}… ${ext.cwd ?? ""})` : "Idle"}
              {ext && (
                <>
                  {" · "}
                  <button className="ac-btn ac-btn-sm secondary" onClick={() => void rpc("clearStaleExternalSession", { provider: p, nativeId: ext.nativeId })}>
                    Clear stale busy state
                  </button>
                </>
              )}
            </div>
          </div>
        );
      })}
      <button className="ac-btn ac-btn-sm secondary" onClick={() => vscodeApi.postMessage({ type: "cmd", command: "agent-conductor.repairRuntime" })}>
        Repair Hook Integration
      </button>
    </Section>
  );
}

function Usage({ state }: { state: ConductorState }) {
  return (
    <Section title="Usage & Limits">
      {(["claude", "codex"] as const).map((p) => {
        const q = state.snapshot?.quota?.[p];
        return (
          <div key={p} style={{ marginBottom: 10 }} aria-label={`${p} usage`}>
            <div className="ac-small" style={{ textTransform: "uppercase", letterSpacing: "0.06em", marginBottom: 2 }}>{p}</div>
            {(q?.windows ?? []).length === 0 && <div className="ac-small ac-muted">No provider usage data yet.</div>}
            {(q?.windows ?? []).map((w) => {
              const pct = w.hasPercent ? Math.min(100, Math.round(w.usedPercent)) : null;
              const cls = w.exhausted ? "full" : pct !== null && pct > 80 ? "warn" : "";
              const reset = w.hasReset ? `resets ${new Date(w.resetsAt).toLocaleString([], { weekday: "short", hour: "2-digit", minute: "2-digit" })}` : "reset time unknown";
              return (
                <div key={w.id} className="ac-quota" title={`${w.source} · confidence: ${w.confidence}`}>
                  <span className="ac-small ac-muted ac-num">{w.id}</span>
                  <div className={`ac-bar ${cls}`} role="progressbar" aria-valuenow={pct ?? undefined} aria-label={`${p} ${w.name} usage`}>
                    <div style={{ width: `${pct ?? 100}%`, opacity: pct === null ? 0.25 : 1 }} />
                  </div>
                  <span className="ac-small ac-num ac-muted">
                    {pct === null ? (w.exhausted ? "exhausted" : "% unavailable") : `${pct}%`}
                    {" · "}{reset}
                  </span>
                </div>
              );
            })}
            {q?.missingReset && <div className="ac-small ac-muted">Reset time unknown — configure weekly reset override below.</div>}
          </div>
        );
      })}
      <WeeklyOverride />
    </Section>
  );
}

function WeeklyOverride() {
  const [val, setVal] = useState("");
  return (
    <div>
      <label className="ac-label" htmlFor="ac-weekly">Claude weekly reset override (RFC3339, optional)</label>
      <div className="ac-row">
        <input id="ac-weekly" className="ac-input" placeholder="2026-09-21T02:00:00Z" value={val}
          onChange={(e) => setVal(e.target.value)} />
        <button className="ac-btn ac-btn-sm" onClick={async () => {
          const s = await rpc<any>("getSettings", {});
          await rpc("updateSettings", { ...s, claudeWeeklyResetUtc: val });
        }}>Save</button>
      </div>
    </div>
  );
}

function AnchorSection({ state }: { state: ConductorState }) {
  const s = state.snapshot?.settings;

  const set = async (patch: any) => {
    const cur = await rpc<any>("getSettings", {});
    await rpc("updateSettings", { ...cur, ...patch });
  };
  return (
    <Section title="Window Anchor">
      <div className="ac-small ac-muted">
        Opens each new quota window with one tiny request while idle overnight, so the clock runs without wasting real work.
      </div>
      <label className="ac-check">
        <input type="checkbox" checked={!!s?.anchorCodexEnabled} onChange={(e) => void set({ anchorCodexEnabled: e.target.checked })} />
        Codex Window Anchor (structured rate limits)
      </label>
      <label className="ac-check">
        <input type="checkbox" checked={!!s?.anchorClaudeEnabled} onChange={(e) => void set({ anchorClaudeEnabled: e.target.checked })} />
        Claude Window Anchor — <span className="ac-glyph wait">Experimental</span>
      </label>
      <div className="ac-row" style={{ marginTop: 4 }}>
        <div className="ac-grow">
          <label className="ac-label" htmlFor="ac-anchor-codex">Codex anchor model</label>
          <select id="ac-anchor-codex" className="ac-select" value={s?.anchorModelCodex ?? "auto"} onChange={(e) => void set({ anchorModelCodex: e.target.value })}>
            <option value="auto">Auto (lightest available)</option>
            {(state.snapshot?.providers?.codex?.models ?? []).map((m) => <option key={m.id} value={m.id}>{m.name || m.id}</option>)}
          </select>
        </div>
        <div className="ac-grow">
          <label className="ac-label" htmlFor="ac-anchor-claude">Claude anchor model</label>
          <select id="ac-anchor-claude" className="ac-select" value={s?.anchorModelClaude ?? "auto"} onChange={(e) => void set({ anchorModelClaude: e.target.value })}>
            <option value="auto">Auto (lightest available)</option>
            {(state.snapshot?.providers?.claude?.models ?? []).map((m) => <option key={m.id} value={m.id}>{m.name || m.id}</option>)}
          </select>
        </div>
      </div>
    </Section>
  );
}

function SecretBlock() {
  const [text, setText] = useState("");
  const [revealed, setRevealed] = useState<string | null>(null);
  const [ack, setAck] = useState(false);
  return (
    <Section title="Shared Testing & Sudo Block">
      {!ack && (
        <div className="ac-banner" role="alert">
          The block is sent literally to Claude/Codex and becomes part of that provider conversation history.
          It is encrypted at rest by the daemon and never written to logs.
          <div style={{ marginTop: 6 }}>
            <button className="ac-btn ac-btn-sm" onClick={() => setAck(true)}>I understand</button>
          </div>
        </div>
      )}
      {ack && (
        <>
          <label className="ac-sr" htmlFor="ac-secret">Shared testing and sudo block</label>
          <textarea id="ac-secret" className="ac-textarea" style={{ minHeight: 120 }} value={text}
            placeholder="Testing credentials, sudo password, usage instructions…"
            onChange={(e) => setText(e.target.value)} />
          <div className="ac-row" style={{ marginTop: 6, flexWrap: "wrap" }}>
            <button className="ac-btn ac-btn-sm" onClick={() => void rpc("updateSecretBlock", { text }).then(() => setText(""))}>Save</button>
            <button className="ac-btn ac-btn-sm secondary" onClick={async () => {
              const r = await rpc<any>("revealSecretBlock", {}).catch(() => null);
              setRevealed(r?.text ?? "(none stored)");
            }}>Reveal</button>
            {revealed !== null && (
              <button className="ac-btn ac-btn-sm secondary" onClick={() => setRevealed(null)}>Hide</button>
            )}
            <button className="ac-btn ac-btn-sm danger" onClick={() => void rpc("deleteSecretBlock", {})}>Delete</button>
          </div>
          {revealed !== null && (
            <pre className="ac-card ac-mono ac-small" style={{ whiteSpace: "pre-wrap", marginTop: 8 }} aria-label="Revealed secret block">{revealed}</pre>
          )}
        </>
      )}
    </Section>
  );
}

function Staged() {
  const [s, setS] = useState<any>(null);
  useEffect(() => {
    void rpc("getSettings", {}).then(setS);
  }, []);
  if (!s) {
    return <Section title="Staged Intelligence"><div className="ac-small ac-muted">loading…</div></Section>;
  }
  const upd = (patch: any) => {
    const next = { ...s, ...patch };
    setS(next);
    void rpc("updateSettings", next);
  };
  const field = (label: string, key: string, options: string[], value: string) => (
    <div className="ac-grow">
      <label className="ac-label">{label}</label>
      <select className="ac-select" value={value} onChange={(e) => upd({ [key]: e.target.value })}>
        <option value="">(default)</option>
        {options.map((o) => <option key={o} value={o}>{o}</option>)}
      </select>
    </div>
  );
  return (
    <Section title="Staged Intelligence">
      <div className="ac-small ac-muted">Plan with a strong model, build with a cheaper one — same native conversation.</div>
      <label className="ac-check">
        <input type="checkbox" checked={!!s.auditEnabled} onChange={(e) => upd({ auditEnabled: e.target.checked })} />
        Final smart audit (one fix cycle)
      </label>
      <div className="ac-row" style={{ alignItems: "flex-start" }}>
        {field("Claude plan model", "claudePlanModel", ["opus", "sonnet", "haiku"], s.claudePlanModel)}
        {field("Claude build model", "claudeBuildModel", ["opus", "sonnet", "haiku"], s.claudeBuildModel)}
      </div>
      <div className="ac-row" style={{ alignItems: "flex-start" }}>
        {field("Codex plan model", "codexPlanModel", ["", ...(s._codexModels ?? [])], s.codexPlanModel)}
        {field("Codex build model", "codexBuildModel", ["", ...(s._codexModels ?? [])], s.codexBuildModel)}
      </div>
    </Section>
  );
}

function Parallel() {
  return (
    <Section title="Parallel Execution">
      <div className="ac-small ac-muted">
        Parallel rows require a clean Git repository; Agent Conductor never stashes or discards your changes.
        Runs execute in isolated worktrees and merge into an internal integration branch. Your original branch
        changes only when you explicitly apply the batch.
      </div>
    </Section>
  );
}

export default function ControlView({ state }: { state: ConductorState }) {
  return (
    <div>
      <header className="ac-header"><div className="ac-title">Control</div></header>
      <Runtime state={state} />
      <Providers state={state} />
      <Usage state={state} />
      <AnchorSection state={state} />
      <SecretBlock />
      <Staged />
      <Parallel />
    </div>
  );
}
