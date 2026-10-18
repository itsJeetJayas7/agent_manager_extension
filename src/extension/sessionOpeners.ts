// sessionOpeners routes users back to native provider conversations.
//
// Claude Code's VS Code extension never attaches to a process it did not
// start: opening a session reads its transcript once, then spawns its own
// `claude --resume`. A managed run therefore only appears live when the
// extension's process launch goes through Agent Conductor's wrapper
// (claudeCode.claudeProcessWrapper), which attaches the tab to the process the
// daemon is running. Success is reported only after the daemon confirms that a
// tab is attached; a tab appearing is not evidence of a live connection.
import * as vscode from "vscode";

import * as path from "node:path";
import { access, realpath } from "node:fs/promises";
import { constants as fsConstants } from "node:fs";

export interface ClaudeSessionTarget {
  sessionId: string;
  cwd: string;
}

export interface OpenResult { opened: boolean; live?: boolean; how: string }
interface Handoff extends ClaudeSessionTarget { expiresAt: number }
export type DaemonRpc = <T>(method: string, params: Record<string, unknown>) => Promise<T>;
interface SessionLive { live: boolean; runId?: string; status?: { pid: number; viewers: number; busy: string } }

const HANDOFF_KEY = "claudeSessionHandoff:";
const LIVE_DECLINED_KEY = "claudeLiveViewDeclined";
export const WRAPPER_SETTING = "claudeProcessWrapper";
export const WRAPPER_NAME = "agent-conductor-claude-wrapper";
const ATTACH_WAIT_MS = 15_000;
const ATTACH_POLL_MS = 500;

async function canonical(folder: string): Promise<string> {
  return realpath(folder).catch(() => path.resolve(folder));
}

function resumeCommand(target: ClaudeSessionTarget): string {
  const quote = (s: string) => "'" + s.replace(/'/g, "'\"'\"'") + "'";
  return `cd -- ${quote(target.cwd)} && claude --resume ${quote(target.sessionId)}`;
}

/** Which claudeProcessWrapper is configured. The setting is machine-scoped,
 * so it applies to every Claude Code conversation on this machine. */
export function wrapperSetting(): { state: "ours" | "none" | "other"; value?: string } {
  const value = vscode.workspace.getConfiguration("claudeCode").inspect<string>(WRAPPER_SETTING)?.globalValue;
  if (!value) return { state: "none" };
  return { state: path.basename(value) === WRAPPER_NAME ? "ours" : "other", value };
}

export class ClaudeSessionOpener {
  constructor(
    private readonly state: vscode.Memento,
    private readonly rpc?: DaemonRpc,
    private readonly sleep: (ms: number) => Promise<void> = ms => new Promise(r => setTimeout(r, ms)),
  ) {}

  /** Install the wrapper and point Claude Code at it. */
  async enableLiveView(): Promise<{ ok: boolean; how: string }> {
    const current = wrapperSetting();
    if (current.state === "other") {
      return { ok: false, how: `Claude Code already uses another process wrapper (${current.value}). Agent Conductor will not replace it, so managed runs open as saved conversations only.` };
    }
    if (!this.rpc) return { ok: false, how: "Agent Conductor's daemon is not connected." };
    try {
      const { path: wrapper } = await this.rpc<{ path: string }>("installClaudeWrapper", {});
      await vscode.workspace.getConfiguration("claudeCode").update(WRAPPER_SETTING, wrapper, vscode.ConfigurationTarget.Global);
      await this.state.update(LIVE_DECLINED_KEY, undefined);
      return { ok: true, how: "Live view in Claude Code is on. Claude Code now starts conversations through Agent Conductor's wrapper; conversations it does not manage start exactly as before." };
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      if (/UNKNOWN_METHOD|unknown method/i.test(message)) {
        return { ok: false, how: "The running Agent Conductor daemon is older than this extension and cannot attach Claude Code to runs. Run \"Agent Conductor: Install Background Service\" to update and restart it, then try again." };
      }
      return { ok: false, how: `Could not enable live view: ${message}` };
    }
  }

  /** Remove the wrapper setting if it is ours. */
  async disableLiveView(): Promise<boolean> {
    if (wrapperSetting().state !== "ours") return false;
    await vscode.workspace.getConfiguration("claudeCode").update(WRAPPER_SETTING, undefined, vscode.ConfigurationTarget.Global);
    return true;
  }

  /** A wrapper setting pointing at a deleted script would stop Claude Code
   * from starting any conversation. Remove it rather than leave it broken. */
  async repairLiveView(): Promise<void> {
    const current = wrapperSetting();
    if (current.state !== "ours" || !current.value) return;
    const usable = await access(current.value, fsConstants.X_OK).then(() => true, () => false);
    if (!usable) await this.disableLiveView();
  }

