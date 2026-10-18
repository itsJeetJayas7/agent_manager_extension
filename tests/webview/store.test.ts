// @vitest-environment jsdom
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { initStore, useConductor, activeRuns, waitingRuns, pausedRuns } from "../../webview/src/state/store";
import { renderHook, act, cleanup } from "@testing-library/react";
import { makeSnapshot, makeRun, idlePause } from "./snapshot";

const postMessage = vi.fn();
let dispose: (() => void) | undefined;

beforeEach(() => {
  (window as any).__acVscode = { postMessage, getState: () => ({}), setState: () => undefined };
  postMessage.mockReset();
});

afterEach(() => {
  dispose?.();
  dispose = undefined;
  cleanup();
});

function send(data: unknown) {
  act(() => {
    window.dispatchEvent(new MessageEvent("message", { data }));
  });
}

describe("store", () => {
  it("adopts a full snapshot", () => {
    const { result } = renderHook(() => useConductor());
    act(() => {
      dispose = initStore();
    });
    send({ type: "snapshot", snapshot: makeSnapshot({ runs: [makeRun()] }) });
    expect(result.current.snapshot?.runs).toHaveLength(1);
  });

  // §2.5 — the webview used to request a whole snapshot on every single daemon
  // event. The daemon now sends the changed entity and the store merges it.
  it("merges a changed run without asking for a new snapshot", () => {
    const { result } = renderHook(() => useConductor());
    act(() => {
      dispose = initStore();
    });
    send({ type: "snapshot", snapshot: makeSnapshot({ runs: [makeRun({ id: "a", state: "QUEUED" })] }) });
    postMessage.mockReset();

    send({ type: "daemonEvent", event: "runUpdated", payload: makeRun({ id: "a", state: "RUNNING" }) });

    expect(result.current.snapshot?.runs[0].state).toBe("RUNNING");
    const askedForSnapshot = postMessage.mock.calls.some((c: any[]) => c[0]?.type === "requestSnapshot");
    expect(askedForSnapshot).toBe(false);
  });

  it("adds a run it has never seen before", () => {
    const { result } = renderHook(() => useConductor());
    act(() => {
      dispose = initStore();
    });
    send({ type: "snapshot", snapshot: makeSnapshot() });
    send({ type: "daemonEvent", event: "runUpdated", payload: makeRun({ id: "new", state: "QUEUED" }) });
    expect(result.current.snapshot?.runs.map((r) => r.id)).toContain("new");
  });

  it("keeps live arrivals and changed positions in saved queue order", () => {
    const { result } = renderHook(() => useConductor());
    act(() => { dispose = initStore(); });
    send({ type: "snapshot", snapshot: makeSnapshot({ runs: [makeRun({ id: "a", sortKey: 1024 })] }) });
    const update = (id: string, sortKey: number) => send({ type: "daemonEvent", event: "runUpdated", payload: makeRun({ id, sortKey }) });
    update("b", 2048);
    update("c", 3072);
    expect(waitingRuns(result.current).map(r => r.id)).toEqual(["a", "b", "c"]);
    update("urgent", -1024);
    update("b", 4096);
    expect(waitingRuns(result.current).map(r => r.id)).toEqual(["urgent", "a", "c", "b"]);
    // Reconnect with a differently ordered snapshot must show the same stack.
    send({ type: "snapshot", snapshot: makeSnapshot({ runs: [...result.current.snapshot!.runs].reverse() }) });
    expect(waitingRuns(result.current).map(r => r.id)).toEqual(["urgent", "a", "c", "b"]);
  });

  it("refreshes all positions after another window reorders the queue", () => {
    act(() => { dispose = initStore(); });
    postMessage.mockReset();
    send({ type: "daemonEvent", event: "queueReordered", payload: { runId: "b" } });
    expect(postMessage).toHaveBeenCalledWith({ type: "requestSnapshot" });
  });

  it("falls back to a snapshot when only an id arrives", () => {
    act(() => {
      dispose = initStore();
    });
    send({ type: "snapshot", snapshot: makeSnapshot() });
    postMessage.mockReset();
    send({ type: "daemonEvent", event: "runUpdated", payload: { id: "a" } });
    expect(postMessage.mock.calls.some((c: any[]) => c[0]?.type === "requestSnapshot")).toBe(true);
  });

  it("merges pause updates", () => {
    const { result } = renderHook(() => useConductor());
    act(() => {
      dispose = initStore();
    });
    send({ type: "snapshot", snapshot: makeSnapshot() });
    send({
      type: "daemonEvent",
      event: "pauseUpdated",
      payload: { ...idlePause, paused: true, reason: "lunch" },
    });
    expect(result.current.snapshot?.pause.paused).toBe(true);
    expect(result.current.snapshot?.paused).toBe(true);
    expect(result.current.snapshot?.pause.reason).toBe("lunch");
  });

  // detailBatchId used to be stashed via `(state as any)`.
  it("tracks the detail batch id as real typed state", () => {
    const { result } = renderHook(() => useConductor());
    act(() => {
      dispose = initStore();
    });
    send({ type: "detailBatchId", batchId: "b42" });
    expect(result.current.detailBatchId).toBe("b42");
  });

  it("records why the daemon is unreachable, with the remedy", () => {
    const { result } = renderHook(() => useConductor());
    act(() => {
      dispose = initStore();
    });
    send({
      type: "daemonOffline",
      reason: "quarantined",
      detail: "macOS quarantined the daemon.",
      remedy: "xattr -d com.apple.quarantine /path",
    });
    expect(result.current.daemonHealthy).toBe(false);
    expect(result.current.daemonProblem?.remedy).toContain("xattr");
  });

  it("keeps identity stable so useSyncExternalStore does not tear", () => {
    const { result, rerender } = renderHook(() => useConductor());
    act(() => {
      dispose = initStore();
    });
    send({ type: "snapshot", snapshot: makeSnapshot() });
    const first = result.current;
    rerender();
    expect(result.current).toBe(first);
  });
});

describe("selectors", () => {
  const snap = makeSnapshot({
    runs: [
      makeRun({ id: "run", state: "RUNNING" }),
      makeRun({ id: "queued", state: "QUEUED" }),
      makeRun({ id: "quota", state: "WAITING_FOR_QUOTA" }),
      makeRun({ id: "held", state: "PAUSED" }),
      makeRun({ id: "done", state: "COMPLETED" }),
    ],
  });
  const state = { snapshot: snap } as any;

  it("separates active, waiting and paused runs", () => {
    expect(activeRuns(state).map((r) => r.id)).toEqual(["run"]);
    expect(pausedRuns(state).map((r) => r.id)).toEqual(["held"]);
    // Quota waits retain their saved position; their reset hint explains the wait.
    expect(waitingRuns(state).map((r) => r.id)).toEqual(["queued", "quota"]);
  });
});
