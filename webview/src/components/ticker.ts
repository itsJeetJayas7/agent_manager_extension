// A single 1s interval for the whole app.
//
// Countdowns are computed from an absolute timestamp on every tick, never by
// decrementing a local counter: a decrementing counter drifts and is simply
// wrong after the machine sleeps.
import { useSyncExternalStore } from "react";

let timer: number | null = null;
let now = Date.now();
const listeners = new Set<() => void>();

function tick(): void {
  now = Date.now();
  for (const l of listeners) {
    l();
  }
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  if (timer === null) {
    now = Date.now();
    timer = window.setInterval(tick, 1000);
  }
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0 && timer !== null) {
      window.clearInterval(timer);
      timer = null;
    }
  };
}

function getNow(): number {
  return now;
}

/** Current wall-clock time, re-rendering the caller once per second. */
export function useNow(): number {
  return useSyncExternalStore(subscribe, getNow, getNow);
}

/**
 * `mm:ss` under an hour, `h:mm:ss` above — tabular figures, so the width never
 * changes as the digits do.
 */
export function formatCountdown(ms: number): string {
  const total = Math.max(0, Math.round(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  const pad = (n: number) => String(n).padStart(2, "0");
  return h > 0 ? `${h}:${pad(m)}:${pad(s)}` : `${pad(m)}:${pad(s)}`;
}

/** "in 45 minutes" style phrasing for screen readers and tooltips. */
export function describeRemaining(ms: number): string {
  const total = Math.max(0, Math.round(ms / 1000));
  if (total < 60) {
    return `${total} second${total === 1 ? "" : "s"}`;
  }
  const m = Math.round(total / 60);
  if (m < 60) {
    return `${m} minute${m === 1 ? "" : "s"}`;
  }
  const h = Math.floor(m / 60);
  const rem = m % 60;
  return rem === 0 ? `${h} hour${h === 1 ? "" : "s"}` : `${h}h ${rem}m`;
}

/** Absolute clock time, for "resumes at 3:15 PM". */
export function formatClock(ms: number): string {
  return new Date(ms).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
}
