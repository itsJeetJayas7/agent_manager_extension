// Central projection state: daemon snapshots + incremental events.
import { useEffect, useState } from "react";
import type { Snapshot, RunLike, BatchLike, RowLike } from "../../../src/extension/protocol";
import { vscodeApi, requestSnapshot, requestHostInfo } from "../api/client";

export interface ConductorState {
  snapshot: Snapshot | null;
  daemonHealthy: boolean;
  daemonText: string;
  host: string;
  workspace: string;
}

let state: ConductorState = { snapshot: null, daemonHealthy: false, daemonText: "connecting", host: "", workspace: "" };
const listeners = new Set<() => void>();

function emit() {
  state = { ...state };
  listeners.forEach((l) => l());
}

export function useConductor(): ConductorState {
  const [, force] = useState(0);
  useEffect(() => {
    const l = () => force((n) => n + 1);
    listeners.add(l);
    return () => {
      listeners.delete(l);
    };
  }, []);
  return state;
}

export function initStore(): () => void {
  const handler = (ev: MessageEvent) => {
    const m = ev.data;
    if (!m || typeof m !== "object") {
      return;
    }
    switch (m.type) {
      case "snapshot":
        state = { ...state, snapshot: m.snapshot as Snapshot };
        listeners.forEach((l) => l());
        break;
      case "daemonHealth":
        state.daemonHealthy = m.healthy;
        state.daemonText = m.text;
        emit();
        break;
      case "daemonOffline":
        state.daemonHealthy = false;
        state.daemonText = "daemon offline";
        emit();
        break;
      case "hostInfo":
        state.host = m.host;
        state.workspace = m.workspace;
        emit();
        break;
      case "detailBatchId":
        state = { ...state };
        (state as any).detailBatchId = m.batchId;
        emit();
        break;
      case "daemonEvent":
        if (m.event === "runUpdated" || m.event === "batchUpdated" || m.event === "quotaUpdated" ||
            m.event === "settingsUpdated" || m.event === "externalSessionUpdated" || m.event === "worktreeUpdated") {
          requestSnapshot();
        }
        break;
      default:
        break;
    }
  };
  window.addEventListener("message", handler);
  requestSnapshot();
  requestHostInfo();
  // periodic safety refresh (event-driven is primary)
  const timer = setInterval(requestSnapshot, 15000);
  return () => {
    window.removeEventListener("message", handler);
    clearInterval(timer);
  };
}
export function detailBatchId(): string | undefined {
  return (state as any).detailBatchId;
}

// ---- selectors ----

export function activeRuns(s: ConductorState): RunLike[] {
  const activeStates = new Set(["PREPARING", "STARTING", "RUNNING", "WAITING_FOR_PROVIDER", "VERIFYING", "INTEGRATING", "CANCEL_REQUESTED", "AUTO_RESUMING"]);
  return (s.snapshot?.runs ?? []).filter((r) => activeStates.has(r.state));
}

export function waitingRuns(s: ConductorState): RunLike[] {
  const waitStates = new Set(["QUEUED", "WAITING_FOR_SLOT", "WAITING_FOR_EXTERNAL_AGENT", "WAITING_FOR_QUOTA"]);
  return (s.snapshot?.runs ?? [])
    .filter((r) => waitStates.has(r.state))
    .sort((a, b) => (a.state === "WAITING_FOR_QUOTA" ? 1 : 0) - (b.state === "WAITING_FOR_QUOTA" ? 1 : 0));
}

export function attentionRuns(s: ConductorState): RunLike[] {
  const att = new Set(["NEEDS_ATTENTION", "FAILED", "BLOCKED", "ORPHANED", "AWAITING_INTEGRATION"]);
  return (s.snapshot?.runs ?? []).filter((r) => att.has(r.state));
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
