import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import * as fs from "fs";
import * as os from "os";
import * as path from "path";
import { createRequire } from "module";
import { resolveDaemonBinaryDetailed, validateDaemonBinary } from "../../src/extension/configuration";
import { __reset, __config } from "../support/vscode";

vi.mock("fs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("fs")>();
  return { ...actual, existsSync: vi.fn(actual.existsSync) };
});

const require = createRequire(import.meta.url);
const { DAEMON_BIN_DIR, DAEMON_BIN_NAME, daemonBinNameFor, goPlatform } =
  require("../../shared/artifacts.cjs");

let tmp: string;

/** A stand-in daemon that answers `version` exactly like the real one. */
function writeFakeDaemon(p: string, mode = 0o755): void {
  fs.mkdirSync(path.dirname(p), { recursive: true });
  fs.writeFileSync(p, `#!/bin/sh\necho "agent-conductor 0.1.0 (test/test)"\n`, { mode });
  fs.chmodSync(p, mode);
}

beforeEach(() => {
  __reset();
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), "ac-bin-"));
});

afterEach(() => {
  vi.mocked(fs.existsSync).mockReset();
  fs.rmSync(tmp, { recursive: true, force: true });
  __reset();
});

describe("daemon binary resolution", () => {
  // §1.3 — configuration.ts looked in `bin/` while scripts/build.mjs wrote to
  // `dist/bin/`, so the daemon was never found and the UI said "offline"
  // forever.
  it("finds the binary at the path the build script actually writes", () => {
    writeFakeDaemon(path.join(tmp, DAEMON_BIN_DIR, DAEMON_BIN_NAME));
    const res = resolveDaemonBinaryDetailed({ extensionPath: tmp });
    expect(res.path).toBe(path.join(tmp, DAEMON_BIN_DIR, DAEMON_BIN_NAME));
    expect(res.source).toBe("bundled");
    expect(res.problem).toBeUndefined();
  });

  it("falls back to the platform-suffixed binary", () => {
    const { goos, goarch } = goPlatform(process.platform, process.arch);
    writeFakeDaemon(path.join(tmp, DAEMON_BIN_DIR, daemonBinNameFor(goos, goarch)));
    const res = resolveDaemonBinaryDetailed({ extensionPath: tmp });
    expect(res.source).toBe("bundled-platform");
    expect(res.path).toContain(`${goos}-${goarch}`);
  });

  it("prefers an explicitly configured path", () => {
    const custom = path.join(tmp, "custom", "agent-conductor");
    writeFakeDaemon(custom);
    writeFakeDaemon(path.join(tmp, DAEMON_BIN_DIR, DAEMON_BIN_NAME));
    __config["agentConductor.daemonPath"] = custom;
    expect(resolveDaemonBinaryDetailed({ extensionPath: tmp }).source).toBe("setting");
  });

  // VSIX extraction does not reliably preserve the execute bit.
  it("restores the execute bit instead of reporting offline", () => {
    const p = path.join(tmp, DAEMON_BIN_DIR, DAEMON_BIN_NAME);
    writeFakeDaemon(p, 0o644);
    expect(() => fs.accessSync(p, fs.constants.X_OK)).toThrow();
    const res = resolveDaemonBinaryDetailed({ extensionPath: tmp });
    expect(res.path).toBe(p);
    expect(fs.statSync(p).mode & 0o111).toBeTruthy();
  });

  it("explains the problem when nothing is usable", () => {
    // A developer's installed daemon must not turn this absence case green.
    vi.mocked(fs.existsSync).mockReturnValue(false);
    const res = resolveDaemonBinaryDetailed({ extensionPath: tmp });
    expect(res.path).toBe("");
    expect(res.problem?.code).toBe("no-binary");
    expect(res.problem?.message).toBeTruthy();
  });

  it("rejects a binary that is not the conductor daemon", () => {
    const p = path.join(tmp, DAEMON_BIN_DIR, DAEMON_BIN_NAME);
    fs.mkdirSync(path.dirname(p), { recursive: true });
    fs.writeFileSync(p, `#!/bin/sh\necho "something else entirely"\n`, { mode: 0o755 });
    const v = validateDaemonBinary(p);
    expect(v.ok).toBe(false);
    if (!v.ok) {
      expect(v.problem.code).toBe("unusable");
    }
  });
});
