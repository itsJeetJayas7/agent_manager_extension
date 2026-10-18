import React from "react";
import { beforeEach, afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import RunActivity from "../../webview/src/components/RunActivity";
import { makeRun } from "./snapshot";
import { rpc } from "../../webview/src/api/client";

vi.mock("../../webview/src/api/client", () => ({ rpc: vi.fn(), vscodeApi: {} }));
let now = Date.now();
vi.mock("../../webview/src/components/ticker", () => ({ useNow: () => now }));
const run = makeRun({ id: "live", state: "RUNNING" });
function activity(patch: object = {}) {
  return { observedAt: new Date(now).toISOString(), executionActive: true, processId: 1234, processAlive: true,
    lastEventAt: new Date(now - 3000).toISOString(), entries: [{ at: new Date(now).toISOString(), kind: "tool", text: "Using Edit" }], ...patch };
}
beforeEach(() => { now = Date.now(); vi.resetAllMocks(); vi.mocked(rpc).mockResolvedValue(activity()); });
afterEach(cleanup);

describe("observable run activity", () => {
  it("shows process evidence and provider output independently of the RUNNING label", async () => {
    render(<RunActivity run={run} />);
    expect(await screen.findByText("Process alive · PID 1234")).toBeTruthy();
    expect(screen.getByText("Using Edit")).toBeTruthy();
    expect(screen.getByText(/Last provider event.*ago/)).toBeTruthy();
    expect(rpc).toHaveBeenCalledWith("getRunActivity", { runId: "live" });
  });
  it("calls out a stale saved state when the daemon has no active worker", async () => {
    vi.mocked(rpc).mockResolvedValue(activity({ executionActive: false, processAlive: false }));
    render(<RunActivity run={run} />);
    expect(await screen.findByText("No active run worker")).toBeTruthy();
    expect(screen.getByText(/saved state says active/)).toBeTruthy();
  });
  it("does not mistake verification for an orphaned provider turn", async () => {
    vi.mocked(rpc).mockResolvedValue(activity({ executionActive: false, processAlive: false }));
    render(<RunActivity run={makeRun({ state: "VERIFYING" })} />);
    expect(await screen.findByText("Verifying result · provider turn finished")).toBeTruthy();
    expect(screen.queryByText(/saved state says active/)).toBeNull();
  });
  it("renders a just-arrived event without an empty duration", async () => {
    vi.mocked(rpc).mockResolvedValue(activity({ lastEventAt: new Date(now).toISOString() }));
    render(<RunActivity run={run} />);
    expect(await screen.findByText("Last provider event just now")).toBeTruthy();
  });
  it("does not equate a quiet live process with progress or failure", async () => {
    vi.mocked(rpc).mockResolvedValue(activity({ lastEventAt: new Date(now - 120000).toISOString() }));
    render(<RunActivity run={run} />);
    await screen.findByText("Process alive · PID 1234");
    expect(screen.getByText(/progress is unconfirmed/)).toBeTruthy();
  });
  it("reports a dead child while the worker is still unwinding", async () => {
    vi.mocked(rpc).mockResolvedValue(activity({ processAlive: false }));
    render(<RunActivity run={run} />);
    expect(await screen.findByText(/Provider process exited/)).toBeTruthy();
  });
  it("does not keep claiming live status after disconnect", async () => {
    const { rerender } = render(<RunActivity run={run} />);
    await screen.findByText("Process alive · PID 1234");
    rerender(<RunActivity run={run} connected={false} />);
    expect(screen.getByText("Live status unavailable")).toBeTruthy();
    expect(screen.queryByText("Process alive · PID 1234")).toBeNull();
    expect(screen.getByText("Using Edit")).toBeTruthy();
  });
  it("expires old process evidence even while a new RPC is hung", async () => {
    const { rerender } = render(<RunActivity run={run} />);
    await screen.findByText("Process alive · PID 1234");
    now += 11000;
    rerender(<RunActivity run={run} />);
    expect(screen.getByText("Live status unavailable")).toBeTruthy();
  });
  it("allows retry after an unavailable or older daemon", async () => {
    vi.mocked(rpc).mockRejectedValueOnce(new Error("UNKNOWN_METHOD: getRunActivity"));
    render(<RunActivity run={run} />);
    expect(await screen.findByText("Live status unavailable")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Check again" }));
    await screen.findByText("Process alive · PID 1234");
  });
  it("polls again and cancels timers after unmount", async () => {
    vi.mocked(rpc).mockResolvedValueOnce(activity()).mockResolvedValue(activity({ entries: [{ at: new Date(now).toISOString(), kind: "message", text: "Verified the fix" }] }));
    const { unmount } = render(<RunActivity run={run} />);
    await screen.findByText("Using Edit");
    await waitFor(() => expect(screen.getByText("Verified the fix")).toBeTruthy(), { timeout: 3500 });
    unmount();
    const calls = vi.mocked(rpc).mock.calls.length;
    await new Promise(resolve => setTimeout(resolve, 2100));
    expect(rpc).toHaveBeenCalledTimes(calls);
  });
});
