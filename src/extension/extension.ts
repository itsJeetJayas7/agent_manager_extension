// Agent Conductor extension entry: sidebar webview, batch planner/detail
// panels, daemon lifecycle, commands. The extension NEVER owns agent
// processes; the persistent daemon does.
import * as vscode from "vscode";
import { DaemonClient, ProtocolMismatchError } from "./daemonClient";
import { Snapshot } from "./protocol";
import {
  resolveDaemonBinary,
  resolveDaemonBinaryDetailed,
  DaemonBinaryResolution,
  mockProviders,
  hostIndicator,
  verificationCommands,
} from "./configuration";
import { openClaudeSession, openCodexThread, openWorktreeWindow } from "./sessionOpeners";
import { computeSetupStatus } from "./setup";
import { Notifier } from "./notifications";
import { WEBVIEW_DIR, WEBVIEW_BUNDLE } from "../../shared/artifacts.cjs";
import { webviewHtml } from "./webviewHtml";
import { spawn, ChildProcess } from "child_process";
import * as fs from "fs";
import * as path from "path";

export function activate(context: vscode.ExtensionContext) {
  const conductor = new ConductorExtension(context);
  context.subscriptions.push(conductor);
  conductor.start();
}

export function deactivate() {}

/** Reconnect backoff: 1s doubling to a 30s ceiling. */
const RECONNECT_MIN_MS = 1000;
const RECONNECT_MAX_MS = 30_000;

/**
 * A webview plus its readiness state. Messages posted before the bundle has
 * booted are dropped by VS Code, which is how `detailBatchId` went missing;
 * we queue until the webview announces itself with `{type:"ready"}`.
 */
class WebviewTarget {
  ready = false;
  private queue: unknown[] = [];
  constructor(readonly webview: vscode.Webview) {}

  post(msg: unknown): void {
    if (!this.ready) {
      this.queue.push(msg);
      return;
    }
    void this.webview.postMessage(msg);
  }

  markReady(): void {
    this.ready = true;
    const pending = this.queue;
    this.queue = [];
    for (const m of pending) {
      void this.webview.postMessage(m);
    }
  }
}

type StatusKind = "offline" | "connecting" | "idle" | "running" | "paused" | "error";

class ConductorExtension implements vscode.Disposable {
  private client: DaemonClient | null = null;
  private reconnectTimer: NodeJS.Timeout | null = null;
  private reconnectDelay = RECONNECT_MIN_MS;
  private disposed = false;
  private devDaemon: ChildProcess | null = null;
  private statusItem: vscode.StatusBarItem;
  private panels = new Map<vscode.WebviewPanel, WebviewTarget>();
  private sidebar?: WebviewTarget;
  private lastSnapshot: Snapshot | null = null;
  private binaryProblemShown = false;
  private notifier: Notifier;
  private reportedAway = false;

