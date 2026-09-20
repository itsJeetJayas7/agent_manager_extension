// configuration reads VS Code settings and resolves the daemon binary.
import * as vscode from "vscode";
import * as fs from "fs";
import * as os from "os";
import * as path from "path";
import { spawnSync } from "child_process";
import {
  DAEMON_BIN_DIR,
  DAEMON_BIN_NAME,
  daemonBinNameFor,
  goPlatform,
} from "../../shared/artifacts.cjs";

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

/** Why a daemon binary could not be used, with the exact remedy. */
export interface DaemonBinaryProblem {
  /** Stable code for tests and telemetry. */
  code: "no-binary" | "not-executable" | "quarantined" | "wrong-arch" | "unusable";
  /** One sentence, shown verbatim in the UI. */
  message: string;
  /** A command the user can copy and run, when one exists. */
  remedy?: string;
}

export interface DaemonBinaryResolution {
  /** Absolute path, or "" when nothing usable was found. */
  path: string;
  /** Where it came from, for diagnostics. */
  source: "setting" | "bundled" | "bundled-platform" | "installed" | "path" | "none";
  /** Version string reported by `<bin> version`, when it ran. */
  version?: string;
  problem?: DaemonBinaryProblem;
}

/** Candidate paths in priority order: setting > bundled > stable install > PATH. */
function daemonCandidates(
  extensionPath: string
): { path: string; source: DaemonBinaryResolution["source"] }[] {
  const { goos, goarch } = goPlatform(process.platform, process.arch);
  const configured = cfg<string>("daemonPath", "").trim();
  const out: { path: string; source: DaemonBinaryResolution["source"] }[] = [];
  if (configured) {
    out.push({ path: configured, source: "setting" });
  }
  // The build writes dist/bin/agent-conductor; the VSIX ships it at the same
  // relative path. This used to look in bin/ and never found anything.
  out.push({ path: path.join(extensionPath, DAEMON_BIN_DIR, DAEMON_BIN_NAME), source: "bundled" });
  out.push({
    path: path.join(extensionPath, DAEMON_BIN_DIR, daemonBinNameFor(goos, goarch)),
    source: "bundled-platform",
  });
  out.push({ path: path.join(os.homedir(), ".local", "bin", DAEMON_BIN_NAME), source: "installed" });
  const onPath = which(DAEMON_BIN_NAME);
  if (onPath) {
    out.push({ path: onPath, source: "path" });
  }
  return out;
}

function which(bin: string): string {
  const r = spawnSync("which", [bin], { encoding: "utf8" });
  return r.status === 0 ? r.stdout.trim() : "";
}

/** True when the file carries macOS's com.apple.quarantine extended attribute. */
export function isQuarantined(binPath: string): boolean {
  if (process.platform !== "darwin") {
    return false;
  }
  const r = spawnSync("xattr", ["-p", "com.apple.quarantine", binPath], { encoding: "utf8" });
  return r.status === 0 && r.stdout.trim() !== "";
}

/**
 * Make a bundled binary executable. VSIX extraction does not reliably preserve
 * the execute bit, which silently turns "daemon shipped" into "daemon offline".
 */
function ensureExecutable(binPath: string): boolean {
  try {
    fs.accessSync(binPath, fs.constants.X_OK);
    return true;
  } catch {
    // fall through to chmod
  }
  try {
    fs.chmodSync(binPath, 0o755);
    fs.accessSync(binPath, fs.constants.X_OK);
    return true;
  } catch {
    return false;
  }
}

/**
 * Run `<bin> version` and confirm it is really our daemon. Catches wrong
 * architecture, Gatekeeper quarantine, and truncated downloads — all of which
 * previously surfaced only as a permanently "offline" daemon.
 */
