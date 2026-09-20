// First-run readiness checks.
//
// Every check answers one question and carries the action that fixes it, so
// nothing about getting started requires reading the README (§7.1).
import * as vscode from "vscode";
import * as fs from "fs";
import * as os from "os";
import * as path from "path";
import { detectProviderRuntimes, resolveDaemonBinaryDetailed } from "./configuration";

export type CheckState = "ok" | "warn" | "fail" | "pending";

export interface SetupCheck {
  id: string;
  label: string;
  state: CheckState;
  detail: string;
  /** The command id that fixes it, run through the host. */
  fixCommand?: string;
  fixLabel?: string;
  /** A shell command the user can copy when no one-click fix exists. */
  copy?: string;
  /** Not required to start; shown but never blocks. */
  optional?: boolean;
}

export interface SetupStatus {
  checks: SetupCheck[];
  /** True when nothing required is failing. */
  ready: boolean;
}

/** True when the persistent service unit exists for this platform. */
export function serviceInstalled(): boolean {
  const home = os.homedir();
  const candidates =
    process.platform === "darwin"
      ? [path.join(home, "Library", "LaunchAgents", "local.agent-conductor.daemon.plist")]
      : [path.join(home, ".config", "systemd", "user", "agent-conductor.service")];
  return candidates.some((p) => fs.existsSync(p));
}

/** Whether Claude Code has recorded this folder as trusted. */
export function workspaceTrusted(cwd: string): boolean {
  try {
    const raw = fs.readFileSync(path.join(os.homedir(), ".claude.json"), "utf8");
    const cfg = JSON.parse(raw) as { projects?: Record<string, { hasTrustDialogAccepted?: boolean }> };
    return !!cfg.projects?.[path.resolve(cwd)]?.hasTrustDialogAccepted;
  } catch {
    return false;
  }
}

export function computeSetupStatus(
  context: Pick<vscode.ExtensionContext, "extensionPath">,
  opts: { daemonConnected: boolean; workspace: string }
): SetupStatus {
  const checks: SetupCheck[] = [];

  const bin = resolveDaemonBinaryDetailed(context);
  checks.push({
    id: "binary",
    label: "Agent Conductor daemon",
    state: bin.path ? "ok" : "fail",
    detail: bin.path ? bin.version || bin.path : bin.problem?.message ?? "not found",
    copy: bin.path ? undefined : bin.problem?.remedy,
  });

  checks.push({
    id: "connected",
    label: "Daemon reachable",
    state: opts.daemonConnected ? "ok" : bin.path ? "warn" : "fail",
    detail: opts.daemonConnected
      ? "connected"
      : bin.path
        ? "not running yet"
        : "nothing to start until the daemon binary exists",
    fixCommand: bin.path && !opts.daemonConnected ? "agent-conductor.installService" : undefined,
    fixLabel: "Start it",
    // Downstream of the binary check: carry its remedy so this line is never
    // a dead end either.
    copy: bin.path ? undefined : bin.problem?.remedy,
  });

  checks.push({
    id: "service",
    label: "Survives closing VS Code",
    state: serviceInstalled() ? "ok" : "warn",
    detail: serviceInstalled()
      ? "background service installed"
      : "runs stop when the daemon does",
    fixCommand: serviceInstalled() ? undefined : "agent-conductor.installService",
    fixLabel: "Install service",
  });

  const runtimes = detectProviderRuntimes();
  checks.push({
    id: "claude",
    label: "Claude Code",
    state: runtimes.claude.installed ? "ok" : "fail",
    detail: runtimes.claude.installed ? runtimes.claude.version || runtimes.claude.path : "not found on PATH",
    copy: runtimes.claude.installed ? undefined : "npm install -g @anthropic-ai/claude-code",
  });
  checks.push({
    id: "codex",
    label: "Codex",
    state: runtimes.codex.installed ? "ok" : "warn",
    detail: runtimes.codex.installed ? runtimes.codex.version || runtimes.codex.path : "not found — Claude alone is fine",
    optional: true,
    copy: runtimes.codex.installed ? undefined : "npm install -g @openai/codex",
  });

  checks.push({
    id: "workspace",
    label: "Workspace folder",
    state: opts.workspace ? "ok" : "fail",
    detail: opts.workspace || "open the folder you want agents to work in",
    fixCommand: opts.workspace ? undefined : "workbench.action.files.openFolder",
    fixLabel: "Open folder",
  });

  if (opts.workspace && runtimes.claude.installed) {
    const trusted = workspaceTrusted(opts.workspace);
    checks.push({
      id: "trust",
      label: "Claude Code trusts this folder",
      state: trusted ? "ok" : "warn",
      detail: trusted ? "trusted" : "Claude Code asks once before working in a new folder",
      fixCommand: trusted ? undefined : "agent-conductor.trustWorkspace",
      fixLabel: "Trust it",
    });
  }

  const ready = checks.every((c) => c.optional || c.state === "ok" || c.state === "warn");
  return { checks, ready };
}
