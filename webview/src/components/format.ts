// Simple duration / time formatting with tabular numerals.
export function fmtDuration(ms: number | undefined | null): string {
  if (!ms || ms < 0) {
    return "—";
  }
  const s = Math.floor(ms / 1000);
  if (s < 60) {
    return `${s}s`;
  }
  const m = Math.floor(s / 60);
  if (m < 60) {
    return `${m}m ${s % 60}s`;
  }
  const h = Math.floor(m / 60);
  if (h < 48) {
    return `${h}h ${m % 60}m`;
  }
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}

export function fmtRelative(iso: string | undefined): string {
  if (!iso) {
    return "—";
  }
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) {
    return "—";
  }
  const d = t - Date.now();
  const abs = Math.abs(d);
  const txt = fmtDuration(abs);
  return d >= 0 ? `in ${txt}` : `${txt} ago`;
}

export function fmtClock(iso: string | undefined): string {
  if (!iso) {
    return "—";
  }
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) {
    return "—";
  }
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

export function elapsedSince(iso: string | undefined, now = Date.now()): number {
  if (!iso) {
    return 0;
  }
  const t = new Date(iso).getTime();
  return Number.isNaN(t) ? 0 : Math.max(0, now - t);
}
