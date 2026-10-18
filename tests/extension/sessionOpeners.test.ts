import { beforeEach, describe, expect, it, vi } from "vitest";
import * as vscode from "vscode";
import { ClaudeSessionOpener } from "../../src/extension/sessionOpeners";

const config = { globalValue: undefined as string | undefined, update: vi.fn() };
vi.mock("vscode", () => {
  const uri = (scheme: string, path: string, authority = ""): any => ({
    scheme, path, fsPath: path, authority,
    with: (p: any) => uri(p.scheme ?? scheme, p.path ?? path, p.authority ?? authority),
  });
  return {
    Uri: { file: (path: string) => uri("file", path) },
    ConfigurationTarget: { Global: 1 },
    workspace: {
      workspaceFolders: [],
      getConfiguration: () => ({
        inspect: () => ({ globalValue: config.globalValue }),
        update: (...args: unknown[]) => { config.globalValue = args[1] as string | undefined; return config.update(...args); },
      }),
    },
    env: { remoteName: undefined, clipboard: { writeText: vi.fn() }, openExternal: vi.fn() },
    extensions: { getExtension: vi.fn() },
    commands: { getCommands: vi.fn(), executeCommand: vi.fn() },
    window: { showWarningMessage: vi.fn(), showInformationMessage: vi.fn() },
  };
});

const id = "550e8400-e29b-41d4-a716-446655440000";
const ours = "/home/u/.agent-conductor/bin/agent-conductor-claude-wrapper";
let values: Map<string, unknown>;
let rpc: ReturnType<typeof vi.fn>;
let opener: ClaudeSessionOpener;
const target = { sessionId: id, cwd: "/project" };
function folders(path: string, remote = false) {
  (vscode.workspace as any).workspaceFolders = [{ uri: vscode.Uri.file(path).with(remote ? { scheme: "vscode-remote", authority: "ssh-remote+vps" } : {}) }];
}
const attached = { live: true, runId: "r1", status: { pid: 4242, viewers: 1, busy: "conductor" } };
let viewersBeforeOpen = 0;
beforeEach(() => {
  vi.resetAllMocks();
  viewersBeforeOpen = 0;
  (vscode.env as any).remoteName = undefined;
  config.globalValue = ours;
  folders("/project");
  values = new Map();
  let opened = false;
  vi.mocked(vscode.commands.executeCommand).mockImplementation(async () => { opened = true; });
  rpc = vi.fn(async (method: string) => {
    if (method === "installClaudeWrapper") return { path: ours };
    if (method === "prepareClaudeAttach") return { paused: true };
    if (method === "releaseClaudeAttach") return { paused: false };
    return opened ? attached : { ...attached, status: { ...attached.status, viewers: viewersBeforeOpen } };
  });
  opener = new ClaudeSessionOpener({
    get: (key: string) => values.get(key),
    update: async (key: string, value: unknown) => { values.set(key, value); },
  } as any, rpc as any, async () => undefined);
  vi.mocked(vscode.extensions.getExtension).mockReturnValue({ activate: vi.fn().mockResolvedValue({}) } as any);
  vi.mocked(vscode.commands.getCommands).mockResolvedValue(["claude-vscode.primaryEditor.open"]);
});

