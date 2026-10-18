import React from "react";
import { beforeEach, afterEach, describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";
import QueueView from "../../webview/src/views/QueueView";
import { makeSnapshot, makeState } from "./snapshot";
import type { PromptSequencePreview } from "../../src/extension/protocol";

const postMessage = vi.fn();
let preview: PromptSequencePreview;
let pickCancelled: boolean;
let importError: string;
beforeEach(() => {
  preview = { id: "review-token", title: "release", folder: "/prompts/release", errors: [], ignored: ["README.md"], totalBytes: 32,
    files: [{ name: "001-build.md", order: 1, bytes: 16, prompt: "Build this.\n<script>alert(1)</script>" }, { name: "002-test.md", order: 2, bytes: 16, prompt: "Verify the build." }] };
  pickCancelled = false;
  importError = "";
  postMessage.mockReset();
  window.__acVscode = { postMessage, getState: () => ({}), setState: () => {} };
  postMessage.mockImplementation((m: any) => {
    if (m.type !== "rpc") return;
    const result = m.method === "promptSequence.pick" ? (pickCancelled ? null : preview)
      : m.method === "promptSequence.refresh" ? { ...preview, id: "refreshed" }
      : m.method === "promptSequence.pickWorkspace" ? "/actual/workspace"
      : { batchId: "batch-123" };
    setTimeout(() => window.dispatchEvent(new MessageEvent("message", { data: importError && m.method === "promptSequence.import"
      ? { type: "rpcError", requestId: m.requestId, error: importError }
      : { type: "rpcResult", requestId: m.requestId, result } })), 0);
  });
});
afterEach(cleanup);
const calls = (method: string) => postMessage.mock.calls.map(([m]) => m).filter(m => m.method === method);
async function open() {
  render(<QueueView state={makeState(makeSnapshot())} />);
  fireEvent.click(screen.getByRole("button", { name: "Import prompts from folder" }));
  await screen.findByText("2 steps in execution order");
}

describe("prompt folder import", () => {
  it("previews without queuing, keeps text inert and hides the single-run actions", async () => {
    await open();
    expect(calls("promptSequence.import")).toHaveLength(0);
    expect(screen.queryByRole("button", { name: "Queue run", exact: true })).toBeNull();
    fireEvent.click(screen.getByText("build", { exact: true }));
    expect(screen.getByLabelText("Prompt 001-build.md").textContent).toContain("<script>alert(1)</script>");
    expect(document.querySelector(".ac-sequence script")).toBeNull();
    expect(screen.getByText("/prompts/release")).toBeTruthy();
  });
  it("uses the reviewed token, chosen working folder and current composer settings", async () => {
    await open();
    fireEvent.click(screen.getByRole("button", { name: "Codex", exact: true }));
    fireEvent.click(screen.getByRole("button", { name: "Change working folder" }));
    await screen.findByText("/actual/workspace");
    fireEvent.change(screen.getByLabelText("Sequence title"), { target: { value: "My release" } });
    const queue = screen.getByRole("button", { name: "Queue 2 prompts in order" });
    fireEvent.click(queue);
    fireEvent.click(queue);
    await screen.findByText(/2 prompts queued in order/);
    expect(calls("promptSequence.import")).toHaveLength(1);
    expect(calls("promptSequence.import")[0].params).toMatchObject({ id: "review-token", title: "My release", cwd: "/actual/workspace", settings: { provider: "codex", permission: "bypass" } });
    fireEvent.click(screen.getByRole("button", { name: "View sequence" }));
    expect(postMessage).toHaveBeenCalledWith({ type: "openDetail", batchId: "batch-123" });
  });
  it("blocks invalid sequences and lets users refresh after fixing the files", async () => {
    preview.errors = ["002-test.md: duplicate step number 2."];
    await open();
    expect(screen.getByRole("alert").textContent).toContain("duplicate step");
    expect((screen.getByRole("button", { name: "Queue 2 prompts in order" }) as HTMLButtonElement).disabled).toBe(true);
    preview = { ...preview, errors: [] };
    fireEvent.click(screen.getByRole("button", { name: "Refresh preview" }));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
    fireEvent.click(screen.getByRole("button", { name: "Queue 2 prompts in order" }));
    await waitFor(() => expect(calls("promptSequence.import")[0]?.params.id).toBe("refreshed"));
  });
  it("handles cancellation without an error or enqueue", async () => {
    pickCancelled = true;
    render(<QueueView state={makeState(makeSnapshot())} />);
    fireEvent.click(screen.getByRole("button", { name: "Import prompts from folder" }));
    await waitFor(() => expect(calls("promptSequence.pick")).toHaveLength(1));
    await waitFor(() => expect(screen.queryByText("Working…")).toBeNull());
    expect(screen.queryByRole("alert")).toBeNull();
    expect(calls("promptSequence.import")).toHaveLength(0);
  });
  it("returns to the single prompt composer from the footer without discarding the preview", async () => {
    await open();
    fireEvent.click(screen.getByRole("button", { name: "Queue a run" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Queue run", exact: true })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Import prompts from folder" }));
    expect(screen.getByText("2 steps in execution order")).toBeTruthy();
    expect(calls("promptSequence.pick")).toHaveLength(1);
  });
  it("shows import failures and retries with the same token", async () => {
    await open();
    importError = "Daemon disconnected. Retry to recover the original import.";
    fireEvent.click(screen.getByRole("button", { name: "Queue 2 prompts in order" }));
    await screen.findByRole("alert");
    importError = "";
    fireEvent.click(screen.getByRole("button", { name: "Queue 2 prompts in order" }));
    await screen.findByText(/2 prompts queued in order/);
    expect(calls("promptSequence.import").map(m => m.params.id)).toEqual(["review-token", "review-token"]);
  });
  it("opens from the command palette signal and copies the generator template", async () => {
    render(<QueueView state={{ ...makeState(makeSnapshot()), showPromptImport: true }} />);
    await screen.findByText("2 steps in execution order");
    fireEvent.click(screen.getByRole("button", { name: "Copy generator prompt" }));
    const copied = postMessage.mock.calls.map(([m]) => m).find(m => m.type === "copy");
    expect(copied.text).toContain("001-short-title.md");
    expect(copied.text).toContain("fresh agent conversation");
    expect(copied.text).toContain("<AC_RESULT>COMPLETE</AC_RESULT>");
  });
  it("holds settings after an uncertain submission until its original status is recovered", async () => {
    await open();
    importError = "Import status could not be confirmed: disconnected. Retry this preview.";
    fireEvent.click(screen.getByRole("button", { name: "Queue 2 prompts in order" }));
    await screen.findByRole("button", { name: "Recover import status" });
    expect((screen.getByRole("button", { name: "Change prompt folder" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Refresh preview" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByLabelText("Sequence title") as HTMLInputElement).disabled).toBe(true);
    importError = "";
    fireEvent.click(screen.getByRole("button", { name: "Recover import status" }));
    await screen.findByText(/2 prompts queued in order/);
    expect(calls("promptSequence.import")[1].params).toEqual(calls("promptSequence.import")[0].params);
  });
});
