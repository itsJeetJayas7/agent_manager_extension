// Durable notifications: tell the user what happened while they were away.
//
// Runs continue in the daemon after VS Code closes, so without this the only
// way to learn an overnight batch failed is to go looking (§7.2).
import * as vscode from "vscode";
import type { Snapshot, RunLike, ExternalRunOverlap } from "./protocol";

const LAST_SEEN_KEY = "agentConductor.lastSeenRunAt";
const NOTIFIED_KEY = "agentConductor.notifiedRunIds";

export interface AwaySummary {
  completed: number;
  failed: number;
  attention: number;
  since: number;
}

function finishedAt(run: RunLike): number {
  const t = run.finishedAt ? new Date(run.finishedAt).getTime() : 0;
  return Number.isNaN(t) ? 0 : t;
}

/** True while the user has asked not to be interrupted. */
export function inQuietHours(snapshot: Snapshot | null, now = new Date()): boolean {
  const s = snapshot?.settings;
  if (!s?.quietHoursEnabled) {
    return false;
  }
  const parse = (v: string): number | null => {
    const m = /^(\d{1,2}):(\d{2})$/.exec((v ?? "").trim());
    if (!m) {
      return null;
    }
    const h = Number(m[1]);
    const min = Number(m[2]);
    return h >= 0 && h < 24 && min >= 0 && min < 60 ? h * 60 + min : null;
  };
  const start = parse(s.quietHoursStart);
  const end = parse(s.quietHoursEnd);
  if (start === null || end === null || start === end) {
    return false;
  }
  const cur = now.getHours() * 60 + now.getMinutes();
  // Inside working hours means NOT quiet.
  const working = start < end ? cur >= start && cur < end : cur >= start || cur < end;
  return !working;
}

export class Notifier {
  constructor(private memento: vscode.Memento) {}

  /** Live feedback about a manual turn; never pauses anything on its own. */
  async reportExternalOverlap(payload: unknown): Promise<void> {
    if (vscode.workspace.getConfiguration("agentConductor").get<boolean>("notifyOnExternalOverlap") === false) {
      return;
    }
    if (!payload || typeof payload !== "object") {
      return;
    }
    const event = payload as Partial<ExternalRunOverlap>;
    const provider = event.provider === "claude" ? "Claude Code" : event.provider === "codex" ? "Codex" : null;
    const count = event.activeRunCount;
    if (!provider || typeof count !== "number" || !Number.isSafeInteger(count) || count < 1) {
      return;
    }
    const status = event.queuePaused
      ? `${count} run${count === 1 ? " is" : "s are"} still active while the queue is paused`
      : `the queue is running right now (${count} active run${count === 1 ? "" : "s"})`;
    const actions = event.queuePaused ? ["Show queue"] : ["Show queue", "Pause queue…"];
    const choice = await vscode.window.showInformationMessage(
      `Agent Conductor: ${status}. Your external ${provider} run will run alongside it.`, ...actions
    );
    if (choice === "Show queue") {
      await vscode.commands.executeCommand("agent-conductor.open");
    } else if (choice === "Pause queue…") {
      await vscode.commands.executeCommand("agent-conductor.pauseQueue");
    }
  }

  private lastSeen(): number {
    return this.memento.get<number>(LAST_SEEN_KEY, 0);
  }

  private notified(): Set<string> {
    return new Set(this.memento.get<string[]>(NOTIFIED_KEY, []));
  }

  private async remember(ids: Set<string>, latest: number): Promise<void> {
    // Keep the set bounded; only recent ids can ever match again.
    const trimmed = [...ids].slice(-500);
    await this.memento.update(NOTIFIED_KEY, trimmed);
    await this.memento.update(LAST_SEEN_KEY, latest);
  }

  /** What finished while the extension was not watching. */
  summariseAway(snapshot: Snapshot): AwaySummary | null {
    const since = this.lastSeen();
    if (since === 0) {
      return null; // first ever run: nothing to catch up on
    }
    const seen = this.notified();
    let completed = 0;
    let failed = 0;
    let attention = 0;
    for (const r of snapshot.runs ?? []) {
      const at = finishedAt(r);
      if (at <= since || seen.has(r.id)) {
        continue;
      }
      if (r.state === "COMPLETED") {
        completed++;
      } else if (r.state === "FAILED") {
        failed++;
      } else if (r.state === "NEEDS_ATTENTION" || r.state === "BLOCKED" || r.state === "AWAITING_INTEGRATION") {
        attention++;
      }
    }
    if (completed + failed + attention === 0) {
      return null;
    }
    return { completed, failed, attention, since };
  }

  /** Record everything currently terminal as already reported. */
  async markSeen(snapshot: Snapshot): Promise<void> {
    const seen = this.notified();
    let latest = this.lastSeen();
    for (const r of snapshot.runs ?? []) {
      const at = finishedAt(r);
      if (at > 0) {
        seen.add(r.id);
        latest = Math.max(latest, at);
      }
    }
    await this.remember(seen, Math.max(latest, Date.now() - 1));
  }

  /** Show the away summary, unless the user asked for quiet. */
  async reportAway(snapshot: Snapshot): Promise<void> {
    const summary = this.summariseAway(snapshot);
    await this.markSeen(snapshot);
    if (!summary || inQuietHours(snapshot)) {
      return;
    }
    const bits: string[] = [];
    if (summary.completed > 0) {
      bits.push(`${summary.completed} run${summary.completed === 1 ? "" : "s"} completed`);
    }
    if (summary.attention > 0) {
      bits.push(`${summary.attention} need${summary.attention === 1 ? "s" : ""} attention`);
    }
    if (summary.failed > 0) {
      bits.push(`${summary.failed} failed`);
    }
    const needsUser = summary.attention + summary.failed > 0;
    const message = `Agent Conductor: ${bits.join(", ")} while you were away.`;
    const choice = needsUser
      ? await vscode.window.showWarningMessage(message, "Show me")
      : await vscode.window.showInformationMessage(message, "Show me");
    if (choice === "Show me") {
      await vscode.commands.executeCommand("agent-conductor.open");
    }
  }
}

export const __test = { finishedAt, LAST_SEEN_KEY, NOTIFIED_KEY };
