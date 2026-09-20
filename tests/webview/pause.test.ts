// @vitest-environment jsdom
import { describe, it, expect } from "vitest";
import { formatCountdown, describeRemaining } from "../../webview/src/components/ticker";
import { parseDuration } from "../../webview/src/components/PauseControl";

describe("countdown formatting", () => {
  it("uses mm:ss under an hour and h:mm:ss above", () => {
    expect(formatCountdown(0)).toBe("00:00");
    expect(formatCountdown(41_000)).toBe("00:41");
    expect(formatCountdown(23 * 60_000 + 41_000)).toBe("23:41");
    expect(formatCountdown(3 * 3_600_000 + 4 * 60_000 + 9_000)).toBe("3:04:09");
  });

  it("never goes negative when the deadline has passed", () => {
    expect(formatCountdown(-5000)).toBe("00:00");
  });

  it("keeps a fixed width as digits change", () => {
    expect(formatCountdown(9_000)).toHaveLength(formatCountdown(59_000).length);
  });

  it("describes remaining time in words for screen readers", () => {
    expect(describeRemaining(30_000)).toBe("30 seconds");
    expect(describeRemaining(60_000)).toBe("1 minute");
    expect(describeRemaining(45 * 60_000)).toBe("45 minutes");
    expect(describeRemaining(2 * 3_600_000)).toBe("2 hours");
    expect(describeRemaining(90 * 60_000)).toBe("1h 30m");
  });
});

describe("custom pause durations", () => {
  it("accepts the shapes a person actually types", () => {
    expect(parseDuration("45m")).toBe(45 * 60_000);
    expect(parseDuration("45")).toBe(45 * 60_000);
    expect(parseDuration("3h")).toBe(3 * 3_600_000);
    expect(parseDuration("1h30m")).toBe(90 * 60_000);
    expect(parseDuration("2h 15m")).toBe(135 * 60_000);
    expect(parseDuration("90 minutes")).toBe(90 * 60_000);
  });

  it("rejects what it does not understand rather than guessing", () => {
    expect(parseDuration("")).toBeNull();
    expect(parseDuration("soon")).toBeNull();
    expect(parseDuration("3 bananas")).toBeNull();
    expect(parseDuration("-5m")).toBeNull(); // a negative pause is not a pause
    expect(parseDuration("0m")).toBeNull();
  });
});
