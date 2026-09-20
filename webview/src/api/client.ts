// Thin messaging layer: webview -> extension host only. The webview never
// touches daemon sockets.
declare global {
  interface Window {
    __acVscode?: { postMessage(msg: unknown): void; getState(): any; setState(s: any): void };
    __acCodicon?: string;
  }
}

export const vscodeApi = {
  postMessage: (msg: unknown) => window.__acVscode?.postMessage(msg),
  getState: () => window.__acVscode?.getState() ?? ({}),
  setState: (s: unknown) => window.__acVscode?.setState(s),
};

let nextRequestId = 1;
const pending = new Map<number, { resolve: (v: any) => void; reject: (e: Error) => void }>();

window.addEventListener("message", (ev) => {
  const m = ev.data;
  if (!m || typeof m !== "object") {
    return;
  }
  if (m.type === "rpcResult" && pending.has(m.requestId)) {
    pending.get(m.requestId)!.resolve(m.result);
    pending.delete(m.requestId);
  } else if (m.type === "rpcError" && pending.has(m.requestId)) {
    pending.get(m.requestId)!.reject(new Error(m.error));
    pending.delete(m.requestId);
  }
});

export function rpc<T = any>(method: string, params?: any): Promise<T> {
  const requestId = nextRequestId++;
  return new Promise<T>((resolve, reject) => {
    pending.set(requestId, { resolve, reject });
    vscodeApi.postMessage({ type: "rpc", requestId, method, params: params ?? {} });
    setTimeout(() => {
      if (pending.has(requestId)) {
        pending.delete(requestId);
        reject(new Error(`request timeout: ${method}`));
      }
    }, 30000);
  });
}

export function openClaudeSession(sessionId: string): void {
  vscodeApi.postMessage({ type: "openClaude", sessionId });
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
