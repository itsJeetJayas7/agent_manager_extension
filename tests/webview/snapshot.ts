// Shared snapshot fixture for webview tests.
import type { Snapshot, RunLike, PauseStateLike } from "../../src/extension/protocol";

export const idlePause: PauseStateLike = {
  paused: false,
  scope: "QUEUE_ONLY",
  reason: "",
  resumeAfter: "",
  source: "user",
  autoResume: false,
};

export function makeSnapshot(
  over: Partial<Snapshot> & { runs?: RunLike[]; pause?: PauseStateLike } = {}
): Snapshot {
  return {
    daemon: { version: "0.1.0-test", pid: 1, protocol: 1, uptime: "10s" },
    host: { hostname: "test", platform: "darwin" },
    batches: [],
    runs: [],
    rows: {},
    external: { claude: [], codex: [] },
    quota: {
      claude: { windows: [], blocked: false, confidence: "unknown" },
      codex: {
        windows: [
          {
            id: "5h",
            name: "5-hour",
            usedPercent: 42,
            hasPercent: true,
            resetsAt: new Date(Date.now() + 3600_000).toISOString(),
            hasReset: true,
            exhausted: false,
            source: "provider_structured",
            confidence: "exact",
          },
        ],
        blocked: false,
        confidence: "exact",
      },
    },
    providers: {
      claude: {
        installed: true,
        version: "2.1.278",
        models: [
          { id: "sonnet", name: "Sonnet", efforts: ["low", "medium", "high"] },
          { id: "opus", name: "Opus", efforts: ["high", "max"] },
        ],
        authState: "ok",
        planLabel: "",
        notes: [],
        realTimeQuota: false,
      },
      codex: {
        installed: true,
        version: "1.0",
        models: [{ id: "gpt-x", name: "GPT X", efforts: ["low", "medium", "high"] }],
        authState: "ok",
        planLabel: "Pro",
        notes: [],
        realTimeQuota: true,
      },
    },
    settings: {
      maxTotalRuns: 2,
      maxClaudeRuns: 2,
      maxCodexRuns: 2,
      anchorCodexEnabled: false,
      anchorClaudeEnabled: false,
      anchorModelCodex: "auto",
      anchorModelClaude: "auto",
      claudeWeeklyResetUtc: "",
      claudePlanModel: "opus",
      claudePlanEffort: "high",
      claudeBuildModel: "sonnet",
      claudeBuildEffort: "high",
      claudeAuditModel: "opus",
      claudeAuditEffort: "high",
      codexPlanModel: "",
      codexPlanEffort: "high",
      codexBuildModel: "",
      codexBuildEffort: "medium",
      codexAuditModel: "",
      codexAuditEffort: "high",
      auditEnabled: false,
      maxAuditCycles: 1,
      maxVerifyFixCycles: 2,
      verificationCommands: [],
      verificationTimeoutSec: 1800,
      claudeApiMode: false,
      paused: false,
    },
    paused: false,
    pause: idlePause,
    hasSecretBlock: false,
    ...over,
  } as Snapshot;
}

export function makeRun(over: Partial<RunLike> = {}): RunLike {
  return {
    id: "r1",
    batchId: "b1",
    rowId: "row1",
    cardIndex: 0,
    title: "Auth rewrite",
    provider: "claude",
    model: "sonnet",
    effort: "high",
    permission: "bypass",
    staged: false,
    attachSecrets: false,
    state: "QUEUED",
    stage: "",
    promptPreview: "Auth rewrite",
    continuationCount: 0,
    verifyCycles: 0,
    createdAt: new Date().toISOString(),
    cwd: "/w",
    ...over,
  } as RunLike;
}

/** The shape QueueView/ControlView receive. */
export function makeState(snapshot: Snapshot, over: Record<string, unknown> = {}) {
  return {
    snapshot,
    daemonHealthy: true,
    daemonText: "ok",
    daemonProblem: null,
    host: "Local",
    workspace: "/tmp/repo",
    detailBatchId: "",
    revision: 1,
    ...over,
  } as any;
}