describe("Claude session handoff", () => {
  it("opens the exact session in the current extension host and reports the daemon-confirmed attachment", async () => {
    const result = await opener.open(target);
    expect(result).toMatchObject({ opened: true, live: true });
    expect(result.how).toContain("4242");
    expect(vscode.extensions.getExtension("anthropic.claude-code")!.activate).toHaveBeenCalled();
    expect(vscode.commands.executeCommand).toHaveBeenCalledWith("claude-vscode.primaryEditor.open", id);
    expect(rpc).toHaveBeenCalledWith("claudeSessionLive", { sessionId: id });
    expect(vscode.env.openExternal).not.toHaveBeenCalled();
    expect(vscode.env.clipboard.writeText).not.toHaveBeenCalled();
  });

  it("pauses the run only between the tab's history read and its attachment", async () => {
    await opener.open(target);
    const order = rpc.mock.calls.map(([m]) => m);
    expect(order.indexOf("prepareClaudeAttach")).toBeLessThan(order.lastIndexOf("claudeSessionLive"));
    expect(order).toContain("releaseClaudeAttach");
    expect(vi.mocked(vscode.commands.executeCommand).mock.invocationCallOrder[0])
      .toBeGreaterThan(rpc.mock.invocationCallOrder[order.indexOf("prepareClaudeAttach")]);
  });

  it("only focuses a tab that is already attached, without pausing the run", async () => {
    viewersBeforeOpen = 1;
    const result = await opener.open(target);
    expect(result).toMatchObject({ opened: true, live: true });
    expect(result.how).toMatch(/Focused/);
    expect(rpc).not.toHaveBeenCalledWith("prepareClaudeAttach", expect.anything());
    expect(vscode.commands.executeCommand).toHaveBeenCalledWith("claude-vscode.primaryEditor.open", id);
  });

  // The regression: a tab appearing was reported as success while the run
  // was not shown live. Only the daemon seeing an attached tab counts.
  it("does not claim a live view when no tab attaches to the run's process", async () => {
    rpc.mockImplementation(async (method: string) => method === "prepareClaudeAttach" ? { paused: true } : { live: true, status: { pid: 4242, viewers: 0, busy: "conductor" } });
    const result = await opener.open(target);
    expect(result).toMatchObject({ opened: true, live: false });
    expect(result.how).toMatch(/did not attach/);
    expect(rpc.mock.calls.filter(([m]) => m === "claudeSessionLive").length).toBeGreaterThan(10);
  });

  it("asks once before setting Claude Code's machine-wide wrapper, then attaches", async () => {
    config.globalValue = undefined;
    vi.mocked(vscode.window.showInformationMessage).mockResolvedValue("Enable live view" as any);
    const result = await opener.open(target);
    expect(vscode.window.showInformationMessage).toHaveBeenCalledWith(expect.stringContaining("live"), expect.objectContaining({ modal: true }), "Enable live view", "Open without live view");
    expect(rpc).toHaveBeenCalledWith("installClaudeWrapper", {});
    expect(config.update).toHaveBeenCalledWith("claudeProcessWrapper", ours, 1);
    expect(result.live).toBe(true);
  });

  it("respects a declined live view and says the tab will not update", async () => {
    config.globalValue = undefined;
    vi.mocked(vscode.window.showInformationMessage).mockResolvedValue("Open without live view" as any);
    const first = await opener.open(target);
    expect(first).toMatchObject({ opened: true, live: false });
    expect(first.how).toMatch(/not attached/);
    expect(config.update).not.toHaveBeenCalled();
    await opener.open(target);
    expect(vscode.window.showInformationMessage).toHaveBeenCalledTimes(1);
  });

  it("tells the user to update a daemon that predates live view", async () => {
    config.globalValue = undefined;
    rpc.mockRejectedValue(new Error("UNKNOWN_METHOD: unknown method installClaudeWrapper"));
    const result = await opener.enableLiveView();
    expect(result.ok).toBe(false);
    expect(result.how).toContain("Install Background Service");
    expect(config.update).not.toHaveBeenCalled();
  });

  it("never replaces a wrapper the user configured", async () => {
    config.globalValue = "/opt/corp/claude-launcher";
    const result = await opener.open(target);
    expect(result.live).toBe(false);
    expect(config.update).not.toHaveBeenCalled();
    expect((await opener.enableLiveView()).ok).toBe(false);
  });

  it("removes its wrapper setting when the script is gone, so Claude Code keeps starting", async () => {
    config.globalValue = "/nonexistent/agent-conductor-claude-wrapper";
    await opener.repairLiveView();
    expect(config.update).toHaveBeenCalledWith("claudeProcessWrapper", undefined, 1);
  });

  it("hands a worktree session to a new window and consumes the handoff once", async () => {
    const worktree = { ...target, cwd: "/project/worktrees/one" };
    expect((await opener.open(worktree)).opened).toBe(true);
    expect(vscode.commands.executeCommand).toHaveBeenCalledWith("vscode.openFolder", expect.objectContaining({ fsPath: worktree.cwd }), { forceNewWindow: true });
    expect(vscode.commands.executeCommand).not.toHaveBeenCalledWith("claude-vscode.primaryEditor.open", id);
    folders(worktree.cwd);
    await opener.resumePending();
    await opener.resumePending();
    expect(vi.mocked(vscode.commands.executeCommand).mock.calls.filter(([c]) => c === "claude-vscode.primaryEditor.open")).toHaveLength(1);
  });

  it("preserves SSH authority when opening the run's folder", async () => {
    (vscode.env as any).remoteName = "ssh-remote";
    folders("/project", true);
    await opener.open({ ...target, cwd: "/srv/worktree" });
    expect(vscode.commands.executeCommand).toHaveBeenCalledWith("vscode.openFolder", expect.objectContaining({ scheme: "vscode-remote", authority: "ssh-remote+vps", path: "/srv/worktree" }), { forceNewWindow: true });
  });

  it("does not route a remote run to a local file URI without a workspace", async () => {
    (vscode.env as any).remoteName = "ssh-remote";
    (vscode.workspace as any).workspaceFolders = [];
    expect((await opener.open(target)).opened).toBe(false);
    expect(vscode.commands.executeCommand).not.toHaveBeenCalled();
  });

  it("does not replay an expired handoff", async () => {
    await opener.open({ ...target, cwd: "/worktree" });
    for (const [key, value] of values) values.set(key, { ...(value as any), expiresAt: 1 });
    folders("/worktree");
    vi.mocked(vscode.commands.executeCommand).mockClear();
    await opener.resumePending();
    expect(vscode.commands.executeCommand).not.toHaveBeenCalled();
  });

  it.each(["missing", "activation", "command", "unsupported"])("reports %s failure and copies a usable fallback", async (type) => {
    if (type === "missing") vi.mocked(vscode.extensions.getExtension).mockReturnValue(undefined);
    if (type === "activation") vi.mocked(vscode.extensions.getExtension).mockReturnValue({ activate: vi.fn().mockRejectedValue(new Error("disabled")) } as any);
    if (type === "command") vi.mocked(vscode.commands.executeCommand).mockRejectedValue(new Error("rejected"));
    if (type === "unsupported") vi.mocked(vscode.commands.getCommands).mockResolvedValue([]);
    const result = await opener.open(target);
    expect(result.opened).toBe(false);
    expect(result.how).toContain("Live activity");
    expect(vscode.env.clipboard.writeText).toHaveBeenCalledWith(`cd -- '/project' && claude --resume '${id}'`);
  });

  it("cleans up failed window handoffs", async () => {
    vi.mocked(vscode.commands.executeCommand).mockRejectedValue(new Error("window failed"));
    expect((await opener.open({ ...target, cwd: "/worktree" })).opened).toBe(false);
    expect([...values.values()].filter(Boolean)).toHaveLength(0);
  });

  it.each(["", "abc&prompt=hello", "../../other", "hello\nworld"])("rejects invalid session id %j", async sessionId => {
    expect((await opener.open({ ...target, sessionId })).opened).toBe(false);
    expect(vscode.commands.executeCommand).not.toHaveBeenCalled();
  });
});
