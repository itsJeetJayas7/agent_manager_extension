import { useEffect, useRef, useState } from "react";
import type { RunActivityLike, RunLike } from "../../../src/extension/protocol";
import { rpc } from "../api/client";
import { ACTIVE_STATES } from "../state/store";
import { useNow } from "./ticker";
import { fmtDuration } from "./format";

/** Liveness is checked independently of the persisted RUNNING label. No
 * invented heartbeat: only a successful daemon response renews freshness. */
export default function RunActivity({ run, connected = true }: { run: RunLike; connected?: boolean }) {
  const [sample, setSample] = useState<{ data: RunActivityLike; receivedAt: number } | null>(null);
  const [error, setError] = useState("");
  const [refresh, setRefresh] = useState(0);
  const log = useRef<HTMLDivElement>(null);
  const follow = useRef(true);
  const now = useNow();
  const active = ACTIVE_STATES.has(run.state);
  const providerExpected = ["STARTING", "RUNNING", "WAITING_FOR_PROVIDER", "CANCEL_REQUESTED"].includes(run.state);

  useEffect(() => {
    let disposed = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const load = async () => {
      try {
        const data = await rpc<RunActivityLike>("getRunActivity", { runId: run.id });
        if (!data || !Array.isArray(data.entries) || typeof data.executionActive !== "boolean") throw new Error("Update and restart the background service to enable live activity.");
        if (!disposed) { setSample({ data, receivedAt: Date.now() }); setError(""); }
      } catch (e) {
        const message = e instanceof Error ? e.message : String(e);
        if (!disposed) setError(/UNKNOWN_METHOD|unknown method/i.test(message)
          ? "Update the daemon with Agent Conductor: Install Background Service, then reload this window."
          : message);
      } finally {
        if (!disposed && active) timer = setTimeout(load, 2000);
      }
    };
    if (connected) void load();
    return () => { disposed = true; if (timer) clearTimeout(timer); };
  }, [run.id, run.state, active, connected, refresh]);

  useEffect(() => {
    if (follow.current && log.current) log.current.scrollTop = log.current.scrollHeight;
  }, [sample]);

  const data = sample?.data;
  const stale = !!sample && now - sample.receivedAt > 10_000;
  const unavailable = !connected || !!error || stale;
  // Compare provider times to daemon time, then advance by local elapsed time.
  const age = data?.lastEventAt && sample
    ? Math.max(0, Date.parse(data.observedAt) - Date.parse(data.lastEventAt) + now - sample.receivedAt) : null;
  const quiet = age !== null && age > 60_000;
  let label = "Checking run activity…";
  if (unavailable) label = "Live status unavailable";
  else if (data) {
    if (!data.executionActive) label = providerExpected ? "No active run worker"
      : run.state === "VERIFYING" ? "Verifying result · provider turn finished"
      : run.state === "INTEGRATING" ? "Integrating changes · provider turn finished"
      : active ? "Waiting for provider dispatch" : "Run worker stopped";
    else if (data.processAlive === false) label = "Provider process exited · awaiting run update";
    else if (data.processAlive) label = `Process alive · PID ${data.processId}`;
    else label = "Run worker active";
  }
  const recent = !unavailable && !!data?.executionActive && data.processAlive !== false && age !== null && age < 30_000;
  // Reported by the daemon from the relay itself, never inferred from a tab.
  const viewers = !unavailable ? data?.live?.viewers ?? 0 : 0;
  const nativeTurn = !unavailable && data?.live?.busy === "native";

  return <section className="ac-activity" aria-label="Run activity">
    <div className="ac-row wrap ac-gap-1">
      <span className={`ac-activity-dot${recent ? " live" : ""}`} aria-hidden="true" />
      <strong className="ac-small">{label}</strong>
      {age !== null && <span className="ac-small ac-muted" title={data?.lastEventAt}>Last provider event {age < 1000 ? "just now" : `${fmtDuration(age)} ago`}</span>}
    </div>
    {viewers > 0 && <p className="ac-small" role="status">Live in Claude Code · {viewers === 1 ? "1 tab attached" : `${viewers} tabs attached`} to process {data?.live?.pid}{nativeTurn ? " · a message sent from Claude Code is running" : ""}</p>}
    {unavailable && <p className="ac-small ac-muted">{!connected ? "Daemon disconnected; the displayed run state may be stale." : error || "The daemon has not confirmed this run recently."}</p>}
    {!unavailable && quiet && data?.executionActive && <p className="ac-small ac-muted">No recent provider output. The agent may be thinking or waiting on a tool; progress is unconfirmed.</p>}
    {!unavailable && data && !data.executionActive && providerExpected && <p className="ac-small ac-muted">The saved state says active, but this daemon is not supervising a provider turn. Refresh the queue or inspect the run timeline.</p>}
    <details className="ac-mt-2" open={active}>
      <summary className="ac-small">Live activity</summary>
      <div ref={log} className="ac-activity-log" tabIndex={0} aria-label="Recent provider output" onScroll={() => {
        const el = log.current;
        if (el) follow.current = el.scrollHeight - el.scrollTop - el.clientHeight < 32;
      }}>
        {(data?.entries ?? []).slice(-40).map((entry, i) => <div className={`ac-activity-entry ${entry.kind}`} key={`${i}:${entry.kind}`}>
          <time title={entry.at}>{new Date(entry.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })}</time>
          <pre>{entry.text}</pre>
        </div>)}
        {!data?.entries.length && <p className="ac-small ac-muted">{active ? "Waiting for provider output…" : "No activity retained by this daemon. Open the native session for conversation history."}</p>}
      </div>
      <p className="ac-small ac-muted">{run.provider === "claude"
        ? "Recent output from this daemon. “Open in Claude Code” shows the full conversation, live."
        : "Recent output from this daemon. Native conversation history is available in the provider extension."}</p>
    </details>
    {unavailable && <button className="ac-btn ghost ac-btn-sm" onClick={() => { setError(""); setRefresh(v => v + 1); }}>Check again</button>}
  </section>;
}
