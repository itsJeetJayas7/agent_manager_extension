// configuration reads VS Code settings and resolves the daemon binary.
import * as vscode from "vscode";
import * as fs from "fs";
import * as os from "os";
import * as path from "path";
import { spawnSync } from "child_process";

export function cfg<T>(key: string, fallback: T): T {
  const v = vscode.workspace.getConfiguration("agentConductor").get<T>(key);
  return v === undefined || v === null ? fallback : v;
}

export function mockProviders(): boolean {
  return cfg<boolean>("mockProviders", false);
}

export function verificationCommands(): string[] {
  return cfg<string[]>("verificationCommands", []);
}

/** Locate the daemon binary: setting > bundled > stable install > PATH. */
export function resolveDaemonBinary(context: vscode.ExtensionContext): string {
  const configured = cfg<string>("daemonPath", "");
  const candidates = [
    configured,
    path.join(context.extensionPath, "bin", "agent-conductor"),
    path.join(context.extensionPath, "bin", process.platform === "darwin" && process.arch === "arm64" ? "agent-conductor-darwin-arm64" : "agent-conductor-linux-amd64"),
    path.join(os.homedir(), ".local", "bin", "agent-conductor"),
    "agent-conductor",
  ].filter(Boolean) as string[];
  for (const c of candidates) {
    if (c === "agent-conductor") {
      const r = spawnSync("which", ["agent-conductor"], { encoding: "utf8" });
      if (r.status === 0) {
        return r.stdout.trim();
      }
      continue;
    }
    try {
      fs.accessSync(c, fs.constants.X_OK);
      return c;
    } catch {
      // try next
    }
  }
  return "";
}

/** Detect provider runtimes for onboarding diagnostics. */
export function detectProviderRuntimes(): { claude: boolean; codex: boolean; claudePath: string; codexPath: string } {
  const which = (bin: string): string => {
    const r = spawnSync("which", [bin], { encoding: "utf8" });
    return r.status === 0 ? r.stdout.trim() : "";
  };
  const claudePath = which("claude") || path.join(os.homedir(), ".local", "bin", "claude");
  const codexPath = which("codex");
  return {
    claude: claudePath !== "" && fs.existsSync(claudePath),
    codex: codexPath !== "",
    claudePath,
    codexPath,
  };
}

/** Remote host indicator for the sidebar header. */
export function hostIndicator(): string {
  const remote = vscode.env.remoteName;
  if (remote) {
    return `Remote · ${os.hostname()}`;
  }
  return "Local";
}
