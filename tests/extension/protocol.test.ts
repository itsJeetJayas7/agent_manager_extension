import { describe, it, expect } from "vitest";

// Pure-logic tests that don't require a VS Code instance. extension.ts and
// friends are exercised through the Extension Development Host manually /
// via @vscode/test-electron when available.

// Mirror of the CSP/nonce logic in extension.ts
export function buildCsp(nonce: string): string {
  return [
    "default-src 'none'",
    `script-src 'nonce-${nonce}'`,
    "style-src 'unsafe-inline'",
    "font-src 'none'",
  ].join("; ");
}

describe("webview CSP", () => {
  it("denies everything by default and scripts only by nonce", () => {
    const csp = buildCsp("abc123");
    expect(csp).toContain("default-src 'none'");
    expect(csp).toContain("script-src 'nonce-abc123'");
    expect(csp).not.toContain("https://"); // no remote origins, no CDN
  });
});

describe("protocol constants", () => {
  it("uses protocol version 1", async () => {
    const mod = await import("../../src/extension/daemonClient");
    expect(mod.PROTOCOL_VERSION).toBe(1);
  });
});

describe("ndjson framing", () => {
  it("parses concatenated responses", () => {
    // simulate the client-side line splitting
    const chunk = `{"id":1,"result":{"ok":true}}\n{"id":2,"error":{"code":"X","message":"bad"}}\n`;
    let buffer = "";
    const messages: any[] = [];
    buffer += chunk;
    let idx: number;
    while ((idx = buffer.indexOf("\n")) >= 0) {
      const line = buffer.slice(0, idx);
      buffer = buffer.slice(idx + 1);
      if (line.trim()) {
        messages.push(JSON.parse(line));
      }
    }
    expect(messages).toHaveLength(2);
    expect(messages[1].error.code).toBe("X");
  });
});
