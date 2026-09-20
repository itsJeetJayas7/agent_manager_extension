import { describe, it, expect } from "vitest";
import { webviewHtml, buildCsp, createNonce } from "../../src/extension/webviewHtml";

const opts = {
  scriptUri: "https://file+.vscode-resource.vscode-cdn.net/ext/dist/webview/webview.js",
  cspSource: "https://file+.vscode-resource.vscode-cdn.net",
  mode: "sidebar",
};

describe("webview bootstrap", () => {
  // §1.1 — `acquireVscodeApi` (lower-case C) threw a ReferenceError before
  // React mounted, leaving a blank sidebar that never talked to anything.
  it("calls acquireVsCodeApi with the correct capitalisation", () => {
    const html = webviewHtml(opts);
    expect(html).toContain("acquireVsCodeApi(");
    expect(html).not.toContain("acquireVscodeApi");
  });

  it("only acquires the API once per document", () => {
    const html = webviewHtml(opts);
    // acquireVsCodeApi() throws on a second call in the same webview document.
    expect(html).toContain("window.__acVscode = window.__acVscode || acquireVsCodeApi();");
  });

  it("loads the script bundle it was given", () => {
    expect(webviewHtml(opts)).toContain(`src="${opts.scriptUri}"`);
  });

  // §1.2 — the design system is bundled into webview.js, so there must be no
  // dangling <link> to a stylesheet the host does not serve.
  it("references no external stylesheet", () => {
    const html = webviewHtml(opts);
    expect(html).not.toContain("<link");
    expect(html).not.toContain(".css");
  });

  it("carries the mode on the body so the bundle can branch on it", () => {
    expect(webviewHtml({ ...opts, mode: "planner" })).toContain('data-mode="planner"');
  });

  it("uses the same nonce for both scripts", () => {
    const html = webviewHtml({ ...opts, nonce: "TESTNONCE" });
    const tags = html.match(/nonce="TESTNONCE"/g) ?? [];
    expect(tags).toHaveLength(2);
    expect(html).toContain("'nonce-TESTNONCE'");
  });
});

describe("nonce", () => {
  // §1.4 — Math.random().toString(36)[2] yields undefined for digit-only
  // chunks and is not crypto-random.
  it("is crypto-random and never contains 'undefined'", () => {
    const seen = new Set<string>();
    for (let i = 0; i < 200; i++) {
      const n = createNonce();
      expect(n).not.toContain("undefined");
      expect(n.length).toBeGreaterThanOrEqual(24);
      seen.add(n);
    }
    expect(seen.size).toBe(200);
  });
});

describe("content security policy", () => {
  it("denies everything by default and scripts only by nonce", () => {
    const csp = buildCsp("abc123", "vscode-resource:");
    expect(csp).toContain("default-src 'none'");
    expect(csp).toContain("script-src 'nonce-abc123'");
    expect(csp).not.toContain("https://");
  });

  it("permits images explicitly rather than relying on default-src", () => {
    expect(buildCsp("n", "vscode-resource:")).toContain("img-src vscode-resource: data:");
  });

  it("never allows inline or remote scripts", () => {
    const csp = buildCsp("n", "vscode-resource:");
    expect(csp).not.toContain("script-src 'unsafe-inline'");
    expect(csp).not.toContain("unsafe-eval");
  });
});
