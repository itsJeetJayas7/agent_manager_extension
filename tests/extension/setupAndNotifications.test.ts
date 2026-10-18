import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import * as fs from "fs";
import * as os from "os";
import * as path from "path";
import { computeSetupStatus } from "../../src/extension/setup";
import { Notifier, inQuietHours } from "../../src/extension/notifications";
import { __reset } from "../support/vscode";
import { createRequire } from "module";

vi.mock("fs", async (importOriginal) => {
  const actual = await importOriginal<typeof import("fs")>();
  return { ...actual, existsSync: vi.fn(actual.existsSync) };
});

const require = createRequire(import.meta.url);
const { DAEMON_BIN_DIR, DAEMON_BIN_NAME } = require("../../shared/artifacts.cjs");

let tmp: string;

beforeEach(() => {
  __reset();
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), "ac-setup-"));
});
afterEach(() => {
  vi.mocked(fs.existsSync).mockReset();
  fs.rmSync(tmp, { recursive: true, force: true });
  __reset();
});

function withDaemon(): string {
  const p = path.join(tmp, DAEMON_BIN_DIR, DAEMON_BIN_NAME);
  fs.mkdirSync(path.dirname(p), { recursive: true });
  fs.writeFileSync(p, `#!/bin/sh\necho "agent-conductor 0.1.0 (test/test)"\n`, { mode: 0o755 });
  fs.chmodSync(p, 0o755);
  return tmp;
}

// §7.1 — every check carries the thing that fixes it.
describe("setup checks", () => {
  it("fails loudly when no daemon binary exists and offers the command", () => {
    vi.mocked(fs.existsSync).mockReturnValue(false);
    const status = computeSetupStatus({ extensionPath: tmp }, { daemonConnected: false, workspace: "" });
    const bin = status.checks.find((c) => c.id === "binary")!;
    expect(bin.state).toBe("fail");
    expect(bin.copy).toBeTruthy();
    expect(status.ready).toBe(false);
  });

  it("passes the binary check once one is present", () => {
    const status = computeSetupStatus({ extensionPath: withDaemon() }, { daemonConnected: true, workspace: "/repo" });
    expect(status.checks.find((c) => c.id === "binary")!.state).toBe("ok");
    expect(status.checks.find((c) => c.id === "connected")!.state).toBe("ok");
  });

  it("asks for a workspace folder before anything can run", () => {
    const status = computeSetupStatus({ extensionPath: withDaemon() }, { daemonConnected: true, workspace: "" });
    const ws = status.checks.find((c) => c.id === "workspace")!;
    expect(ws.state).toBe("fail");
    expect(ws.fixCommand).toBe("workbench.action.files.openFolder");
  });

  it("treats Codex as optional so Claude alone is a complete setup", () => {
    const status = computeSetupStatus({ extensionPath: withDaemon() }, { daemonConnected: true, workspace: "/repo" });
    expect(status.checks.find((c) => c.id === "codex")!.optional).toBe(true);
  });

  it("every failing check offers either a one-click fix or a copyable command", () => {
    const status = computeSetupStatus({ extensionPath: tmp }, { daemonConnected: false, workspace: "" });
    for (const c of status.checks) {
      if (c.state === "fail") {
        expect(Boolean(c.fixCommand || c.copy)).toBe(true);
      }
    }
  });
});

// §7.2 — the user should learn what happened overnight.
describe("away notifications", () => {
  function memento(seed: Record<string, unknown> = {}) {
    const store: Record<string, unknown> = { ...seed };
    return {
      get: <T,>(k: string, d: T): T => (store[k] as T) ?? d,
      update: async (k: string, v: unknown) => {
        store[k] = v;
      },
      keys: () => Object.keys(store),
      _store: store,
    } as any;
  }

  const snap = (runs: any[], settings: any = {}) =>
    ({ runs, settings: { quietHoursEnabled: false, quietHoursStart: "", quietHoursEnd: "", ...settings } }) as any;

  it("reports nothing on the very first sight of the queue", () => {
    const n = new Notifier(memento());
    const finished = { id: "a", state: "COMPLETED", finishedAt: new Date().toISOString() };
    expect(n.summariseAway(snap([finished]))).toBeNull();
  });

  it("counts what finished since the last time it looked", async () => {
    const m = memento();
    const n = new Notifier(m);
    const old = new Date(Date.now() - 60 * 60_000).toISOString();
    await n.markSeen(snap([{ id: "old", state: "COMPLETED", finishedAt: old }]));

    const now = new Date().toISOString();
    const summary = n.summariseAway(
      snap([
        { id: "old", state: "COMPLETED", finishedAt: old },
        { id: "b", state: "COMPLETED", finishedAt: now },
        { id: "c", state: "NEEDS_ATTENTION", finishedAt: now },
        { id: "d", state: "FAILED", finishedAt: now },
      ])
    );
    expect(summary).toEqual(expect.objectContaining({ completed: 1, attention: 1, failed: 1 }));
  });

  it("never reports the same run twice", async () => {
    const m = memento();
    const n = new Notifier(m);
    const old = new Date(Date.now() - 60 * 60_000).toISOString();
    await n.markSeen(snap([{ id: "seed", state: "COMPLETED", finishedAt: old }]));
    const s = snap([{ id: "b", state: "COMPLETED", finishedAt: new Date().toISOString() }]);
    expect(n.summariseAway(s)).not.toBeNull();
    await n.markSeen(s);
    expect(n.summariseAway(s)).toBeNull();
  });

  it("ignores runs that are still going", async () => {
    const m = memento();
    const n = new Notifier(m);
    await n.markSeen(snap([]));
    expect(n.summariseAway(snap([{ id: "x", state: "RUNNING" }]))).toBeNull();
  });
});

describe("quiet hours", () => {
  const at = (h: number) => new Date(2026, 2, 4, h, 0, 0);
  const s = (over: any) => ({ settings: { quietHoursEnabled: true, quietHoursStart: "09:00", quietHoursEnd: "18:00", ...over } }) as any;

  it("is quiet outside the working window", () => {
    expect(inQuietHours(s({}), at(7))).toBe(true);
    expect(inQuietHours(s({}), at(12))).toBe(false);
    expect(inQuietHours(s({}), at(22))).toBe(true);
  });

  it("handles a window that wraps past midnight", () => {
    const overnight = s({ quietHoursStart: "22:00", quietHoursEnd: "08:00" });
    expect(inQuietHours(overnight, at(23))).toBe(false);
    expect(inQuietHours(overnight, at(3))).toBe(false);
    expect(inQuietHours(overnight, at(12))).toBe(true);
  });

  it("is never quiet when disabled or misconfigured", () => {
    expect(inQuietHours(s({ quietHoursEnabled: false }), at(3))).toBe(false);
    expect(inQuietHours(s({ quietHoursStart: "nonsense" }), at(3))).toBe(false);
    expect(inQuietHours(s({ quietHoursStart: "09:00", quietHoursEnd: "09:00" }), at(3))).toBe(false);
    expect(inQuietHours(null, at(3))).toBe(false);
  });
});
