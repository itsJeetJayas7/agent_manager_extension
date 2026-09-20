import { describe, it, expect } from "vitest";

// Wire-protocol tests. The CSP and webview bootstrap are covered against the
// real implementation in webviewHtml.test.ts rather than a copy of it.

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
