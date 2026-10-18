import React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import App from "../../webview/src/App";
import ControlView from "../../webview/src/views/ControlView";
import { makeSnapshot, makeState } from "./snapshot";

const note = "Measured wall-clock time only. Agent Conductor does not estimate spend.";
const emptyUsage = {
  runs: 0, completed: 0, failed: 0, activeMs: 0, quotaWaitMs: 0,
  continuations: 0, byProviderActiveMs: {}, recent: null, note,
};
let usage: Record<string, unknown>;

beforeEach(() => {
  usage = { ...emptyUsage };
  window.__acVscode = {
    getState: () => ({}),
    setState: () => undefined,
    postMessage: vi.fn((message: any) => {
      if (message.type === "rpc" && message.method === "usageSummary") {
        // Exercise the real asynchronous host bridge, including the legacy
        // daemon's JSON null for an empty Go slice.
        setTimeout(() => window.dispatchEvent(new MessageEvent("message", {
          data: { type: "rpcResult", requestId: message.requestId, result: usage },
        })), 0);
      }
    }),
  };
});

afterEach(() => {
  cleanup();
  delete window.__acVscode;
});

function openControl() {
  render(<App mode="sidebar" />);
  act(() => window.dispatchEvent(new MessageEvent("message", {
    data: { type: "snapshot", snapshot: makeSnapshot() },
  })));
  fireEvent.click(screen.getByRole("tab", { name: "CONTROL" }));
}

describe("Control tab usage", () => {
  it.each([
    ["null", null],
    ["missing", undefined],
    ["empty", []],
  ])("keeps the sidebar usable with %s recent history", async (_label, recent) => {
    usage.recent = recent;
    openControl();
    await screen.findByText(note);
    expect(screen.getByText("0 · 0 completed · 0 needed you")).toBeTruthy();
    expect(screen.queryByText("Longest runs")).toBeNull();

    fireEvent.click(screen.getByRole("tab", { name: "QUEUE" }));
    expect(screen.getByLabelText("Prompt")).toBeTruthy();
    fireEvent.click(screen.getByRole("tab", { name: "CONTROL" }));
    await screen.findByText(note);
  });

  it("still renders recent runs and can refresh to empty history", async () => {
    usage = {
      ...emptyUsage, runs: 1, completed: 1, activeMs: 120_000,
      recent: [{ runId: "r1", title: "Finished task", provider: "claude", activeMs: 120_000, continuations: 0 }],
    };
    openControl();
    await screen.findByText("Finished task");
    expect(screen.getByText("1 · 1 completed · 0 needed you")).toBeTruthy();
    // Opening the disclosure makes its Refresh button accessible.
    screen.getByText("This week").closest("details")!.open = true;
    usage = { ...emptyUsage };
    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await screen.findByText("0 · 0 completed · 0 needed you");
    expect(screen.queryByText("Longest runs")).toBeNull();
  });
});

describe("Control external activity", () => {
  it("explains concurrent external work and removes the hold setting", async () => {
    render(<ControlView state={makeState(makeSnapshot())} />);
    await screen.findByText(note);
    expect(screen.getByText(/external agents can run alongside the queue/i)).toBeTruthy();
    expect(screen.queryByLabelText(/hold the queue while an agent runs outside/i)).toBeNull();
  });

  it.each([
    ["SessionStart", false], ["UserPromptSubmit", true], ["PreToolUse", true],
    ["PostToolUseFailure", true], ["Stop", false],
  ])("reports %s consistently with the queue", async (lastEvent, busy) => {
    const external = { claude: [{
      provider: "claude", nativeId: "manual", kind: "external", cwd: "/project",
      lastEvent, lastSeenAt: new Date().toISOString(),
    }] };
    render(<ControlView state={makeState(makeSnapshot({ external }))} />);
    await screen.findByText(note);
    expect(!!screen.queryByText("running in /project")).toBe(busy);
    expect(!!screen.queryByText("Clear stale busy state")).toBe(busy);
  });
});
