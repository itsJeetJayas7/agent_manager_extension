// Central projection state: daemon snapshots merged with incremental events.
//
// Rewritten on useSyncExternalStore. The previous store kept module-level
// mutable state and forced re-renders with useState(0) plus a listener Set,
// which tears under React 18 concurrent rendering, and stashed detailBatchId
// through `(state as any)`.
import { useCallback, useRef, useSyncExternalStore } from "react";
import type {
  Snapshot,
  RunLike,
  BatchLike,
  RowLike,
  PauseStateLike,
} from "../../../src/extension/protocol";
import { vscodeApi, requestSnapshot, requestHostInfo } from "../api/client";

/** Why the daemon is unreachable, with the exact command that fixes it. */
export interface DaemonProblem {
  reason: string;
  detail?: string;
  remedy?: string;
}

export interface ConductorState {
  snapshot: Snapshot | null;
  daemonHealthy: boolean;
  daemonText: string;
  daemonProblem: DaemonProblem | null;
  host: string;
  workspace: string;
  detailBatchId: string;
  /** Bumped on every accepted update; lets views cheaply detect staleness. */
  revision: number;
}

const initial: ConductorState = {
  snapshot: null,
  daemonHealthy: false,
  daemonText: "connecting",
  daemonProblem: null,
  host: "",
  workspace: "",
  detailBatchId: "",
  revision: 0,
};

let state: ConductorState = initial;
const listeners = new Set<() => void>();

function commit(patch: Partial<ConductorState>): void {
  state = { ...state, ...patch, revision: state.revision + 1 };
  for (const l of listeners) {
    l();
  }
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function getState(): ConductorState {
  return state;
}

export function useConductor(): ConductorState {
  return useSyncExternalStore(subscribe, getState, getState);
}

/**
 * Subscribe to a slice of the store, so a run update does not re-render the
 * whole panel. `isEqual` defaults to Object.is; pass a shallow comparator for
 * derived arrays.
 */
export function useSelector<T>(
  selector: (s: ConductorState) => T,
  isEqual: (a: T, b: T) => boolean = Object.is
): T {
  const cache = useRef<{ value: T; has: boolean }>({ value: undefined as unknown as T, has: false });
  const snap = useCallback(() => {
    const next = selector(state);
    if (!cache.current.has || !isEqual(cache.current.value, next)) {
      cache.current = { value: next, has: true };
    }
    return cache.current.value;
    // selector/isEqual are expected to be stable (module-level functions).
  }, [selector, isEqual]);
  return useSyncExternalStore(subscribe, snap, snap);
}

/** Shallow array comparison for list selectors. */
export function sameList<T>(a: readonly T[], b: readonly T[]): boolean {
  if (a === b) {
    return true;
  }
  if (a.length !== b.length) {
    return false;
  }
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i]) {
      return false;
    }
  }
  return true;
}

// ---- incremental merge ----

function mergeRun(run: RunLike): void {
  const snap = state.snapshot;
  if (!snap || !run?.id) {
    return;
  }
  const runs = snap.runs ?? [];
  const i = runs.findIndex((r) => r.id === run.id);
  const next = i >= 0 ? [...runs.slice(0, i), run, ...runs.slice(i + 1)] : [run, ...runs];
  commit({ snapshot: { ...snap, runs: next } });
}

function mergeBatch(batch: BatchLike, rows: RowLike[] | undefined): void {
  const snap = state.snapshot;
  if (!snap || !batch?.id) {
    return;
  }
  const batches = snap.batches ?? [];
  const i = batches.findIndex((b) => b.id === batch.id);
  const nextBatches = i >= 0 ? [...batches.slice(0, i), batch, ...batches.slice(i + 1)] : [batch, ...batches];
  const nextRows = rows ? { ...(snap.rows ?? {}), [batch.id]: rows } : snap.rows;
  commit({ snapshot: { ...snap, batches: nextBatches, rows: nextRows } });
}

function mergePause(pause: PauseStateLike): void {
  const snap = state.snapshot;
  if (!snap || !pause) {
    return;
  }
  commit({ snapshot: { ...snap, pause, paused: !!pause.paused } });
}

/**
 * Reconciliation interval. Incremental events are primary; this is a slow
 * safety net, not the mechanism. It used to be a blanket 15s poll that also
 * fired a full snapshot request on every single daemon event.
 */
const RECONCILE_MS = 60_000;

