// Status glyphs: never color-only — each carries a distinct shape/character.


const MAP: Record<string, { glyph: string; label: string; cls: string }> = {
  DRAFT: { glyph: "○", label: "Draft", cls: "" },
  QUEUED: { glyph: "○", label: "Queued", cls: "" },
  WAITING_FOR_SLOT: { glyph: "◷", label: "Waiting for slot", cls: "wait" },
  WAITING_FOR_EXTERNAL_AGENT: { glyph: "◷", label: "Waiting — external agent active", cls: "wait" },
  PREPARING: { glyph: "◐", label: "Preparing", cls: "running" },
  STARTING: { glyph: "◐", label: "Starting", cls: "running" },
  RUNNING: { glyph: "●", label: "Running", cls: "running" },
  WAITING_FOR_PROVIDER: { glyph: "●", label: "Working", cls: "running" },
  WAITING_FOR_QUOTA: { glyph: "◷", label: "Waiting for quota", cls: "wait" },
  AUTO_RESUMING: { glyph: "↻", label: "Auto-resuming", cls: "running" },
  VERIFYING: { glyph: "✓·", label: "Verifying", cls: "running" },
  COMPLETED: { glyph: "✓", label: "Complete", cls: "complete" },
  BLOCKED: { glyph: "◧", label: "Blocked", cls: "wait" },
  NEEDS_ATTENTION: { glyph: "!", label: "Attention", cls: "failed" },
  FAILED: { glyph: "✕", label: "Failed", cls: "failed" },
  CANCEL_REQUESTED: { glyph: "ø", label: "Cancelling", cls: "wait" },
  CANCELLED: { glyph: "ø", label: "Cancelled", cls: "" },
  ORPHANED: { glyph: "?", label: "Orphaned", cls: "failed" },
  SKIPPED: { glyph: "→", label: "Skipped", cls: "" },
  INTEGRATING: { glyph: "⧉", label: "Integrating", cls: "running" },
  AWAITING_INTEGRATION: { glyph: "⧉!", label: "Awaiting integration", cls: "failed" },
};

export default function StatusGlyph({ state, pulse }: { state: string; pulse?: boolean }) {
  const m = MAP[state] ?? { glyph: "·", label: state, cls: "" };
  return (
    <span className={`ac-glyph ${m.cls} ${pulse ? "ac-pulse" : ""}`}>
      <span aria-hidden="true">{m.glyph}</span>
      <span>{m.label}</span>
      <span className="ac-sr"> ({state})</span>
    </span>
  );
}

export function stateLabel(state: string): string {
  return MAP[state]?.label ?? state;
}