  private async ensureLiveView(): Promise<boolean> {
    const current = wrapperSetting();
    if (current.state === "ours") return true;
    if (current.state === "other" || this.state.get(LIVE_DECLINED_KEY) || !this.rpc) return false;
    const enable = "Enable live view";
    const choice = await vscode.window.showInformationMessage(
      "Show Agent Conductor runs live in Claude Code?",
      {
        modal: true,
        detail: "Claude Code can only follow a run that it is attached to. Agent Conductor will set Claude Code's \"Claude Process Wrapper\" setting so that opening a managed run attaches to the process Agent Conductor is already running, instead of starting a second copy of it. All other conversations start exactly as before. You can turn this off with \"Agent Conductor: Disable Live View in Claude Code\".",
      },
      enable, "Open without live view",
    );
    if (choice !== enable) {
      if (choice) await this.state.update(LIVE_DECLINED_KEY, true);
      return false;
    }
    const result = await this.enableLiveView();
    if (!result.ok) void vscode.window.showWarningMessage(result.how);
    return result.ok;
  }

  private async liveState(sessionId: string): Promise<SessionLive | undefined> {
    return this.rpc?.<SessionLive>("claudeSessionLive", { sessionId }).catch(() => undefined);
  }

  private async release(sessionId: string): Promise<void> {
    await this.rpc?.("releaseClaudeAttach", { sessionId }).catch(() => undefined);
  }

  /** Ask the daemon whether a Claude Code tab is attached to the session's
   * process. Opening a tab proves nothing by itself. */
  private async waitForAttachment(sessionId: string): Promise<SessionLive | undefined> {
    if (!this.rpc) return undefined;
    let last: SessionLive | undefined;
    for (let waited = 0; ; waited += ATTACH_POLL_MS) {
      try {
        last = await this.rpc<SessionLive>("claudeSessionLive", { sessionId });
        if (last?.live && (last.status?.viewers ?? 0) > 0) return last;
      } catch {
        // daemon briefly unavailable; keep waiting until the deadline
      }
      if (waited >= ATTACH_WAIT_MS) return last;
      await this.sleep(ATTACH_POLL_MS);
    }
  }

  /** Use the window's extension host, never an OS URL routed to a different
   * IDE or SSH window. claude-vscode.primaryEditor.open(sessionId) is the
   * command the extension's documented /open?session= URI uses; it focuses an
   * existing tab for the session instead of opening another. Verified with
   * Claude Code 2.1.288 and probed at runtime for other versions. */
  private async openInCurrentHost(target: ClaudeSessionTarget): Promise<OpenResult> {
    const fallback = async (reason: string): Promise<OpenResult> => {
      await vscode.env.clipboard.writeText(resumeCommand(target));
      return { opened: false, how: `${reason} Resume command copied for the run's folder. Pause an active run before resuming it manually. Live activity remains available in Agent Conductor.` };
    };
    const ext = vscode.extensions.getExtension("anthropic.claude-code");
    if (!ext) return fallback("Install or enable Claude Code in this VS Code window (on the workspace host for Remote SSH).");
    let live: boolean;
    let paused = false;
    try {
      await ext.activate();
      const commands = await vscode.commands.getCommands(true);
      const command = ["claude-vscode.primaryEditor.open", "claude-vscode.editor.open"].find(c => commands.includes(c));
      if (!command) return fallback("This Claude Code version does not expose a session-opening command. Update Claude Code.");
      live = await this.ensureLiveView();
      const before = live ? await this.liveState(target.sessionId) : undefined;
      if (before?.live && (before.status?.viewers ?? 0) > 0) {
        // Already attached: the command only focuses the existing tab.
        await vscode.commands.executeCommand(command, target.sessionId);
        return { opened: true, live: true, how: `Focused the Claude Code tab already attached to this run (process ${before.status?.pid}).` };
      }
      if (before?.live && this.rpc) {
        // The tab reads the transcript, then attaches. Pausing the run's
        // process in between keeps the two exactly continuous.
        paused = (await this.rpc<{ paused: boolean }>("prepareClaudeAttach", { sessionId: target.sessionId }).catch(() => ({ paused: false }))).paused;
      }
      await vscode.commands.executeCommand(command, target.sessionId);
    } catch (error) {
      if (paused) await this.release(target.sessionId);
      return fallback(`Claude Code could not open the session: ${error instanceof Error ? error.message : String(error)}.`);
    }
    if (!live) {
      return { opened: true, live: false, how: "Opened the saved conversation in Claude Code. It is not attached to the run's process, so it will not update while the run continues; Live activity in Agent Conductor shows the running output. Enable live view with \"Agent Conductor: Enable Live View in Claude Code\"." };
    }
    const attached = await this.waitForAttachment(target.sessionId);
    if (paused) await this.release(target.sessionId); // normally already resumed by the attach
    if (attached?.live && (attached.status?.viewers ?? 0) > 0) {
      const busy = attached.status?.busy ? " The run is working; new messages appear as they happen." : "";
      return { opened: true, live: true, how: `Claude Code is attached to the running session (process ${attached.status?.pid}).${busy}` };
    }
    return { opened: true, live: false, how: "Claude Code opened the conversation but did not attach to Agent Conductor's process, so it may not update. Make sure this window's Claude Code uses the Claude Process Wrapper setting, then open the run again. See Agent Conductor's logs (claude-wrapper.log) for the reason." };
  }