export function initStore(): () => void {
  const handler = (ev: MessageEvent) => {
    const m = ev.data;
    if (!m || typeof m !== "object") {
      return;
    }
    switch (m.type) {
      case "snapshot":
        commit({ snapshot: m.snapshot as Snapshot, daemonProblem: null });
        break;
      case "daemonHealth":
        commit({ daemonHealthy: !!m.healthy, daemonText: String(m.text ?? "") });
        break;
      case "daemonOnline":
        commit({ daemonHealthy: true, daemonProblem: null });
        break;
      case "daemonOffline":
        commit({
          daemonHealthy: false,
          daemonText: "daemon offline",
          daemonProblem: { reason: String(m.reason ?? "offline"), detail: m.detail, remedy: m.remedy },
        });
        break;
      case "hostInfo":
        commit({ host: String(m.host ?? ""), workspace: String(m.workspace ?? "") });
        break;
      case "detailBatchId":
        commit({ detailBatchId: String(m.batchId ?? "") });
        break;
      case "daemonEvent":
        onDaemonEvent(m.event, m.payload);
        break;
      default:
        break;
    }
  };
  window.addEventListener("message", handler);
  requestSnapshot();
  requestHostInfo();
  const timer = window.setInterval(requestSnapshot, RECONCILE_MS);
  return () => {
    window.removeEventListener("message", handler);
    window.clearInterval(timer);
  };
}

function onDaemonEvent(event: string, payload: any): void {
  switch (event) {
    case "runUpdated":
      if (payload && typeof payload === "object" && payload.state) {
        mergeRun(payload as RunLike);
      } else {
        requestSnapshot(); // id-only fallback
      }
      return;
    case "batchUpdated":
      if (payload?.batch) {
        mergeBatch(payload.batch as BatchLike, payload.rows as RowLike[] | undefined);
      } else {
        requestSnapshot();
      }
      return;
    case "pauseUpdated":
      if (payload && typeof payload === "object") {
        mergePause(payload as PauseStateLike);
      }
      return;
    case "quotaUpdated":
    case "settingsUpdated":
    case "externalSessionUpdated":
    case "worktreeUpdated":
      // Small, infrequent: a snapshot is cheaper than bespoke merge paths.
      requestSnapshot();
      return;
    default:
      return;
  }
}

export function detailBatchId(): string | undefined {
  return state.detailBatchId || undefined;
}

// ---- selectors ----

const ACTIVE_STATES = new Set([
  "PREPARING", "STARTING", "RUNNING", "WAITING_FOR_PROVIDER",
  "VERIFYING", "INTEGRATING", "CANCEL_REQUESTED", "AUTO_RESUMING",
]);
const WAITING_STATES = new Set([
  "QUEUED", "WAITING_FOR_SLOT", "WAITING_FOR_EXTERNAL_AGENT", "WAITING_FOR_QUOTA",
]);
const ATTENTION_STATES = new Set([
  "NEEDS_ATTENTION", "FAILED", "BLOCKED", "ORPHANED", "AWAITING_INTEGRATION",
]);

export function activeRuns(s: ConductorState): RunLike[] {
  return (s.snapshot?.runs ?? []).filter((r) => ACTIVE_STATES.has(r.state));
}

/** Runs parked by an INCLUDE_ACTIVE pause: stopped safely, resumable. */
export function pausedRuns(s: ConductorState): RunLike[] {
  return (s.snapshot?.runs ?? []).filter((r) => r.state === "PAUSED");
}

export function waitingRuns(s: ConductorState): RunLike[] {
  return (s.snapshot?.runs ?? [])
    .filter((r) => WAITING_STATES.has(r.state))
    .sort((a, b) => (a.state === "WAITING_FOR_QUOTA" ? 1 : 0) - (b.state === "WAITING_FOR_QUOTA" ? 1 : 0));
}

export function attentionRuns(s: ConductorState): RunLike[] {
  return (s.snapshot?.runs ?? []).filter((r) => ATTENTION_STATES.has(r.state));
}

export function pauseState(s: ConductorState): PauseStateLike | null {
  return s.snapshot?.pause ?? null;
}

export function recentBatches(s: ConductorState): BatchLike[] {
  return (s.snapshot?.batches ?? []).slice(0, 50);
}

export function rowsOf(s: ConductorState, batchId: string): RowLike[] {
  return s.snapshot?.rows?.[batchId] ?? [];
}

export function runsOfBatch(s: ConductorState, batchId: string): RunLike[] {
  return (s.snapshot?.runs ?? []).filter((r) => r.batchId === batchId);
}

export const _vscodeApi = vscodeApi;
