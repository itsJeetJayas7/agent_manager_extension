import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as vscode from "vscode";
import { Notifier } from "../../src/extension/notifications";
import { __config, __reset } from "../support/vscode";

const event = { provider: "claude", sessionId: "manual", activeRunCount: 1 };
const notifier = new Notifier({ get: () => undefined, update: async () => undefined } as any);

beforeEach(__reset);
afterEach(() => { vi.restoreAllMocks(); __reset(); });

describe("external overlap notifications", () => {
  it.each([["claude", "Claude Code", 1], ["codex", "Codex", 2]])(
    "informs about %s (%s) alongside %i active runs without pausing",
    async (provider, title, activeRunCount) => {
      const show = vi.spyOn(vscode.window, "showInformationMessage").mockResolvedValue(undefined);
      const command = vi.spyOn(vscode.commands, "executeCommand");
      await notifier.reportExternalOverlap({ ...event, provider, activeRunCount });
      expect(show).toHaveBeenCalledWith(
        expect.stringContaining(`queue is running right now (${activeRunCount} active run`),
        "Show queue", "Pause queue…"
      );
      expect(show.mock.calls[0][0]).toContain(`external ${title} run will run alongside it`);
      expect(command).not.toHaveBeenCalled();
    }
  );

  it.each([["Show queue", "agent-conductor.open"], ["Pause queue…", "agent-conductor.pauseQueue"]])(
    "only acts when the user chooses %s", async (choice, action) => {
      vi.spyOn(vscode.window, "showInformationMessage").mockResolvedValue(choice as any);
      const command = vi.spyOn(vscode.commands, "executeCommand");
      await notifier.reportExternalOverlap(event);
      expect(command).toHaveBeenCalledExactlyOnceWith(action);
    }
  );

  it("can be muted without affecting scheduling", async () => {
    __config["agentConductor.notifyOnExternalOverlap"] = false;
    const show = vi.spyOn(vscode.window, "showInformationMessage");
    await notifier.reportExternalOverlap(event);
    expect(show).not.toHaveBeenCalled();
  });

  it("distinguishes an already paused queue from work still finishing", async () => {
    const show = vi.spyOn(vscode.window, "showInformationMessage").mockResolvedValue(undefined);
    await notifier.reportExternalOverlap({ ...event, queuePaused: true });
    expect(show).toHaveBeenCalledWith(
      expect.stringContaining("1 run is still active while the queue is paused"), "Show queue"
    );
  });

  it.each([null, undefined, {}, { ...event, activeRunCount: 0 }, { ...event, activeRunCount: -1 },
    { ...event, activeRunCount: NaN }, { ...event, activeRunCount: 1.5 }, { ...event, provider: "unknown" }])(
    "ignores empty, idle, or invalid overlap events (%j)", async (payload) => {
      const show = vi.spyOn(vscode.window, "showInformationMessage");
      await notifier.reportExternalOverlap(payload);
      expect(show).not.toHaveBeenCalled();
    }
  );
});
