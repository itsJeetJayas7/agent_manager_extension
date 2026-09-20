// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import React from "react";
import QueueView from "../../webview/src/views/QueueView";
import { initStore } from "../../webview/src/state/store";
import type { Snapshot } from "../../src/extension/protocol";

function fakeSnapshot(runs: any[] = [], paused = false): Snapshot {
  return {
    daemon: { version: "0.1.0-test", pid: 1, protocol: 1, uptime: "10s" },
    host: { hostname: "test", platform: "darwin" },
    batches: [],
    runs,
    rows: {},
    external: { claude: [], codex: [] },
    quota: {
      claude: { windows: [], blocked: false, confidence: "unknown" },
      codex: { windows: [{ id: "5h", name: "5-hour", usedPercent: 42, hasPercent: true, resetsAt: new Date(Date.now() + 3600_000).toISOString(), hasReset: true, exhausted: false, source: "provider_structured", confidence: "exact" }], blocked: false, confidence: "exact" },
    },
    providers: {
      claude: { installed: true, version: "2.0", models: [{ id: "sonnet", name: "Sonnet", efforts: ["high"] }, { id: "opus", name: "Opus", efforts: ["high", "max"] }], authState: "ok", planLabel: "", notes: [], realTimeQuota: false },
      codex: { installed: true, version: "1.0", models: [{ id: "gpt-x", name: "GPT X", efforts: ["low", "medium", "high"] }], authState: "ok", planLabel: "Pro", notes: [], realTimeQuota: true },
    },
    settings: {
      maxTotalRuns: 2, maxClaudeRuns: 2, maxCodexRuns: 2,
      anchorCodexEnabled: false, anchorClaudeEnabled: false,
      anchorModelCodex: "auto", anchorModelClaude: "auto", claudeWeeklyResetUtc: "",
      claudePlanModel: "opus", claudePlanEffort: "high",
      claudeBuildModel: "sonnet", claudeBuildEffort: "high",
      claudeAuditModel: "opus", claudeAuditEffort: "high",
      codexPlanModel: "", codexPlanEffort: "high",
      codexBuildModel: "", codexBuildEffort: "medium",
      codexAuditModel: "", codexAuditEffort: "high",
      auditEnabled: false, maxAuditCycles: 1, maxVerifyFixCycles: 2,
      verificationCommands: [], claudeApiMode: false, fifoBlockOnExternal: true,
      paused,
    },
    paused,
  };
}

const postMessage = vi.fn();
beforeEach(() => {
  (window as any).__acVscode = { postMessage, getState: () => ({}), setState: () => undefined };
  postMessage.mockReset();
  // respond to rpc calls like the extension host would
  postMessage.mockImplementation((msg: any) => {
    if (msg?.type === "rpc" && typeof msg.requestId === "number") {
      const result = msg.method === "enqueueRun" ? { runId: "r1", batchId: "b1" } : {};
      setTimeout(() => {
        window.dispatchEvent(new MessageEvent("message", { data: { type: "rpcResult", requestId: msg.requestId, result } }));
      }, 0);
    }
  });
});

describe("QueueView", () => {
  it("renders composer and empty queue", () => {
    render(<QueueView state={{ snapshot: fakeSnapshot(), daemonHealthy: true, daemonText: "ok", host: "", workspace: "/tmp/repo" } as any} />);
    expect(screen.getByLabelText("Prompt")).toBeTruthy();
    expect(screen.getByText(/queue is empty/i)).toBeTruthy();
    expect(screen.getAllByRole("button", { name: "+ Queue Run" }).length).toBeGreaterThan(0);
  });

  it("queues a run with composer fields via Cmd+Enter", async () => {
    render(<QueueView state={{ snapshot: fakeSnapshot(), daemonHealthy: true, daemonText: "", host: "", workspace: "/w" } as any} />);
    const ta = screen.getByLabelText("Prompt") as HTMLTextAreaElement;
    fireEvent.change(ta, { target: { value: "Do the thing" } });
    fireEvent.keyDown(ta, { key: "Enter", metaKey: true });
    await waitFor(() => {
      const call = (postMessage as any).mock.calls.find((c: any[]) => c[0]?.type === "rpc" && c[0].method === "enqueueRun");
      expect(call).toBeTruthy();
      expect(call![0].params.prompt).toBe("Do the thing");
      expect(call![0].params.permission).toBe("bypass");
    });
  });

  it("shows an active run with provider opener", () => {
    const run = {
      id: "r1", batchId: "b1", rowId: "row1", cardIndex: 0, title: "Auth rewrite",
      provider: "claude", model: "sonnet", effort: "high", permission: "bypass",
      staged: false, attachSecrets: false, state: "RUNNING", stage: "",
      promptPreview: "Auth rewrite", providerSessionId: "sess-123",
      startedAt: new Date().toISOString(), continuationCount: 0, verifyCycles: 0, cwd: "/w",
    };
    render(<QueueView state={{ snapshot: fakeSnapshot([run]), daemonHealthy: true, daemonText: "", host: "", workspace: "/w" } as any} />);
    expect(screen.getByText("Open in Claude Code")).toBeTruthy();
    expect(screen.getByText("Running")).toBeTruthy();
  });

  it("marks quota-waiting runs with a reset hint", () => {
    const run = {
      id: "r2", batchId: "b2", rowId: "row2", cardIndex: 0, title: "Blocked task",
      provider: "codex", model: "", effort: "", permission: "bypass",
      staged: false, attachSecrets: false, state: "WAITING_FOR_QUOTA", stage: "",
      promptPreview: "Blocked task", nextEligibleAt: new Date(Date.now() + 3600_000).toISOString(),
      continuationCount: 1, verifyCycles: 0, cwd: "/w",
    };
    render(<QueueView state={{ snapshot: fakeSnapshot([run]), daemonHealthy: true, daemonText: "", host: "", workspace: "/w" } as any} />);
    expect(screen.getByTitle(/resumes automatically/i)).toBeTruthy();
  });
});
