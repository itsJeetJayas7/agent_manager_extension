// @vitest-environment jsdom
import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import React from "react";
import StatusGlyph from "../../webview/src/components/StatusGlyph";
import { fmtDuration, fmtRelative } from "../../webview/src/components/format";
import { EffectivePromptMock } from "./fixtures";

// vitest runs with globals disabled, so React Testing Library's automatic
// cleanup does not fire; unmount explicitly between tests.
afterEach(cleanup);

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
    // A hair over a minute, so the assertion does not race the clock.
    const future = new Date(Date.now() + 61_000).toISOString();
    expect(fmtRelative(future)).toMatch(/^in 1m \d+s$/);
    const past = new Date(Date.now() - 125_000).toISOString();
    expect(fmtRelative(past)).toMatch(/^2m \d+s ago$/);
    expect(fmtRelative(undefined)).toBe("—");
    expect(fmtRelative("not a date")).toBe("—");
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
