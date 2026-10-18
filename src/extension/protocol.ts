// Protocol types shared between extension host and webview projections.
// The webview never talks to the daemon directly.

export const PROTOCOL_VERSION = 1;

export interface RunActivityLike {
  observedAt: string;
  executionActive: boolean;
  processId?: number;
  processAlive?: boolean;
  lastEventAt?: string;
  entries: { at: string; kind: string; text: string }[];
  /** The daemon-owned provider process and the Claude Code tabs attached to it. */
  live?: { sessionId: string; pid: number; viewers: number; attaches: number; busy: "" | "conductor" | "native" };
}

export interface RunLike {
  id: string;
  batchId: string;
  rowId: string;
  cardIndex: number;
  /** Queue position; the snapshot already arrives in this order. */
  sortKey?: number;
  title: string;
  provider: string;
  model: string;
  effort: string;
  resolvedModel?: string;
  effectiveEffort?: string;
  retryCount?: number;
  completionRetries?: number;
  permission: string;
  staged: boolean;
  attachSecrets: boolean;
  state: string;
  stage: string;
  promptPreview: string;
  worktreePath?: string;
  branch?: string;
  providerSessionId?: string;
  providerTurnId?: string;
  providerTitle?: string;
  continuationCount: number;
  verifyCycles: number;
  errorCode?: string;
  errorMsg?: string;
  finalOutcome?: string;
  createdAt: string;
  startedAt?: string;
  finishedAt?: string;
  nextEligibleAt?: string;
  cwd: string;
}

export interface BatchLike {
  id: string;
  title: string;
  status: string;
  baseCwd: string;
  baseBranch: string;
  baseCommit: string;
  integrationBranch?: string;
  integrationWorktreePath?: string;
  createdAt: string;
  startedAt?: string;
  finishedAt?: string;
  applied: boolean;
  implicit: boolean;
}

export interface RowLike {
  id: string;
  batchId: string;
  position: number;
  mode: "LINEAR" | "PARALLEL";
  status: string;
}

export interface QuotaWindowLike {
  id: string;
  name: string;
  usedPercent: number;
  hasPercent: boolean;
  resetsAt: string;
  hasReset: boolean;
  exhausted: boolean;
  source: string;
  confidence: string;
}

export interface QuotaStateLike {
  windows: QuotaWindowLike[];
  blocked: boolean;
  confidence: string;
  nextEligibleAt?: string;
  missingReset?: boolean;
}

export interface ProviderCapsLike {
  installed: boolean;
  version: string;
  models: { id: string; name: string; efforts: string[]; hidden?: boolean }[];
  authState: string;
  planLabel: string;
  notes: string[];
  realTimeQuota: boolean;
}

export interface ExternalSessionLike {
  provider: string;
  nativeId: string;
  runId?: string;
  kind: string;
  cwd?: string;
  model?: string;
  title?: string;
  lastEvent: string;
  lastSeenAt: string;
}

/** Live advisory emitted only when an external turn starts during managed work. */
export interface ExternalRunOverlap {
  provider: string;
  sessionId: string;
  activeRunCount: number;
  queuePaused: boolean;
}

export interface SettingsLike {
  maxTotalRuns: number;
  maxClaudeRuns: number;
  maxCodexRuns: number;
  anchorCodexEnabled: boolean;
  anchorClaudeEnabled: boolean;
  anchorModelCodex: string;
  anchorModelClaude: string;
  claudeWeeklyResetUtc: string;
  claudePlanModel: string;
  claudePlanEffort: string;
  claudeBuildModel: string;
  claudeBuildEffort: string;
  claudeAuditModel: string;
  claudeAuditEffort: string;
  codexPlanModel: string;
  codexPlanEffort: string;
  codexBuildModel: string;
  codexBuildEffort: string;
  codexAuditModel: string;
  codexAuditEffort: string;
  auditEnabled: boolean;
  maxAuditCycles: number;
  maxVerifyFixCycles: number;
  maxTransientRetries?: number;
  maxCompletionContinuations?: number;
  verificationCommands: string[];
  verificationTimeoutSec: number;
  claudeApiMode: boolean;
  quietHoursEnabled: boolean;
  quietHoursStart: string;
  quietHoursEnd: string;
  /** Legacy mirror of pause.paused. */
  paused: boolean;
}

/** Why the queue is held. A user pause always outranks a system pause. */
export type PauseSource = "user" | "quota" | "external-agent" | "schedule" | "api";

/**
 * QUEUE_ONLY  — stop dispatching; runs already executing finish normally.
 * INCLUDE_ACTIVE — also ask in-flight runs to stop at a safe point, resumable
 *                  in the same native session.
 */
export type PauseScope = "QUEUE_ONLY" | "INCLUDE_ACTIVE";

export interface PauseStateLike {
  paused: boolean;
  scope: PauseScope;
  reason: string;
  pausedAt?: string;
  /** Absolute ISO timestamp; system holds may instead clear when their condition ends. */
  resumeAt?: string;
  /** The human input that produced resumeAt, e.g. "45m", for display only. */
  resumeAfter: string;
  source: PauseSource;
  autoResume: boolean;
}

export interface Snapshot {
  daemon: { version: string; pid: number; protocol: number; uptime: string };
  host: { hostname: string; platform: string };
  batches: BatchLike[];
  runs: RunLike[];
  rows: Record<string, RowLike[]>;
  external: Record<string, ExternalSessionLike[]>;
  quota: Record<string, QuotaStateLike>;
  providers: Record<string, ProviderCapsLike>;
  settings: SettingsLike;
  /** Legacy mirror of pause.paused; kept for older webviews. */
  paused: boolean;
  pause: PauseStateLike;
  /** True when a Shared Testing & Sudo block exists — never the block itself. */
  hasSecretBlock: boolean;
}

export interface RunEventLike {
  id: number;
  runId: string;
  timestamp: string;
  type: string;
  data?: string;
}

export interface EnqueueRunInput {
  prompt: string;
  provider: string;
  model?: string;
  effort?: string;
  permission?: string;
  attachSecrets?: boolean;
  staged?: boolean;
  cwd?: string;
  runNext?: boolean;
  title?: string;
}

export interface RowSpecInput {
  mode: "LINEAR" | "PARALLEL";
  runs: {
    prompt: string;
    provider: string;
    model?: string;
    effort?: string;
    permission?: string;
    attachSecrets?: boolean;
    staged?: boolean;
    title?: string;
  }[];
}

export interface BatchSpecInput {
  /** Stable key for retrying an import without creating a second batch. */
  importKey?: string;
  title: string;
  cwd: string;
  rows: RowSpecInput[];
}

export interface SequenceSettings {
  provider: string;
  model: string;
  effort: string;
  permission: string;
  attachSecrets: boolean;
  staged: boolean;
}

export interface PromptSequenceFile {
  name: string;
  order: number;
  bytes: number;
  prompt: string;
}

export interface PromptSequencePreview {
  id: string;
  folder: string;
  title: string;
  files: PromptSequenceFile[];
  ignored: string[];
  errors: string[];
  totalBytes: number;
}
