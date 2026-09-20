// Webview document generation, kept pure so it can be tested without a running
// VS Code. Two shipped bugs lived in here: `acquireVscodeApi` (lower-case C,
// which threw before React ever mounted) and a stylesheet that was built but
// never referenced. Both are covered by tests now.
import * as crypto from "crypto";

export interface WebviewHtmlOptions {
  /** Webview URI of the bundle, already passed through asWebviewUri. */
  scriptUri: string;
  /** `webview.cspSource`, used for img-src. */
  cspSource: string;
  /** sidebar | planner | detail — read by the bundle from document.body. */
  mode: string;
  /** Injected for tests; a crypto-random value is generated otherwise. */
  nonce?: string;
}

/** A real nonce: crypto-random, never Math.random().toString(36)[2]. */
export function createNonce(): string {
  return crypto.randomBytes(24).toString("base64");
}

export function buildCsp(nonce: string, cspSource: string): string {
  return [
    "default-src 'none'",
    `script-src 'nonce-${nonce}'`,
    // VS Code injects its own `_defaultStyles` <style> element into every
    // webview document, and dynamic widths (progress rules, countdown bars)
    // are set through style attributes, so inline styles must remain allowed.
    // CSP3 ignores 'unsafe-inline' whenever a nonce or hash is also present,
    // so a nonce cannot be added here without breaking VS Code's own theming.
    // The design system itself ships inside webview.js — there is no external
    // stylesheet to permit.
    "style-src 'unsafe-inline'",
    `img-src ${cspSource} data:`,
    "font-src 'none'",
  ].join("; ");
}

export function webviewHtml(opts: WebviewHtmlOptions): string {
  const nonce = opts.nonce ?? createNonce();
  const csp = buildCsp(nonce, opts.cspSource);
  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta http-equiv="Content-Security-Policy" content="${csp}">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
</head>
<body data-mode="${opts.mode}">
<div id="root"></div>
<script nonce="${nonce}">window.__acVscode = window.__acVscode || acquireVsCodeApi();</script>
<script nonce="${nonce}" src="${opts.scriptUri}"></script>
</body>
</html>`;
}
