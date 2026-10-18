import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { render, screen, fireEvent, waitFor, within, cleanup } from "@testing-library/react";
import React from "react";
import QueueView from "../../webview/src/views/QueueView";
import { makeSnapshot, makeRun, makeState, idlePause } from "./snapshot";

const postMessage = vi.fn();

beforeEach(() => {
  const store: any = {};
  (window as any).__acVscode = {
    postMessage,
    getState: () => store.value ?? {},
    setState: (s: any) => {
      store.value = s;
    },
  };
  postMessage.mockReset();
  // Answer rpc/confirm exactly as the extension host would.
  postMessage.mockImplementation((msg: any) => {
    if (msg?.type === "rpc" && typeof msg.requestId === "number") {
      const result = msg.method === "enqueueRun" ? { runId: "r1", batchId: "b1" } : {};
      setTimeout(() => {
        window.dispatchEvent(
          new MessageEvent("message", { data: { type: "rpcResult", requestId: msg.requestId, result } })
        );
      }, 0);
    }
    if (msg?.type === "confirm") {
      setTimeout(() => {
        window.dispatchEvent(
          new MessageEvent("message", {
            data: { type: "confirmResult", requestId: msg.requestId, confirmed: true },
          })
        );
      }, 0);
    }
  });
});

function rpcCalls(method: string) {
  return postMessage.mock.calls.filter((c: any[]) => c[0]?.type === "rpc" && c[0].method === method);
}

// vitest runs with globals disabled, so React Testing Library's automatic
// cleanup does not fire; unmount explicitly between tests.
afterEach(cleanup);

describe("QueueView", () => {
  it.each([
    ["SessionStart", false], ["UserPromptSubmit", true], ["PreToolUse", true],
    ["PostToolUse", true], ["PostToolUseFailure", true], ["Stop", false],
    ["StopFailure", false], ["Interrupt", false], ["SessionEnd", false],
  ])("shows external activity for %s only when a turn is active", (lastEvent, busy) => {
    const external = { claude: [{
      provider: "claude", nativeId: "manual", kind: "external", cwd: "/project",
      lastEvent, lastSeenAt: new Date().toISOString(),
    }] };
    render(<QueueView state={makeState(makeSnapshot({ external }))} />);
    expect(!!screen.queryByTitle("claude: external agent active · queue continues normally")).toBe(busy);
    expect(!!screen.queryByTitle("claude: ready")).toBe(!busy);
  });

  it.each(["managed", "stale"])("does not warn about a %s session", (type) => {
    const external = { claude: [{
      provider: "claude", nativeId: "session", kind: type === "managed" ? "managed" : "external",
      cwd: "/project", lastEvent: "UserPromptSubmit",
      lastSeenAt: new Date(Date.now() - (type === "stale" ? 46 * 60_000 : 0)).toISOString(),
    }] };
    render(<QueueView state={makeState(makeSnapshot({ external }))} />);
    expect(screen.getByTitle("claude: ready")).toBeTruthy();
  });

  it("passes the effective worktree to the native Claude opener", () => {
    render(<QueueView state={makeState(makeSnapshot({ runs: [makeRun({ state: "RUNNING", provider: "claude", providerSessionId: "session-id", cwd: "/base", worktreePath: "/worktree" })] }))} />);
    fireEvent.click(screen.getByRole("button", { name: "Open in Claude Code" }));
    expect(postMessage).toHaveBeenCalledWith({ type: "openClaude", sessionId: "session-id", cwd: "/worktree" });
  });

  it("renders the composer and a composed empty state", () => {
    render(<QueueView state={makeState(makeSnapshot())} />);
    expect(screen.getByLabelText("Prompt")).toBeTruthy();
    expect(screen.getByText(/the queue is empty/i)).toBeTruthy();
    expect(screen.getByText(/nothing is running/i)).toBeTruthy();
    expect(screen.getAllByRole("button", { name: /queue a run/i }).length).toBeGreaterThan(0);
  });

  it("queues a run with the composer fields on Cmd+Enter", async () => {
    render(<QueueView state={makeState(makeSnapshot())} />);
    const ta = screen.getByLabelText("Prompt") as HTMLTextAreaElement;
    fireEvent.change(ta, { target: { value: "Do the thing" } });
    fireEvent.keyDown(ta, { key: "Enter", metaKey: true });
    await waitFor(() => {
      const call = rpcCalls("enqueueRun")[0];
      expect(call).toBeTruthy();
      expect(call[0].params.prompt).toBe("Do the thing");
      expect(call[0].params.permission).toBe("bypass");
      expect(call[0].params.runNext).toBe(false);
    });
  });

  it("sends runNext when Run next is used", async () => {
    render(<QueueView state={makeState(makeSnapshot())} />);
    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "urgent" } });
    fireEvent.click(screen.getByRole("button", { name: "Run next" }));
    await waitFor(() => expect(rpcCalls("enqueueRun")[0][0].params.runNext).toBe(true));
  });

  // §3.3 — the composer used to call revealSecretBlock on mount purely to test
  // existence, decrypting a secret and shipping it over the socket.
  it("never reveals the secret block just to know whether one exists", () => {
    render(<QueueView state={makeState(makeSnapshot({ hasSecretBlock: true }))} />);
    expect(rpcCalls("revealSecretBlock")).toHaveLength(0);
    const box = screen.getByLabelText(/attach shared testing/i) as HTMLInputElement;
    expect(box.disabled).toBe(false);
  });

  it("disables the secret checkbox when no block is stored", () => {
    render(<QueueView state={makeState(makeSnapshot({ hasSecretBlock: false }))} />);
    const box = screen.getByLabelText(/attach shared testing/i) as HTMLInputElement;
    expect(box.disabled).toBe(true);
    expect(screen.getByText(/none saved yet/i)).toBeTruthy();
  });

  it("remembers composer choices per workspace", async () => {
    const { unmount } = render(<QueueView state={makeState(makeSnapshot())} />);
    fireEvent.change(screen.getByLabelText("Permission"), { target: { value: "plan" } });
    unmount();
    render(<QueueView state={makeState(makeSnapshot())} />);
    expect((screen.getByLabelText("Permission") as HTMLSelectElement).value).toBe("plan");
  });

  it("shows the active run as the hero with a provider opener", () => {
    const run = makeRun({ state: "RUNNING", providerSessionId: "sess-123", startedAt: new Date().toISOString() });
    render(<QueueView state={makeState(makeSnapshot({ runs: [run] }))} />);
    expect(screen.getByText("Open in Claude Code")).toBeTruthy();
    expect(screen.getByText("Running")).toBeTruthy();
  });

  it("marks quota-waiting runs with a reset hint", () => {
    const run = makeRun({
      id: "r2",
      state: "WAITING_FOR_QUOTA",
      provider: "codex",
      nextEligibleAt: new Date(Date.now() + 3600_000).toISOString(),
    });
    render(<QueueView state={makeState(makeSnapshot({ runs: [run] }))} />);
    expect(screen.getByTitle(/resumes automatically/i)).toBeTruthy();
  });
});

