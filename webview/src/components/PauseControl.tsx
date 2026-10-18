// Pause is a primary control, not a settings toggle: it lives in the queue
// header, one click away, and says plainly what it is doing.
import { useEffect, useRef, useState } from "react";
import type { PauseStateLike } from "../../../src/extension/protocol";
import { rpc } from "../api/client";
import { useNow, formatCountdown, describeRemaining, formatClock } from "./ticker";

const PRESETS: { label: string; ms: number }[] = [
  { label: "15m", ms: 15 * 60_000 },
  { label: "30m", ms: 30 * 60_000 },
  { label: "1h", ms: 60 * 60_000 },
  { label: "2h", ms: 2 * 60 * 60_000 },
  { label: "4h", ms: 4 * 60 * 60_000 },
  { label: "8h", ms: 8 * 60 * 60_000 },
];

function resumeAtMs(pause: PauseStateLike | null): number | null {
  if (!pause?.resumeAt) {
    return null;
  }
  const t = new Date(pause.resumeAt).getTime();
  return Number.isNaN(t) ? null : t;
}

/** The quiet ghost button in the queue header, plus its popover. */
export function PauseButton({ pause }: { pause: PauseStateLike | null }) {
  const [open, setOpen] = useState(false);
  const [timed, setTimed] = useState(true);
  const [durationMs, setDurationMs] = useState(PRESETS[2].ms);
  const [custom, setCustom] = useState("");
  const [includeActive, setIncludeActive] = useState(false);
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const wrapRef = useRef<HTMLDivElement>(null);
  const firstFieldRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (!open) {
      return;
    }
    firstFieldRef.current?.focus();
    const onDown = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  if (pause?.paused) {
    return null; // the band takes over
  }

  const effectiveMs = (): number | undefined => {
    if (!timed) {
      return undefined;
    }
    const parsed = parseDuration(custom);
    return parsed ?? durationMs;
  };

  const submit = async () => {
    setBusy(true);
    setError("");
    try {
      await rpc("pauseQueue", {
        durationMs: effectiveMs(),
        scope: includeActive ? "INCLUDE_ACTIVE" : "QUEUE_ONLY",
        reason: reason.trim(),
      });
      setOpen(false);
      setReason("");
      setCustom("");
    } catch (e: any) {
      setError(String(e?.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="ac-popover-anchor" ref={wrapRef}>
      <button
        className="ac-btn ghost ac-btn-sm"
        aria-expanded={open}
        aria-haspopup="dialog"
        onClick={() => setOpen((v) => !v)}
        title="Pause the queue"
      >
        <span aria-hidden="true">⏸</span> Pause
      </button>
      {open && (
        <div className="ac-popover" role="dialog" aria-label="Pause queue">
          <div className="ac-popover-title">Pause queue</div>

          <label className="ac-radio">
            <input
              ref={firstFieldRef}
              type="radio"
              name="ac-pause-mode"
              checked={!timed}
              onChange={() => setTimed(false)}
            />
            <span>Until I resume</span>
          </label>
          <label className="ac-radio">
            <input type="radio" name="ac-pause-mode" checked={timed} onChange={() => setTimed(true)} />
            <span>Resume automatically in…</span>
          </label>

          <div className="ac-chips" role="group" aria-label="Pause duration">
            {PRESETS.map((p) => (
              <button
                key={p.label}
                className="ac-chip"
                aria-pressed={timed && !parseDuration(custom) && durationMs === p.ms}
                onClick={() => {
                  setTimed(true);
                  setCustom("");
                  setDurationMs(p.ms);
                }}
              >
                {p.label}
              </button>
            ))}
          </div>

          <label className="ac-label" htmlFor="ac-pause-custom">
            Or a custom duration
          </label>
          <input
            id="ac-pause-custom"
            className="ac-input"
            placeholder="e.g. 45m, 90m, 3h"
            value={custom}
            onChange={(e) => {
              setCustom(e.target.value);
              setTimed(true);
            }}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                void submit();
              }
            }}
          />
          {custom.trim() !== "" && parseDuration(custom) === null && (
            <div className="ac-xs ac-muted ac-mt-1">Not a duration I understand — try 45m or 3h.</div>
          )}

          <label className="ac-check ac-mt-3">
            <input
              type="checkbox"
              checked={includeActive}
              onChange={(e) => setIncludeActive(e.target.checked)}
            />
            <span>
              Also pause the run in progress
              <span className="ac-xs ac-muted ac-block">
                Stops at a safe point, resumable in the same session.
              </span>
            </span>
          </label>

          <label className="ac-label ac-mt-2" htmlFor="ac-pause-reason">
            Note (optional)
          </label>
          <input
            id="ac-pause-reason"
            className="ac-input"
            placeholder="waiting on the API review"
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                void submit();
              }
            }}
          />

          {error && (
            <div className="ac-banner err ac-mt-2" role="alert">
              {error}
            </div>
          )}

          <div className="ac-row ac-mt-3 ac-end">
            <button className="ac-btn ghost ac-btn-sm" onClick={() => setOpen(false)}>
              Cancel
            </button>
            <button className="ac-btn ac-btn-sm" disabled={busy} onClick={() => void submit()}>
              Pause
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

/** The calm, unmissable band shown while the queue is held. */
export function PauseBand({
  pause,
  waitingCount,
  availableProvider,
}: {
  pause: PauseStateLike;
  waitingCount: number;
  /** A provider that still has quota, when the hold is a quota hold. */
  availableProvider?: string;
}) {
  const now = useNow();
  const [busy, setBusy] = useState(false);
  const until = resumeAtMs(pause);
  const remaining = until === null ? null : Math.max(0, until - now);
  const isUser = pause.source === "user";
  const indefiniteHint = pause.source === "external-agent"
    ? "resumes automatically when the external run ends"
    : "until you resume";

  const call = async (method: string, params: any = {}) => {
    setBusy(true);
    try {
      await rpc(method, params);
    } finally {
      setBusy(false);
    }
  };

  const headline = isUser
    ? remaining === null
      ? "Queue paused"
      : "Queue paused"
    : systemHeadline(pause.source);

  return (
    <div className="ac-pause-band">
      {/* Announced to screen readers when the state changes, not on every tick. */}
      <div className="ac-sr" role="status" aria-live="polite">
        {headline}
        {remaining === null ? `, ${indefiniteHint}` : `, resuming in ${describeRemaining(remaining)}`}
      </div>

      <div className="ac-pause-head">
        <span className="ac-glyph paused" aria-hidden="true">
          ⏸
        </span>
        <span className="ac-pause-title">{headline}</span>
        {remaining !== null && (
          <span className="ac-muted ac-small">
            · resumes in <span className="ac-countdown">{formatCountdown(remaining)}</span>
          </span>
        )}
        {remaining === null && <span className="ac-muted ac-small">· {indefiniteHint}</span>}
      </div>

      <div className="ac-pause-sub">
        {waitingCount > 0
          ? `${waitingCount} run${waitingCount === 1 ? "" : "s"} waiting`
          : "Nothing waiting"}
        {pause.reason ? ` · “${pause.reason}”` : ""}
        {remaining !== null && until !== null ? ` · at ${formatClock(until)}` : ""}
        {pause.scope === "INCLUDE_ACTIVE" ? " · in-progress runs stopped at a safe point" : ""}
      </div>

      {pause.source !== "external-agent" && <div className="ac-pause-actions">
        {remaining !== null && (
          <>
            <button className="ac-chip" disabled={busy} onClick={() => void call("extendPause", { deltaMs: 15 * 60_000 })}>
              +15m
            </button>
            <button className="ac-chip" disabled={busy} onClick={() => void call("extendPause", { deltaMs: 60 * 60_000 })}>
              +1h
            </button>
            <button className="ac-chip" disabled={busy} onClick={() => void call("clearPauseTimer")}>
              Clear timer
            </button>
          </>
        )}
        <button className="ac-btn ac-btn-sm" disabled={busy} onClick={() => void call("resumeQueue")}>
          Resume now
        </button>
        {pause.source === "quota" && availableProvider && (
          <span className="ac-xs ac-muted">
            {availableProvider} still has quota — queue there instead.
          </span>
        )}
      </div>}
    </div>
  );
}

function systemHeadline(source: string): string {
  switch (source) {
    case "quota":
      return "Holding — out of quota";
    case "external-agent":
      return "Holding — an agent is running outside Agent Conductor";
    case "schedule":
      return "Holding — outside your working hours";
    default:
      return "Queue paused";
  }
}

/** Accepts `45m`, `90`, `3h`, `1h30m`, `2h 15m`. Returns null when unparseable. */
export function parseDuration(input: string): number | null {
  const s = input.trim().toLowerCase();
  if (s === "") {
    return null;
  }
  if (/^\d+$/.test(s)) {
    return Number(s) * 60_000; // bare number means minutes
  }
  // Longest alternatives first: `m|minutes` would match the "m" of
  // "minutes" and leave "inutes" behind.
  const re = /(\d+(?:\.\d+)?)\s*(hours|hour|hrs|hr|h|minutes|minute|mins|min|m)/g;
  let total = 0;
  let matched = false;
  let m: RegExpExecArray | null;
  while ((m = re.exec(s)) !== null) {
    matched = true;
    const value = Number(m[1]);
    total += m[2].startsWith("h") ? value * 3_600_000 : value * 60_000;
  }
  if (!matched || total <= 0) {
    return null;
  }
  // Leftover characters mean we did not understand the whole input.
  if (s.replace(re, "").trim() !== "") {
    return null;
  }
  return Math.round(total);
}
