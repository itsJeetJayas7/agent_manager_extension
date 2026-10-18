import type { ExternalSessionLike } from "../../../src/extension/protocol";

// Keep aligned with the scheduler's busyEvents and 45-minute stale cutoff.
// Opening a session is not a turn; tool activity continues an existing turn.
const ACTIVE_EVENTS = new Set(["UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure"]);
const STALE_AFTER_MS = 45 * 60_000;

export function isExternalSessionBusy(session: ExternalSessionLike, now: number): boolean {
  return session.kind === "external"
    && ACTIVE_EVENTS.has(session.lastEvent)
    && now - Date.parse(session.lastSeenAt) <= STALE_AFTER_MS;
}