describe("pause", () => {
  it("offers a pause control in the queue header, not three clicks deep", () => {
    render(<QueueView state={makeState(makeSnapshot())} />);
    expect(screen.getByRole("button", { name: /pause/i })).toBeTruthy();
  });

  it("sends scope, duration and reason from the popover", async () => {
    render(<QueueView state={makeState(makeSnapshot())} />);
    fireEvent.click(screen.getByRole("button", { name: /pause/i }));
    const dialog = screen.getByRole("dialog", { name: /pause queue/i });
    fireEvent.click(within(dialog).getByRole("button", { name: "30m" }));
    fireEvent.click(within(dialog).getByLabelText(/also pause the run in progress/i));
    fireEvent.change(within(dialog).getByLabelText(/note/i), { target: { value: "API review" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Pause" }));
    await waitFor(() => {
      const call = rpcCalls("pauseQueue")[0];
      expect(call).toBeTruthy();
      expect(call[0].params.durationMs).toBe(30 * 60_000);
      expect(call[0].params.scope).toBe("INCLUDE_ACTIVE");
      expect(call[0].params.reason).toBe("API review");
    });
  });

  it("pauses indefinitely when 'until I resume' is chosen", async () => {
    render(<QueueView state={makeState(makeSnapshot())} />);
    fireEvent.click(screen.getByRole("button", { name: /pause/i }));
    const dialog = screen.getByRole("dialog", { name: /pause queue/i });
    fireEvent.click(within(dialog).getByLabelText(/until i resume/i));
    fireEvent.click(within(dialog).getByRole("button", { name: "Pause" }));
    await waitFor(() => expect(rpcCalls("pauseQueue")[0][0].params.durationMs).toBeUndefined());
  });

  // §5.6 — the countdown is computed from the absolute resumeAt, so it cannot
  // drift and survives the machine sleeping.
  it("shows a live countdown from the absolute resume time", () => {
    const pause = {
      ...idlePause,
      paused: true,
      resumeAt: new Date(Date.now() + 23 * 60_000 + 41_000).toISOString(),
      reason: "waiting on the API review",
    };
    render(<QueueView state={makeState(makeSnapshot({ pause, paused: true, runs: [makeRun()] }))} />);
    expect(screen.getByText(/23:4\d/)).toBeTruthy();
    expect(screen.getByText(/waiting on the API review/)).toBeTruthy();
    expect(screen.getByText(/1 run waiting/)).toBeTruthy();
  });

  it("offers extend, clear-timer and resume while paused", async () => {
    const pause = { ...idlePause, paused: true, resumeAt: new Date(Date.now() + 600_000).toISOString() };
    render(<QueueView state={makeState(makeSnapshot({ pause, paused: true }))} />);
    // Each action disables the band while its request is in flight, so they
    // are exercised one at a time — exactly as a user would.
    fireEvent.click(screen.getByRole("button", { name: "+15m" }));
    await waitFor(() => expect(rpcCalls("extendPause")[0][0].params.deltaMs).toBe(15 * 60_000));
    fireEvent.click(screen.getByRole("button", { name: "+1h" }));
    await waitFor(() => expect(rpcCalls("extendPause")[1][0].params.deltaMs).toBe(60 * 60_000));
    fireEvent.click(screen.getByRole("button", { name: "Clear timer" }));
    await waitFor(() => expect(rpcCalls("clearPauseTimer")).toHaveLength(1));
    fireEvent.click(screen.getByRole("button", { name: "Resume now" }));
    await waitFor(() => expect(rpcCalls("resumeQueue")).toHaveLength(1));
  });

  it("says 'until you resume' when there is no timer", () => {
    const pause = { ...idlePause, paused: true };
    render(<QueueView state={makeState(makeSnapshot({ pause, paused: true }))} />);
    // Once visibly, once in the polite live region announced to screen readers.
    expect(screen.getAllByText(/until you resume/i).length).toBe(2);
    expect(screen.queryByRole("button", { name: "+15m" })).toBeNull();
  });

  it("explains that an external hold resumes automatically", () => {
    const pause = { ...idlePause, paused: true, source: "external-agent" as const };
    render(<QueueView state={makeState(makeSnapshot({ pause, paused: true, runs: [makeRun()] }))} />);
    expect(screen.getAllByText(/resumes automatically when the external run ends/i)).toHaveLength(2);
    expect(screen.queryByText(/until you resume/i)).toBeNull();
    expect(screen.queryByRole("button", { name: "Resume now" })).toBeNull();
  });

  // §5.4 — a system hold explains itself rather than leaving the user guessing.
  it("explains a system hold in one plain sentence", () => {
    const pause = {
      ...idlePause,
      paused: true,
      source: "quota" as const,
      reason: "quota resets at 15:15",
      resumeAt: new Date(Date.now() + 3600_000).toISOString(),
    };
    render(<QueueView state={makeState(makeSnapshot({ pause, paused: true }))} />);
    expect(screen.getAllByText(/out of quota/i).length).toBeGreaterThan(0);
    expect(screen.getByText(/quota resets at 15:15/)).toBeTruthy();
  });

  // §5.6 — the queue itself shows the consequence.
  it("dims the queue and renames the first row while paused", () => {
    const pause = { ...idlePause, paused: true };
    const runs = [makeRun({ id: "a" }), makeRun({ id: "b" })];
    const { container } = render(
      <QueueView state={makeState(makeSnapshot({ pause, paused: true, runs }))} />
    );
    expect(screen.getByText(/starts when the queue resumes/i)).toBeTruthy();
    expect(container.querySelectorAll(".ac-qrow.dimmed").length).toBe(2);
  });

  it("reads 'next' for the leading row when running normally", () => {
    const runs = [makeRun({ id: "a" }), makeRun({ id: "b" })];
    render(<QueueView state={makeState(makeSnapshot({ runs }))} />);
    expect(screen.getByText("next")).toBeTruthy();
  });

  it("hides the pause button while the band is showing", () => {
    const pause = { ...idlePause, paused: true };
    render(<QueueView state={makeState(makeSnapshot({ pause, paused: true }))} />);
    expect(screen.queryByRole("button", { name: /^pause$/i })).toBeNull();
  });
});

describe("queue reordering", () => {
  it.each([
    ["Move b up", "b", "a"],
    ["Move b down", "b", ""],
    ["Move a down", "a", "c"],
    ["Move a to the end of the queue", "a", ""],
  ])("supports the visible %s control", async (label, runId, beforeRunId) => {
    const runs = ["a", "b", "c"].map((id, i) => makeRun({ id, title: id, sortKey: (i + 1) * 1024 }));
    render(<QueueView state={makeState(makeSnapshot({ runs: runs.reverse() }))} />);
    fireEvent.click(screen.getByRole("button", { name: label }));
    await waitFor(() => expect(rpcCalls("reorderQueue")[0][0].params).toEqual({ runId, beforeRunId }));
    await waitFor(() => expect(postMessage).toHaveBeenCalledWith({ type: "requestSnapshot" }));
  });

  it("disables boundary moves and shows rejected moves", async () => {
    const runs = ["a", "b"].map(id => makeRun({ id, title: id }));
    render(<QueueView state={makeState(makeSnapshot({ runs }))} />);
    expect((screen.getByRole("button", { name: "Move a up" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Move b down" }) as HTMLButtonElement).disabled).toBe(true);
    postMessage.mockImplementation(msg => {
      if (msg.method === "reorderQueue") setTimeout(() => window.dispatchEvent(new MessageEvent("message", {
        data: { type: "rpcError", requestId: msg.requestId, error: "cannot move a run across its batch row dependency" },
      })), 0);
    });
    fireEvent.click(screen.getByRole("button", { name: "Move b up" }));
    expect((await screen.findByRole("alert")).textContent).toContain("batch row dependency");
  });

  it("allows dragging a run to the end of the stack", async () => {
    const runs = ["a", "b"].map(id => makeRun({ id, title: id }));
    render(<QueueView state={makeState(makeSnapshot({ runs }))} />);
    fireEvent.drop(screen.getByText("End of queue"), { dataTransfer: { getData: () => "a" } });
    await waitFor(() => expect(rpcCalls("reorderQueue")[0][0].params).toEqual({ runId: "a", beforeRunId: "" }));
  });

  // §7.5 — reorderQueue existed in the RPC surface with no UI at all.
  it("moves a run to the front with the row action", async () => {
    const runs = [makeRun({ id: "a" }), makeRun({ id: "b" }), makeRun({ id: "c" })];
    render(<QueueView state={makeState(makeSnapshot({ runs }))} />);
    const buttons = screen.getAllByRole("button", { name: /move .* to the front/i });
    fireEvent.click(buttons[2]);
    await waitFor(() => {
      const call = rpcCalls("reorderQueue")[0];
      expect(call[0].params.runId).toBe("c");
      expect(call[0].params.beforeRunId).toBe("a");
    });
  });

  it("supports Alt+ArrowUp as a keyboard alternative to dragging", async () => {
    const runs = [makeRun({ id: "a" }), makeRun({ id: "b" })];
    const { container } = render(<QueueView state={makeState(makeSnapshot({ runs }))} />);
    const rows = container.querySelectorAll(".ac-qrow");
    fireEvent.keyDown(rows[1], { key: "ArrowUp", altKey: true });
    await waitFor(() => {
      const call = rpcCalls("reorderQueue")[0];
      expect(call[0].params.runId).toBe("b");
      expect(call[0].params.beforeRunId).toBe("a");
    });
  });
});

describe("destructive actions", () => {
  // §6.2 — window.confirm is a no-op in a sandboxed webview, so Skip either
  // did nothing or silently went ahead.
  it("routes confirmation through the host before skipping a run", async () => {
    const run = makeRun({ state: "NEEDS_ATTENTION", errorMsg: "stopped without a marker" });
    render(<QueueView state={makeState(makeSnapshot({ runs: [run] }))} />);
    fireEvent.click(screen.getByRole("button", { name: "Skip" }));
    await waitFor(() => {
      expect(postMessage.mock.calls.some((c: any[]) => c[0]?.type === "confirm")).toBe(true);
      expect(rpcCalls("skipRun")).toHaveLength(1);
    });
  });
});

// §7.3 — most real usage is the same shape repeated, so the shape is nameable.
describe("saved setups", () => {
  it("offers to save the current composer configuration", () => {
    render(<QueueView state={makeState(makeSnapshot())} />);
    expect(screen.getByRole("button", { name: /save setup/i })).toBeTruthy();
  });

  it("round-trips a preset and applies it back to the composer", async () => {
    const { unmount } = render(<QueueView state={makeState(makeSnapshot())} />);
    fireEvent.change(screen.getByLabelText("Permission"), { target: { value: "plan" } });
    fireEvent.change(screen.getByLabelText("Effort"), { target: { value: "low" } });
    fireEvent.click(screen.getByRole("button", { name: /save setup/i }));
    fireEvent.change(screen.getByLabelText(/name this setup/i), { target: { value: "read only" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(screen.getByRole("button", { name: "read only" })).toBeTruthy();
    unmount();

    render(<QueueView state={makeState(makeSnapshot())} />);
    // Change away from the saved values, then apply the preset.
    fireEvent.change(screen.getByLabelText("Permission"), { target: { value: "bypass" } });
    fireEvent.click(screen.getByRole("button", { name: "read only" }));
    await waitFor(() => {
      expect((screen.getByLabelText("Permission") as HTMLSelectElement).value).toBe("plan");
      expect((screen.getByLabelText("Effort") as HTMLSelectElement).value).toBe("low");
    });
  });

  it("forgets a preset on request", () => {
    render(<QueueView state={makeState(makeSnapshot())} />);
    fireEvent.click(screen.getByRole("button", { name: /save setup/i }));
    fireEvent.change(screen.getByLabelText(/name this setup/i), { target: { value: "temp" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    fireEvent.click(screen.getByRole("button", { name: /forget the .temp. setup/i }));
    expect(screen.queryByRole("button", { name: "temp" })).toBeNull();
  });

  // The composer reads the selected provider's capability record. A working,
  // signed-in Codex install used to render "runtime missing" here, because the
  // daemon could not find a CLI that shipped inside the ChatGPT extension
  // rather than on PATH.
  it("shows the selected provider's version, per provider", async () => {
    render(<QueueView state={makeState(makeSnapshot())} />);
    expect(screen.getByText("2.1.278")).toBeTruthy();
    expect(screen.queryByText(/runtime missing/i)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Codex" }));
    await waitFor(() => {
      expect(screen.getByText("1.0")).toBeTruthy();
      expect(screen.queryByText(/runtime missing/i)).toBeNull();
    });
  });

  it("says the runtime is missing only when it really is", async () => {
    const snap = makeSnapshot();
    snap.providers.codex = { ...snap.providers.codex, installed: false, version: "", models: [] };
    render(<QueueView state={makeState(snap)} />);

    // Claude is still installed, so nothing is claimed about it.
    expect(screen.queryByText(/runtime missing/i)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Codex" }));
    await waitFor(() => expect(screen.getByText(/runtime missing/i)).toBeTruthy());
  });
});

describe("explicit model and effort selection", () => {
  function modelSnapshot() {
    const snap = makeSnapshot();
    snap.providers.claude.models = [
      { id: "claude-opus-5-5", name: "Claude Opus 5.5", efforts: ["low", "medium", "high", "xhigh", "max"] },
      { id: "claude-haiku-4-5-20251001", name: "Claude Haiku 4.5", efforts: [] },
    ];
    return snap;
  }
  it("pins Opus 5.5 and sends every advertised effort unchanged", async () => {
    render(<QueueView state={makeState(modelSnapshot())} />);
    const model = screen.getByLabelText("Model") as HTMLSelectElement;
    expect(model.value).toBe("claude-opus-5-5");
    const effort = screen.getByLabelText("Effort") as HTMLSelectElement;
    expect(Array.from(effort.options).map((o) => o.value)).toEqual(["", "low", "medium", "high", "xhigh", "max"]);
    for (const level of ["low", "medium", "high", "xhigh", "max"]) {
      fireEvent.change(effort, { target: { value: level } });
      fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: `task ${level}` } });
      fireEvent.click(screen.getByRole("button", { name: "Queue run", exact: true }));
      await waitFor(() => expect(rpcCalls("enqueueRun").at(-1)?.[0].params).toMatchObject({ model: "claude-opus-5-5", effort: level }));
      await waitFor(() => expect((screen.getByLabelText("Prompt") as HTMLTextAreaElement).value).toBe(""));
    }
  });
  it("clears unsupported effort on a model change", async () => {
    render(<QueueView state={makeState(modelSnapshot())} />);
    fireEvent.change(screen.getByLabelText("Effort"), { target: { value: "max" } });
    fireEvent.change(screen.getByLabelText("Model"), { target: { value: "claude-haiku-4-5-20251001" } });
    await waitFor(() => expect((screen.getByLabelText("Effort") as HTMLSelectElement).value).toBe(""));
    fireEvent.change(screen.getByLabelText("Prompt"), { target: { value: "small task" } });
    fireEvent.click(screen.getByRole("button", { name: "Queue run", exact: true }));
    await waitFor(() => expect(rpcCalls("enqueueRun")[0][0].params.effort).toBe(""));
  });
  it("shows the actual provider model beside the requested effort", () => {
    const run = makeRun({ state: "RUNNING", model: "opus", resolvedModel: "claude-opus-5-5", effort: "xhigh" });
    render(<QueueView state={makeState(makeSnapshot({ runs: [run] }))} />);
    expect(screen.getByTitle("Requested: opus").textContent).toBe("claude-opus-5-5");
    expect(screen.getByText("Effort: xhigh")).toBeTruthy();
  });
});
