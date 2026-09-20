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