  constructor(private context: vscode.ExtensionContext) {
    this.statusItem = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 50);
    this.statusItem.command = "agent-conductor.open";
    context.subscriptions.push(this.statusItem);
    this.notifier = new Notifier(context.globalState);
  }

  dispose() {
    this.disposed = true;
    this.statusItem.dispose();
    this.devDaemon?.kill();
    this.client?.close();
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  start() {
    this.registerCommands();
    this.registerWebviewView();
    this.registerWatcher();
    void this.ensureDaemonAndConnect();
    this.statusItem.show();
  }

  // ---- daemon lifecycle ----

  private async ensureDaemonAndConnect(): Promise<void> {
    if (this.disposed) {
      return;
    }
    try {
      const client = new DaemonClient();
      await client.connect();
      this.client = client;
      this.onConnected();
    } catch (e) {
      if (e instanceof ProtocolMismatchError) {
        this.setStatus("error", "Agent Conductor: version mismatch");
        this.pushToWebviews({ type: "daemonOffline", reason: "protocol", detail: e.message });
        void vscode.window.showErrorMessage(e.message);
        return; // retrying cannot help until one side is updated
      }
      this.setStatus("offline", "Agent Conductor: daemon offline");
      this.tryStartDaemon();
    }
  }

  private tryStartDaemon(): void {
    const res = resolveDaemonBinaryDetailed(this.context);
    if (!res.path) {
      this.reportBinaryProblem(res);
      this.pushToWebviews({
        type: "daemonOffline",
        reason: res.problem?.code ?? "no-binary",
        detail: res.problem?.message,
        remedy: res.problem?.remedy,
      });
      // Still retry slowly: the user may install the service meanwhile.
      this.scheduleReconnect();
      return;
    }
    try {
      const env = { ...process.env };
      if (mockProviders()) {
        env.AC_MOCK = "1";
      }
      const child = spawn(res.path, ["serve"], { env, detached: true, stdio: "ignore" });
      child.unref();
      this.scheduleReconnect(1500);
    } catch {
      this.scheduleReconnect(10000);
    }
  }

  /** Tell the user exactly what is wrong and exactly what to run about it. */
  private reportBinaryProblem(res: DaemonBinaryResolution): void {
    if (this.binaryProblemShown || !res.problem) {
      return;
    }
    this.binaryProblemShown = true;
    const { message, remedy } = res.problem;
    if (!remedy) {
      void vscode.window.showErrorMessage(message);
      return;
    }
    void vscode.window
      .showErrorMessage(message, "Copy fix command", "Show details")
      .then((choice) => {
        if (choice === "Copy fix command") {
          void vscode.env.clipboard.writeText(remedy);
          void vscode.window.showInformationMessage(`Copied: ${remedy}`);
        } else if (choice === "Show details") {
          void vscode.commands.executeCommand("agent-conductor.diagnostics");
        }
      });
  }

  /** Exponential backoff, so a daemon crash self-heals instead of bricking the UI. */
  private scheduleReconnect(delayMs?: number): void {
    if (this.disposed) {
      return;
    }
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
    }
    const delay = delayMs ?? this.reconnectDelay;
    if (delayMs === undefined) {
      this.reconnectDelay = Math.min(this.reconnectDelay * 2, RECONNECT_MAX_MS);
    }
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      void this.ensureDaemonAndConnect();
    }, delay);
    this.reconnectTimer.unref?.();
  }

  private onConnected(): void {
    const client = this.client;
    if (!client) {
      return;
    }
    this.reconnectDelay = RECONNECT_MIN_MS;
    this.binaryProblemShown = false;
    this.setStatus("idle", "Agent Conductor");

    client.onClose(() => {
      if (this.disposed || this.client !== client) {
        return;
      }
      this.client = null;
      this.reportedAway = false;
      this.setStatus("offline", "Agent Conductor: daemon offline");
      this.pushToWebviews({ type: "daemonOffline", reason: "disconnected" });
      this.scheduleReconnect();
    });

    // push verification commands setting to daemon (patch, never read-modify-write)
    const cmds = verificationCommands();
    if (cmds.length > 0) {
      void client.request("patchSettings", { verificationCommands: cmds }).catch(() => undefined);
    }

    client.subscribe((ev) => {
      if (ev.event === "snapshot") {
        this.onSnapshot(ev.payload as Snapshot);
      } else {
        this.pushToWebviews({ type: "daemonEvent", event: ev.event, payload: ev.payload });
        if (ev.event === "runUpdated" || ev.event === "pauseUpdated") {
          void this.refreshStatus();
        }
      }
    });
    void this.sendSnapshot();
    this.pushToWebviews({ type: "daemonOnline" });
  }

  // ---- status bar ----

  private setStatus(kind: StatusKind, text: string, tooltip?: string): void {
    const icon: Record<StatusKind, string> = {
      offline: "$(debug-disconnect)",
      connecting: "$(sync~spin)",
      idle: "$(circle-outline)",
      running: "$(pulse)",
      paused: "$(debug-pause)",
      error: "$(warning)",
    };
    this.statusItem.text = `${icon[kind]} ${text}`;
    this.statusItem.tooltip = tooltip ?? text;
    this.statusItem.backgroundColor =
      kind === "error" ? new vscode.ThemeColor("statusBarItem.warningBackground") : undefined;
    const healthy = kind !== "offline" && kind !== "error";
    this.pushToWebviews({ type: "daemonHealth", healthy, text });
  }

  /** Derive the status-bar presentation from the latest snapshot. */
  private async refreshStatus(): Promise<void> {
    if (!this.client) {
      return;
    }
    const snap = this.lastSnapshot;
    if (!snap) {
      return;
    }
    const pause = snap.pause;
    if (pause?.paused) {
      const until = pause.resumeAt ? new Date(pause.resumeAt).getTime() : 0;
      const remainMs = until ? Math.max(0, until - Date.now()) : 0;
      const short = until ? ` · ${formatShort(remainMs)}` : "";
      const why =
        pause.source && pause.source !== "user" ? ` (${pause.source})` : "";
      this.setStatus(
        "paused",
        `Paused${short}`,
        [
          `Agent Conductor queue paused${why}`,
          pause.reason ? `Reason: ${pause.reason}` : "",
          until ? `Resumes at ${new Date(until).toLocaleTimeString()}` : "Paused until you resume",
        ]
          .filter(Boolean)
          .join("\n")
      );
      return;
    }
    const active = (snap.runs ?? []).filter((r) =>
      ["PREPARING", "STARTING", "RUNNING", "WAITING_FOR_PROVIDER", "VERIFYING", "INTEGRATING"].includes(
        r.state
      )
    ).length;
    const waiting = (snap.runs ?? []).filter((r) =>
      ["QUEUED", "WAITING_FOR_SLOT", "WAITING_FOR_EXTERNAL_AGENT", "WAITING_FOR_QUOTA"].includes(r.state)
    ).length;
    if (active > 0) {
      this.setStatus("running", `${active} running`, `${active} running · ${waiting} waiting`);
    } else if (waiting > 0) {
      this.setStatus("idle", `${waiting} waiting`, `${waiting} waiting to start`);
    } else {
      this.setStatus("idle", "Agent Conductor", "Queue is empty");
    }
  }

  private onSnapshot(snap: Snapshot): void {
    this.lastSnapshot = snap;
    this.pushToWebviews({ type: "snapshot", snapshot: snap });
    void this.refreshStatus();
    this.pushSetupStatus();
    // Tell the user once per connection what finished while they were away.
    if (!this.reportedAway) {
      this.reportedAway = true;
      void this.notifier.reportAway(snap);
    }
  }

  private pushSetupStatus(): void {
    const status = computeSetupStatus(this.context, {
      daemonConnected: !!this.client,
      workspace: vscode.workspace.workspaceFolders?.[0]?.uri.fsPath ?? "",
    });
    this.pushToWebviews({ type: "setupStatus", status });
  }

  private async sendSnapshot(): Promise<void> {
    if (!this.client) {
      return;
    }
    try {
      const snap = await this.client.request<Snapshot>("getSnapshot", {});
      this.onSnapshot(snap);
    } catch {
      this.setStatus("error", "Agent Conductor: daemon error");
    }
  }

  // ---- webview surfaces ----

  private registerWebviewView(): void {
    this.context.subscriptions.push(
      vscode.window.registerWebviewViewProvider(
        "agent-conductor.sidebar",
        {
          resolveWebviewView: (view) => {
            const target = new WebviewTarget(view.webview);
            this.sidebar = target;
            view.webview.options = {
              enableScripts: true,
              localResourceRoots: [
                vscode.Uri.joinPath(this.context.extensionUri, ...WEBVIEW_DIR.split("/")),
              ],
            };
            view.webview.html = this.webviewHtml(view.webview, "sidebar");
            view.webview.onDidReceiveMessage((m) => void this.onWebviewMessage(m, target));
            view.onDidDispose(() => {
              if (this.sidebar === target) {
                this.sidebar = undefined;
              }
            });
          },
        },
        { webviewOptions: { retainContextWhenHidden: true } }
      )
    );
  }

  private openPanel(mode: "planner" | "detail", title: string, state?: unknown): void {
    for (const [panel, target] of this.panels) {
      const p = panel as any;
      if (p._acMode !== mode) {
        continue;
      }
      if (mode === "planner" || p._acBatchId === state) {
        panel.reveal();
        if (mode === "detail" && state) {
          p._acBatchId = state;
          target.post({ type: "detailBatchId", batchId: state });
        }
        return;
      }
    }
    const panel = vscode.window.createWebviewPanel(`agent-conductor.${mode}`, title, vscode.ViewColumn.One, {
      enableScripts: true,
      retainContextWhenHidden: true,
      localResourceRoots: [vscode.Uri.joinPath(this.context.extensionUri, ...WEBVIEW_DIR.split("/"))],
    });
    (panel as any)._acMode = mode;
    if (mode === "detail") {
      (panel as any)._acBatchId = state;
    }
    const target = new WebviewTarget(panel.webview);
    this.panels.set(panel, target);
    panel.webview.html = this.webviewHtml(panel.webview, mode);
    panel.webview.onDidReceiveMessage((m) => void this.onWebviewMessage(m, target));
    panel.onDidDispose(() => this.panels.delete(panel));
    if (mode === "detail" && state) {
      target.post({ type: "detailBatchId", batchId: state });
    }
  }

  private webviewHtml(webview: vscode.Webview, mode: string): string {
    const js = webview.asWebviewUri(
      vscode.Uri.joinPath(this.context.extensionUri, ...WEBVIEW_DIR.split("/"), WEBVIEW_BUNDLE)
    );
    return webviewHtml({ scriptUri: js.toString(), cspSource: webview.cspSource, mode });
  }

  private targets(): WebviewTarget[] {
    const out: WebviewTarget[] = [];
    if (this.sidebar) {
      out.push(this.sidebar);
    }
    out.push(...this.panels.values());
    return out;
  }

  private pushToWebviews(msg: unknown): void {
    for (const t of this.targets()) {
      t.post(msg);
    }
  }

  // ---- message routing (webview -> extension -> daemon) ----

  private async onWebviewMessage(m: any, source: WebviewTarget): Promise<void> {
    if (!m || typeof m.type !== "string") {
      return;
    }
    try {
      switch (m.type) {
        case "ready": {
          source.markReady();
          source.post({
            type: "hostInfo",
            host: hostIndicator(),
            workspace: vscode.workspace.workspaceFolders?.[0]?.uri.fsPath ?? "",
          });
          if (this.lastSnapshot) {
            source.post({ type: "snapshot", snapshot: this.lastSnapshot });
          }
          this.pushSetupStatus();
          await this.sendSnapshot();
          return;
        }
        case "requestSnapshot":
          await this.sendSnapshot();
          return;
        case "requestSetupStatus":
          this.pushSetupStatus();
          return;
        case "hostInfo": {
          source.post({
            type: "hostInfo",
            host: hostIndicator(),
            workspace: vscode.workspace.workspaceFolders?.[0]?.uri.fsPath ?? "",
          });
          return;
        }
        case "rpc": {
          if (!this.client) {
            source.post({ type: "rpcError", requestId: m.requestId, error: "daemon offline" });
            return;
          }
          const result = await this.client.request(m.method, m.params ?? {});
          source.post({ type: "rpcResult", requestId: m.requestId, result });
          return;
        }
        case "confirm": {
          // VS Code webviews are sandboxed: window.confirm() does nothing there.
          const choice = await vscode.window.showWarningMessage(
            String(m.message ?? "Are you sure?"),
            { modal: true, detail: m.detail ? String(m.detail) : undefined },
            String(m.confirmLabel ?? "Confirm")
          );
          source.post({
            type: "confirmResult",
            requestId: m.requestId,
            confirmed: choice === String(m.confirmLabel ?? "Confirm"),
          });
          return;
        }
        case "notify": {
          const text = String(m.message ?? "");
          if (m.level === "error") {
            void vscode.window.showErrorMessage(text);
          } else if (m.level === "warning") {
            void vscode.window.showWarningMessage(text);
          } else {
            void vscode.window.showInformationMessage(text);
          }
          return;
        }
        case "openClaude": {
          const r = await openClaudeSession(m.sessionId);
          void vscode.window.showInformationMessage(r.how);
          return;
        }
        case "openCodex": {
          const r = await openCodexThread(m.threadId, m.title);
          void vscode.window.showInformationMessage(r.how);
          return;
        }
        case "openWorktree": {
          await openWorktreeWindow(m.path);
          return;
        }
        case "openDetail": {
          this.openPanel("detail", "Batch Detail", m.batchId);
          return;
        }
        case "openPlanner": {
          this.openPanel("planner", "Batch Planner");
          return;
        }
        case "cmd": {
          await vscode.commands.executeCommand(m.command);
          return;
        }
        case "copy": {
          await vscode.env.clipboard.writeText(String(m.text ?? ""));
          if (m.warnSecrets) {
            void vscode.window.showWarningMessage(
              "Copied text includes Shared Testing & Sudo content. Handle carefully."
            );
          }
          return;
        }
        case "revealPrompt": {
          if (!this.client) {
            return;
          }
          const r = await this.client.request<{ text: string; containsSecrets: boolean }>(
            "revealEffectivePrompt",
            { runId: m.runId }
          );
          if (r.containsSecrets) {
            const go = await vscode.window.showWarningMessage(
              "This prompt contains your Shared Testing & Sudo block in plain text.",
              { modal: true },
              "Show anyway"
            );
            if (go !== "Show anyway") {
              return;
            }
          }
          const doc = await vscode.workspace.openTextDocument({
            content: r.text,
            language: "markdown",
          });
          await vscode.window.showTextDocument(doc, { preview: true });
          return;
        }
        default:
          return;
      }
    } catch (e: any) {
      source.post({ type: "rpcError", requestId: m.requestId, error: String(e?.message ?? e) });
    }
  }

  // ---- commands ----

  private registerCommands(): void {
    const reg = (id: string, fn: (...a: any[]) => any) =>
      this.context.subscriptions.push(vscode.commands.registerCommand(id, fn));

    reg("agent-conductor.open", () => {
      void vscode.commands.executeCommand("agent-conductor.sidebar.focus");
    });
    reg("agent-conductor.buildBatch", () => this.openPanel("planner", "Batch Planner"));
    reg("agent-conductor.diagnostics", () => void this.runDiagnostics());
    reg("agent-conductor.startDevDaemon", () => this.startDevDaemon());
    reg("agent-conductor.repairRuntime", () => void this.repairRuntime());
    reg("agent-conductor.openLogs", () => void this.openLogs());
    reg("agent-conductor.installService", () => void this.installService());
    reg("agent-conductor.uninstallService", () => void this.uninstallService());
    reg("agent-conductor.pauseQueue", () => void this.pauseQueueInteractive());
    reg("agent-conductor.resumeQueue", () => void this.resumeQueue());
    reg("agent-conductor.extendPause", () => void this.extendPause(15 * 60_000));
    reg("agent-conductor.trustWorkspace", () => void this.trustWorkspace());
  }

  /** Trust the open folder for Claude Code — explicit user action only. */
  private async trustWorkspace(): Promise<void> {
    const folder = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
    if (!folder) {
      void vscode.window.showWarningMessage("Open the folder you want agents to work in first.");
      return;
    }
    if (!this.client) {
      void vscode.window.showWarningMessage("Agent Conductor daemon is not connected.");
      return;
    }
    const ok = await vscode.window.showWarningMessage(
      `Let Claude Code work in ${folder}?`,
      { modal: true, detail: "This records the folder as trusted in ~/.claude.json, exactly as answering Claude Code's own prompt would. A backup is taken first." },
      "Trust folder"
    );
    if (ok !== "Trust folder") {
      return;
    }
    try {
      await this.client.request("trustWorkspace", { path: folder });
      void vscode.window.showInformationMessage("Folder trusted.");
      this.pushSetupStatus();
    } catch (e: any) {
      void vscode.window.showErrorMessage(`Could not trust the folder: ${e?.message ?? e}`);
    }
  }

  // ---- pause commands (§5.6) ----

  private async pauseQueueInteractive(): Promise<void> {
    if (!this.client) {
      void vscode.window.showWarningMessage("Agent Conductor daemon is not connected.");
      return;
    }
    const choices: { label: string; description?: string; durationMs?: number }[] = [
      { label: "Until I resume", description: "No timer" },
      { label: "15 minutes", durationMs: 15 * 60_000 },
      { label: "30 minutes", durationMs: 30 * 60_000 },
      { label: "1 hour", durationMs: 60 * 60_000 },
      { label: "2 hours", durationMs: 2 * 60 * 60_000 },
      { label: "4 hours", durationMs: 4 * 60 * 60_000 },
      { label: "8 hours", durationMs: 8 * 60 * 60_000 },
    ];
    const pick = await vscode.window.showQuickPick(choices, {
      title: "Pause the Agent Conductor queue",
      placeHolder: "Resume automatically after…",
    });
    if (!pick) {
      return;
    }
    try {
      await this.client.request("pauseQueue", {
        durationMs: pick.durationMs,
        scope: "QUEUE_ONLY",
        reason: "",
      });
      await this.sendSnapshot();
      void vscode.window.showInformationMessage(
        pick.durationMs ? `Queue paused — resumes in ${pick.label}.` : "Queue paused."
      );
    } catch (e: any) {
      void vscode.window.showErrorMessage(`Could not pause: ${e?.message ?? e}`);
    }
  }

  private async resumeQueue(): Promise<void> {
    if (!this.client) {
      return;
    }
    try {
      await this.client.request("resumeQueue", {});
      await this.sendSnapshot();
      void vscode.window.showInformationMessage("Queue resumed.");
    } catch (e: any) {
      void vscode.window.showErrorMessage(`Could not resume: ${e?.message ?? e}`);
    }
  }

  private async extendPause(deltaMs: number): Promise<void> {
    if (!this.client) {
      return;
    }
    try {
      await this.client.request("extendPause", { deltaMs });
      await this.sendSnapshot();
    } catch (e: any) {
      void vscode.window.showErrorMessage(`Could not extend the pause: ${e?.message ?? e}`);
    }
  }

  private async runDiagnostics(): Promise<void> {
    const lines = ["# Agent Conductor Diagnostics", ""];
    const res = resolveDaemonBinaryDetailed(this.context);
    lines.push("## Daemon binary", "");
    lines.push(`- Resolved: ${res.path || "(none found)"}`);
    lines.push(`- Source: ${res.source}`);
    if (res.version) {
      lines.push(`- Reports: ${res.version}`);
    }
    if (res.problem) {
      lines.push(`- Problem: ${res.problem.message}`);
      if (res.problem.remedy) {
        lines.push(`- Fix: \`${res.problem.remedy}\``);
      }
    }
    lines.push("");
    if (!this.client) {
      lines.push("Daemon is not connected.");
    } else {
      const d = await this.client.request<any>("diagnostics", {});
      lines.push(`Daemon: v${d.daemon.version} (pid ${d.daemon.pid})`);
      lines.push("");
      lines.push("## Providers", "");
      for (const [p, caps] of Object.entries<any>(d.providers ?? {})) {
        lines.push(
          `- **${p}**: installed=${caps.installed} version=${caps.version} auth=${caps.authState}` +
            (caps.notes?.length ? ` — ${caps.notes.join("; ")}` : "")
        );
      }
    }
    const doc = await vscode.workspace.openTextDocument({
      content: lines.join("\n"),
      language: "markdown",
    });
    await vscode.window.showTextDocument(doc);
  }

  private startDevDaemon(): void {
    const res = resolveDaemonBinaryDetailed(this.context);
    if (!res.path) {
      this.reportBinaryProblem(res);
      return;
    }
    const term = vscode.window.createTerminal({ name: "Agent Conductor daemon" });
    this.devDaemon = null;
    term.sendText(`"${res.path}" serve`);
    term.show();
    this.scheduleReconnect(2500);
  }

  private async repairRuntime(): Promise<void> {
    const bin = resolveDaemonBinary(this.context);
    if (!bin) {
      void vscode.window.showErrorMessage("Daemon binary missing; cannot repair hooks.");
      return;
    }
    const { spawnSync } = require("child_process") as typeof import("child_process");
    const r = spawnSync(bin, ["repair-hooks"], { encoding: "utf8" });
    void vscode.window.showInformationMessage(`Hook repair:\n${r.stdout || r.stderr}`);
  }

  private async openLogs(): Promise<void> {
    const logDir = path.join(require("os").homedir(), ".agent-conductor", "logs");
    const logFile = path.join(logDir, "daemon.jsonl");
    if (!fs.existsSync(logFile)) {
      void vscode.window.showWarningMessage("No daemon log yet.");
      return;
    }
    const doc = await vscode.workspace.openTextDocument(vscode.Uri.file(logFile));
    await vscode.window.showTextDocument(doc, { preview: false });
  }

  private async installService(): Promise<void> {
    const res = resolveDaemonBinaryDetailed(this.context);
    if (!res.path) {
      this.reportBinaryProblem(res);
      return;
    }
    const choice = await vscode.window.showInformationMessage(
      "Install Agent Conductor as a persistent background service? It will keep runs alive after VS Code closes.",
      "Install"
    );
    if (choice !== "Install") {
      return;
    }
    const { spawnSync } = require("child_process") as typeof import("child_process");
    const r = spawnSync(res.path, ["install-service"], { encoding: "utf8" });
    const out = (r.stdout || "") + (r.stderr || "");
    void vscode.window.showInformationMessage(out.slice(0, 800));
    this.scheduleReconnect(3000);
  }

  private async uninstallService(): Promise<void> {
    const bin = resolveDaemonBinary(this.context);
    if (!bin) {
      return;
    }
    const confirm = await vscode.window.showWarningMessage(
      "Uninstall the background service and remove Agent Conductor provider hooks? Runs and secrets stay on disk.",
      { modal: true },
      "Uninstall"
    );
    if (confirm !== "Uninstall") {
      return;
    }
    const { spawnSync } = require("child_process") as typeof import("child_process");
    const r = spawnSync(bin, ["uninstall-service"], { encoding: "utf8" });
    void vscode.window.showInformationMessage(((r.stdout || "") + (r.stderr || "")).slice(0, 800));
  }

  private registerWatcher(): void {
    // refresh snapshot when the window gains focus (cheap, event-driven)
    vscode.window.onDidChangeWindowState(
      (s) => {
        if (s.focused) {
          void this.sendSnapshot();
        }
      },
      undefined,
      this.context.subscriptions
    );
  }
}

/** `23m` / `1h 05m`, for the status bar. */
function formatShort(ms: number): string {
  const totalMin = Math.max(0, Math.round(ms / 60_000));
  if (totalMin < 60) {
    return `${totalMin}m`;
  }
  const h = Math.floor(totalMin / 60);
  const m = totalMin % 60;
  return `${h}h ${String(m).padStart(2, "0")}m`;
}

export const __test = { formatShort, WebviewTarget };
