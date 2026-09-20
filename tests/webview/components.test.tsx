// @vitest-environment jsdom
import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import React from "react";
import StatusGlyph from "../../webview/src/components/StatusGlyph";
import { fmtDuration, fmtRelative } from "../../webview/src/components/format";
import { EffectivePromptMock } from "./fixtures";

describe("StatusGlyph", () => {
  it("renders non-color status labels", () => {
    render(<StatusGlyph state="RUNNING" />);
    expect(screen.getByText("Running")).toBeTruthy();
  });
  it("exposes machine state for screen readers", () => {
    render(<StatusGlyph state="WAITING_FOR_QUOTA" />);
    expect(screen.getByText(/\(WAITING_FOR_QUOTA\)/)).toBeTruthy();
  });
});

describe("format helpers", () => {
  it("formats durations", () => {
    expect(fmtDuration(0)).toBe("—");
    expect(fmtDuration(45_000)).toBe("45s");
    expect(fmtDuration(3 * 60_000 + 42_000)).toBe("3m 42s");
    expect(fmtDuration(2 * 3_600_000 + 41 * 60_000)).toBe("2h 41m");
  });
  it("formats relative times", () => {
    const future = new Date(Date.now() + 60_000).toISOString();
    expect(fmtRelative(future)).toBe("in 1m 0s");
    expect(fmtRelative(undefined)).toBe("—");
  });
});

describe("prompt assembly contract (mirrors daemon)", () => {
  it("places contract, task, and secrets in order", () => {
    const p = EffectivePromptMock("TASK A", true, "SECRETPW", false);
    expect(p.indexOf("[AGENT CONDUCTOR EXECUTION CONTRACT]")).toBeLessThan(p.indexOf("TASK A"));
    expect(p.indexOf("TASK A")).toBeLessThan(p.indexOf("SECRETPW"));
    expect(p).toContain("[SHARED TESTING / SUDO BLOCK — USER SUPPLIED]");
  });
  it("omits secrets in plan phase and when disabled", () => {
    expect(EffectivePromptMock("T", true, "SECRETPW", true)).not.toContain("SECRETPW");
    expect(EffectivePromptMock("T", false, "SECRETPW", false)).not.toContain("SECRETPW");
  });
});