export function validateDaemonBinary(
  binPath: string
): { ok: true; version: string } | { ok: false; problem: DaemonBinaryProblem } {
  const r = spawnSync(binPath, ["version"], { encoding: "utf8", timeout: 5000 });
  const out = `${r.stdout ?? ""}${r.stderr ?? ""}`.trim();
  if (r.status === 0 && out.startsWith(DAEMON_BIN_NAME)) {
    return { ok: true, version: out };
  }
  if (isQuarantined(binPath)) {
    return {
      ok: false,
      problem: {
        code: "quarantined",
        message: "macOS quarantined the Agent Conductor daemon, so it cannot run.",
        remedy: `xattr -d com.apple.quarantine "${binPath}"`,
      },
    };
  }
  const combined = out.toLowerCase();
  if (
    combined.includes("bad cpu type") ||
    combined.includes("exec format error") ||
    combined.includes("cannot execute binary file")
  ) {
    const { goos, goarch } = goPlatform(process.platform, process.arch);
    return {
      ok: false,
      problem: {
        code: "wrong-arch",
        message: `The bundled daemon was built for a different architecture (this machine is ${goos}/${goarch}).`,
        remedy: "npm run daemon:build",
      },
    };
  }
  return {
    ok: false,
    problem: {
      code: "unusable",
      message: out
        ? `The daemon binary did not start: ${out.split("\n")[0].slice(0, 160)}`
        : "The daemon binary did not start and produced no output.",
      remedy: `"${binPath}" version`,
    },
  };
}

/**
 * Locate and validate the daemon binary. Unlike the previous version this
 * reports *why* a binary is unusable instead of silently reporting "offline".
 */
export function resolveDaemonBinaryDetailed(
  context: Pick<vscode.ExtensionContext, "extensionPath">
): DaemonBinaryResolution {
  let lastProblem: DaemonBinaryProblem | undefined;
  for (const c of daemonCandidates(context.extensionPath)) {
    if (!fs.existsSync(c.path)) {
      continue;
    }
    if (!ensureExecutable(c.path)) {
      lastProblem = {
        code: "not-executable",
        message: "The daemon binary is not executable and could not be made executable.",
        remedy: `chmod +x "${c.path}"`,
      };
      continue;
    }
    const v = validateDaemonBinary(c.path);
    if (v.ok) {
      return { path: c.path, source: c.source, version: v.version };
    }
    lastProblem = v.problem;
  }
  return {
    path: "",
    source: "none",
    problem: lastProblem ?? {
      code: "no-binary",
      message: "No agent-conductor daemon binary was found.",
      remedy: "npm run daemon:build",
    },
  };
}

/** Locate the daemon binary, or "" when none is usable. */
export function resolveDaemonBinary(
  context: Pick<vscode.ExtensionContext, "extensionPath">
): string {
  return resolveDaemonBinaryDetailed(context).path;
}

export interface ProviderRuntime {
  installed: boolean;
  path: string;
  version: string;
}

/** Detect provider runtimes for onboarding diagnostics (§7.1). */
export function detectProviderRuntimes(): Record<"claude" | "codex", ProviderRuntime> {
  const probe = (bin: string, extraDirs: string[]): ProviderRuntime => {
    let p = which(bin);
    if (!p) {
      for (const d of extraDirs) {
        const candidate = path.join(d, bin);
        if (fs.existsSync(candidate)) {
          p = candidate;
          break;
        }
      }
    }
    if (!p) {
      return { installed: false, path: "", version: "" };
    }
    const r = spawnSync(p, ["--version"], { encoding: "utf8", timeout: 5000 });
    return {
      installed: true,
      path: p,
      version: `${r.stdout ?? ""}`.trim().split("\n")[0] ?? "",
    };
  };
  // LaunchAgents and GUI-launched VS Code often have a minimal PATH, so look in
  // the usual user-level install roots too (§9).
  const extra = [
    path.join(os.homedir(), ".local", "bin"),
    "/opt/homebrew/bin",
    "/usr/local/bin",
    path.join(os.homedir(), ".npm-global", "bin"),
  ];
  return { claude: probe("claude", extra), codex: probe("codex", extra) };
}

/** Remote host indicator for the sidebar header. */
export function hostIndicator(): string {
  const remote = vscode.env.remoteName;
  if (remote) {
    return `Remote · ${os.hostname()}`;
  }
  return "Local";
}
