// Agent Conductor extension entry: sidebar webview, batch planner/detail
// panels, daemon lifecycle, commands. The extension NEVER owns agent
// processes; the persistent daemon does.
import * as vscode from "vscode";
import { DaemonClient } from "./daemonClient";
import { Snapshot } from "./protocol";
import { resolveDaemonBinary, mockProviders, hostIndicator, verificationCommands } from "./configuration";
import { openClaudeSession, openCodexThread, openWorktreeWindow } from "./sessionOpeners";
import { spawn, ChildProcess } from "child_process";
import * as fs from "fs";
import * as path from "path";

export function activate(context: vscode.ExtensionContext) {
  const conductor = new ConductorExtension(context);
  context.subscriptions.push(conductor);
  conductor.start();
}

export function deactivate() {}

class ConductorExtension implements vscode.Disposable {
  private client: DaemonClient | null = null;
  private reconnectTimer: NodeJS.Timeout | null = null;
  private devDaemon: ChildProcess | null = null;
  private statusItem: vscode.StatusBarItem;
  private panels = new Set<vscode.WebviewPanel>();
  private sidebar?: vscode.WebviewView;

  constructor(private context: vscode.ExtensionContext) {
    this.statusItem = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 50);
    this.statusItem.command = "agent-conductor.open";
  }

  dispose() {
    this.statusItem.dispose();
    this.devDaemon?.kill();
    this.client?.close();
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
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
    try {
      this.client = new DaemonClient();
      await this.client.connect();
      this.onConnected();
    } catch {
      this.setStatus("Agent Conductor: daemon offline", true);
      this.tryStartDaemon();
    }
  }

  private tryStartDaemon(): void {
    const bin = resolveDaemonBinary(this.context);
    if (!bin) {
      this.pushToWebviews({ type: "daemonOffline", reason: "no-binary" });
      return;
    }
    try {
      const env = { ...process.env };
      if (mockProviders()) {
        env.AC_MOCK = "1";
      }
      const child = spawn(bin, ["serve"], {
        env,
        detached: true,
        stdio: "ignore",
      });
      child.unref();
      // retry connection shortly after spawn
      this.scheduleReconnect(1500);
    } catch {
      this.scheduleReconnect(10000);
    }
  }

  private scheduleReconnect(delayMs: number): void {
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
    }
    this.reconnectTimer = setTimeout(() => void this.ensureDaemonAndConnect(), delayMs);
  }

  private onConnected(): void {
    if (!this.client) {
      return;
    }
    this.setStatus("Agent Conductor", false);
    // push verification commands setting to daemon
    const cmds = verificationCommands();
    void this.client.request("getSettings", {}).then((s: any) => {
      if (cmds.length > 0 && JSON.stringify(s.verificationCommands ?? []) !== JSON.stringify(cmds)) {
        return this.client!.request("updateSettings", { ...s, verificationCommands: cmds });
      }
      return undefined;
    }).catch(() => undefined);

    this.client.subscribe((ev) => {
      if (ev.event === "snapshot") {
        this.pushToWebviews({ type: "snapshot", snapshot: ev.payload as Snapshot });
      } else {
        this.pushToWebviews({ type: "daemonEvent", event: ev.event, payload: ev.payload });
      }
    });
    void this.sendSnapshot();
    this.pushToWebviews({ type: "daemonOnline" });
  }

  private setStatus(text: string, warn: boolean): void {
    this.statusItem.text = warn ? `$(debug-disconnect) ${text}` : `$(agent) ${text}`;
    this.statusItem.tooltip = text;
    this.pushToWebviews({ type: "daemonHealth", healthy: !warn, text });
  }

  private async sendSnapshot(): Promise<void> {
    if (!this.client) {
      return;
    }
    try {
      const snap = await this.client.request<Snapshot>("getSnapshot", {});
      this.pushToWebviews({ type: "snapshot", snapshot: snap });
    } catch (e) {
      this.setStatus("Agent Conductor: daemon error", true);
    }
  }

  // ---- webview surfaces ----

  private registerWebviewView(): void {
    vscode.window.registerWebviewViewProvider("agent-conductor.sidebar", {
      resolveWebviewView: (view) => {
        this.sidebar = view;
        view.webview.options = { enableScripts: true, localResourceRoots: [vscode.Uri.joinPath(this.context.extensionUri, "dist", "webview")] };
        view.webview.html = this.webviewHtml(view.webview, "sidebar");
        view.webview.onDidReceiveMessage((m) => void this.onWebviewMessage(m, view.webview));
        view.onDidDispose(() => { this.sidebar = undefined; });
        void this.sendSnapshot();
      },
    });
  }

  private openPanel(mode: "planner" | "detail", title: string, state?: unknown): void {
    const existing = [...this.panels].find((p) => (p as any)._acMode === mode && (mode === "planner" || (p as any)._acBatchId === state));
    if (existing) {
      existing.reveal();
      if (mode === "detail" && state) {
        (existing as any)._acBatchId = state;
        existing.webview.postMessage({ type: "detailBatchId", batchId: state });
      }
      return;
    }
    const panel = vscode.window.createWebviewPanel(
      `agent-conductor.${mode}`,
      title,
      vscode.ViewColumn.One,
      { enableScripts: true, localResourceRoots: [vscode.Uri.joinPath(this.context.extensionUri, "dist", "webview")] }
    );
    (panel as any)._acMode = mode;
    if (mode === "detail") {
      (panel as any)._acBatchId = state;
    }
    panel.webview.html = this.webviewHtml(panel.webview, mode);
    panel.webview.onDidReceiveMessage((m) => void this.onWebviewMessage(m, panel.webview));
    this.panels.add(panel);
    panel.onDidDispose(() => this.panels.delete(panel));
    if (mode === "detail" && state) {
      panel.webview.postMessage({ type: "detailBatchId", batchId: state });
    }
    void this.sendSnapshot();
  }

  private webviewHtml(webview: vscode.Webview, mode: string): string {
    const js = webview.asWebviewUri(vscode.Uri.joinPath(this.context.extensionUri, "dist", "webview", "webview.js"));
    const nonce = Array.from({ length: 32 }, () => Math.random().toString(36)[2]).join("");
    const csp = [
      "default-src 'none'",
      `script-src 'nonce-${nonce}'`,
      "style-src 'unsafe-inline'",
      "font-src 'none'",
    ].join("; ");
    return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta http-equiv="Content-Security-Policy" content="${csp}">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
</head>
<body data-mode="${mode}">
<div id="root"></div>
<script nonce="${nonce}">window.__acVscode=acquireVscodeApi();</script>
<script nonce="${nonce}" src="${js.toString()}"></script>
</body>
</html>`;
  }

  private pushToWebviews(msg: unknown): void {
    this.sidebar?.webview.postMessage(msg);
    for (const p of this.panels) {
      p.webview.postMessage(msg);
    }
  }

  // ---- message routing (webview -> extension -> daemon) ----

  private async onWebviewMessage(m: any, source: vscode.Webview): Promise<void> {
    if (!m || typeof m.type !== "string") {
      return;
    }
    try {
      switch (m.type) {
        case "requestSnapshot":
          await this.sendSnapshot();
          return;
        case "hostInfo": {
          source.postMessage({
            type: "hostInfo",
            host: hostIndicator(),
            workspace: vscode.workspace.workspaceFolders?.[0]?.uri.fsPath ?? "",
          });
          return;
        }
        case "rpc": {
          if (!this.client) {
            source.postMessage({ type: "rpcError", requestId: m.requestId, error: "daemon offline" });
            return;
          }
          const result = await this.client.request(m.method, m.params ?? {});
          source.postMessage({ type: "rpcResult", requestId: m.requestId, result });
          return;
        }
        case "openClaude": {
          const r = await openClaudeSession(m.sessionId);
          vscode.window.showInformationMessage(r.how);
          return;
        }
        case "openCodex": {
          const r = await openCodexThread(m.threadId, m.title);
          vscode.window.showInformationMessage(r.how);
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
            vscode.window.showWarningMessage("Copied text includes Shared Testing & Sudo content. Handle carefully.");
          }
          return;
        }
        case "revealPrompt": {
          if (!this.client) {
            return;
          }
          const r = await this.client.request("revealEffectivePrompt", { runId: m.runId });
          const doc = await vscode.workspace.openTextDocument({ content: (r as any).text, language: "markdown" });
          await vscode.window.showTextDocument(doc, { preview: true });
          return;
        }
        default:
          return;
      }
    } catch (e: any) {
      source.postMessage({ type: "rpcError", requestId: m.requestId, error: String(e?.message ?? e) });
    }
  }

  // ---- commands ----

  private registerCommands(): void {
    const reg = (id: string, fn: (...a: any[]) => any) =>
      this.context.subscriptions.push(vscode.commands.registerCommand(id, fn));

    reg("agent-conductor.open", () => {
      vscode.commands.executeCommand("agent-conductor.sidebar.focus");
    });
    reg("agent-conductor.buildBatch", () => this.openPanel("planner", "Batch Planner"));
    reg("agent-conductor.diagnostics", () => void this.runDiagnostics());
    reg("agent-conductor.startDevDaemon", () => this.startDevDaemon());
    reg("agent-conductor.repairRuntime", () => void this.repairRuntime());
    reg("agent-conductor.openLogs", () => void this.openLogs());
    reg("agent-conductor.installService", () => void this.installService());
    reg("agent-conductor.uninstallService", () => void this.uninstallService());
  }

  private async runDiagnostics(): Promise<void> {
    if (!this.client) {
      vscode.window.showWarningMessage("Agent Conductor daemon is not connected.");
      return;
    }
    const d = await this.client.request<any>("diagnostics", {});
    const lines = ["# Agent Conductor Diagnostics", ""];
    lines.push(`Daemon: v${d.daemon.version} (pid ${d.daemon.pid}, ${d.daemon.platform ?? ""})`);
    for (const [p, caps] of Object.entries<any>(d.providers ?? {})) {
      lines.push(`${p}: installed=${caps.installed} version=${caps.version} auth=${caps.authState}${caps.notes?.length ? " — " + caps.notes.join("; ") : ""}`);
    }
    const doc = await vscode.workspace.openTextDocument({ content: lines.join("\n"), language: "markdown" });
    await vscode.window.showTextDocument(doc);
  }

  private startDevDaemon(): void {
    const bin = resolveDaemonBinary(this.context);
    if (!bin) {
      vscode.window.showErrorMessage("No agent-conductor daemon binary found. Build it with `npm run daemon:build`.");
      return;
    }
    const term = vscode.window.createTerminal({ name: "Agent Conductor daemon" });
    this.devDaemon = null;
    term.sendText(`"${bin}" serve`);
    term.show();
    this.scheduleReconnect(2500);
  }

  private async repairRuntime(): Promise<void> {
    const bin = resolveDaemonBinary(this.context);
    if (!bin) {
      vscode.window.showErrorMessage("Daemon binary missing; cannot repair hooks.");
      return;
    }
    const { spawnSync } = require("child_process") as typeof import("child_process");
    const r = spawnSync(bin, ["repair-hooks"], { encoding: "utf8" });
    vscode.window.showInformationMessage(`Hook repair:\n${r.stdout || r.stderr}`);
  }

  private async openLogs(): Promise<void> {
    const logDir = path.join(require("os").homedir(), ".agent-conductor", "logs");
    const logFile = path.join(logDir, "daemon.jsonl");
    if (!fs.existsSync(logFile)) {
      vscode.window.showWarningMessage("No daemon log yet.");
      return;
    }
    const doc = await vscode.workspace.openTextDocument(vscode.Uri.file(logFile));
    await vscode.window.showTextDocument(doc, { preview: false });
  }

  private async installService(): Promise<void> {
    const bin = resolveDaemonBinary(this.context);
    if (!bin) {
      vscode.window.showErrorMessage("Daemon binary missing.");
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
    const r = spawnSync(bin, ["install-service"], { encoding: "utf8" });
    const out = (r.stdout || "") + (r.stderr || "");
    vscode.window.showInformationMessage(out.slice(0, 800));
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
    vscode.window.showInformationMessage(((r.stdout || "") + (r.stderr || "")).slice(0, 800));
  }

  private registerWatcher(): void {
    // refresh snapshot when the window gains focus (cheap, event-driven)
    vscode.window.onDidChangeWindowState((s) => {
      if (s.focused) {
        void this.sendSnapshot();
      }
    }, undefined, this.context.subscriptions);
  }
}