  /** Session history is scoped to the run's cwd. A worktree must be opened in
   * its own window before asking Claude to load the session. An expiring
   * handoff lets our extension in that window complete the action after
   * activation. */
  async open(target: ClaudeSessionTarget): Promise<OpenResult> {
    if (!target || typeof target.sessionId !== "string" || !/^[a-zA-Z0-9_-]+$/.test(target.sessionId) ||
        typeof target.cwd !== "string" || !path.isAbsolute(target.cwd)) {
      return { opened: false, how: "The run does not have a valid native session and working folder yet. Check Live activity and try again after the session starts." };
    }
    const folders = vscode.workspace.workspaceFolders ?? [];
    const cwd = await canonical(target.cwd);
    for (const folder of folders) {
      if (await canonical(folder.uri.fsPath) === cwd) return this.openInCurrentHost(target);
    }
    const base = folders[0]?.uri;
    if (vscode.env.remoteName && !base) {
      return { opened: false, how: "Open the run's folder in this remote VS Code connection, then try again." };
    }
    // Preserve vscode-remote authority: Uri.file would open a remote path on
    // the local machine. No remote hostnames or OS-specific URI schemes guessed.
    const uri = base ? base.with({ path: vscode.Uri.file(target.cwd).path, query: "", fragment: "" }) : vscode.Uri.file(target.cwd);
    const key = HANDOFF_KEY + cwd;
    await this.state.update(key, { ...target, expiresAt: Date.now() + 120_000 } satisfies Handoff);
    try {
      await vscode.commands.executeCommand("vscode.openFolder", uri, { forceNewWindow: true });
      return { opened: true, how: "Opening the run's folder in a new window; Claude Code will open the session there." };
    } catch (error) {
      await this.state.update(key, undefined);
      return { opened: false, how: `Could not open the run's folder: ${error instanceof Error ? error.message : String(error)}` };
    }
  }

  async resumePending(): Promise<void> {
    for (const folder of vscode.workspace.workspaceFolders ?? []) {
      const key = HANDOFF_KEY + await canonical(folder.uri.fsPath);
      const pending = this.state.get<Handoff>(key);
      if (!pending) continue;
      await this.state.update(key, undefined);
      if (pending.expiresAt < Date.now()) continue;
      const result = await this.open(pending);
      if (!result.opened || result.live === false) await vscode.window.showWarningMessage(result.how);
      else await vscode.window.showInformationMessage(result.how);
    }
  }
}

/**
 * Open a managed Codex thread in the official Codex experience.
 * The App Server and the IDE share CODEX_HOME, so the thread is present in
 * native history. Exact programmatic thread focusing depends on documented
 * public commands; we probe them without reverse engineering.
 */
export async function openCodexThread(threadId: string, threadTitle: string): Promise<{ opened: boolean; how: string }> {
  const ext = vscode.extensions.getExtension("openai.chatgpt") ?? vscode.extensions.getExtension("openai.codex");
  let commands: string[] = [];
  try {
    commands = await vscode.commands.getCommands(true);
  } catch {
    commands = [];
  }
  // Documented-style focus commands only; never private DOM tricks.
  const candidates = [
    "codex.extension.openSidebar",
    "openai.codex.openChat",
    "codex.openChat",
    "chatgpt.openChat",
  ];
  const available = candidates.find((c) => commands.includes(c));
  if (available) {
    try {
      await vscode.commands.executeCommand(available);
      await vscode.env.clipboard.writeText(threadTitle);
      return {
        opened: true,
        how: `Codex opened. Thread is in native history under its deterministic title (copied): ${threadTitle}`,
      };
    } catch {
      // fall through
    }
  }
  if (!ext) {
    await vscode.env.clipboard.writeText(threadTitle);
    return {
      opened: false,
      how: "Codex extension not detected. Thread title copied — find it in Codex history after installing the extension.",
    };
  }
  await vscode.env.clipboard.writeText(threadTitle);
  return {
    opened: false,
    how: "No documented thread-focus command in this Codex version. Thread title copied; select it in Codex history.",
  };
}

/** Open a run worktree in a new VS Code window. */
export async function openWorktreeWindow(worktreePath: string): Promise<void> {
  const uri = vscode.Uri.file(worktreePath);
  await vscode.commands.executeCommand("vscode.openFolder", uri, true);
}
