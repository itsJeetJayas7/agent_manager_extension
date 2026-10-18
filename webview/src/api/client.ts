// Thin messaging layer: webview -> extension host only. The webview never
// touches daemon sockets.
declare global {
  interface Window {
    __acVscode?: { postMessage(msg: unknown): void; getState(): any; setState(s: any): void };
  }
}

export const vscodeApi = {
  postMessage: (msg: unknown) => window.__acVscode?.postMessage(msg),
  getState: () => window.__acVscode?.getState() ?? {},
  setState: (s: unknown) => window.__acVscode?.setState(s),
};

let nextRequestId = 1;
type Pending = { resolve: (v: any) => void; reject: (e: Error) => void; timer: number };
const pending = new Map<number, Pending>();

function settle(requestId: number): Pending | undefined {
  const p = pending.get(requestId);
  if (!p) {
    return undefined;
  }
  clearTimeout(p.timer);
  pending.delete(requestId);
  return p;
}

window.addEventListener("message", (ev) => {
  const m = ev.data;
  if (!m || typeof m !== "object") {
    return;
  }
  if (m.type === "rpcResult") {
    settle(m.requestId)?.resolve(m.result);
  } else if (m.type === "rpcError") {
    settle(m.requestId)?.reject(new Error(m.error));
  } else if (m.type === "confirmResult") {
    settle(m.requestId)?.resolve(m.confirmed);
  }
});

export function rpc<T = any>(method: string, params?: any, timeoutMs = 15000): Promise<T> {
  const requestId = nextRequestId++;
  return new Promise<T>((resolve, reject) => {
    const timer = window.setTimeout(() => {
      if (pending.delete(requestId)) {
        reject(new Error(`request timeout: ${method}`));
      }
    }, timeoutMs);
    pending.set(requestId, { resolve, reject, timer });
    vscodeApi.postMessage({ type: "rpc", requestId, method, params: params ?? {} });
  });
}

/**
 * Modal confirmation routed through the host.
 * `window.confirm` is a no-op inside a sandboxed VS Code webview, so the Skip
 * and Apply Batch confirmations silently did nothing (AGENT_BRIEF §6.2).
 */
export function confirm(
  message: string,
  opts: { detail?: string; confirmLabel?: string } = {}
): Promise<boolean> {
  const requestId = nextRequestId++;
  return new Promise<boolean>((resolve, reject) => {
    const timer = window.setTimeout(() => {
      if (pending.delete(requestId)) {
        resolve(false); // dialog dismissed or lost: treat as "no"
      }
    }, 120000);
    pending.set(requestId, { resolve, reject, timer });
    vscodeApi.postMessage({
      type: "confirm",
      requestId,
      message,
      detail: opts.detail,
      confirmLabel: opts.confirmLabel ?? "Confirm",
    });
  });
}

/** Host-side toast. `window.alert` does not work in a webview either. */
export function notify(message: string, level: "info" | "warning" | "error" = "info"): void {
  vscodeApi.postMessage({ type: "notify", message, level });
}

export function announceReady(): void {
  vscodeApi.postMessage({ type: "ready" });
}

export function openClaudeSession(sessionId: string, cwd: string): void {
  vscodeApi.postMessage({ type: "openClaude", sessionId, cwd });
}
export function openCodexThread(threadId: string, title: string): void {
  vscodeApi.postMessage({ type: "openCodex", threadId, title });
}
export function openWorktree(path: string): void {
  vscodeApi.postMessage({ type: "openWorktree", path });
}
export function openDetail(batchId: string): void {
  vscodeApi.postMessage({ type: "openDetail", batchId });
}
export function openPlanner(): void {
  vscodeApi.postMessage({ type: "openPlanner" });
}
export function copyText(text: string, warnSecrets = false): void {
  vscodeApi.postMessage({ type: "copy", text, warnSecrets });
}
export function revealPrompt(runId: string): void {
  vscodeApi.postMessage({ type: "revealPrompt", runId });
}
export function requestSnapshot(): void {
  vscodeApi.postMessage({ type: "requestSnapshot" });
}
export function requestHostInfo(): void {
  vscodeApi.postMessage({ type: "hostInfo" });
}
