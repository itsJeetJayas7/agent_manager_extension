import { describe, it, expect } from "vitest";

describe("session openers (URI construction)", () => {
  it("builds claude session URIs safely", () => {
    const sessionId = "550e8400-e29b-41d4-a716-446655440000";
    const uri = `vscode://anthropic.claude-code/open?session=${encodeURIComponent(sessionId)}`;
    expect(uri).toBe("vscode://anthropic.claude-code/open?session=550e8400-e29b-41d4-a716-446655440000");
  });
  it("encodes hostile session ids", () => {
    const evil = 'abc"&timeout=0';
    const uri = `vscode://anthropic.claude-code/open?session=${encodeURIComponent(evil)}`;
    expect(uri).not.toContain('"');
    expect(uri).not.toContain("&timeout");
  });
});
