// First-run checklist: one line per question, each with the button that fixes
// it. No paragraphs of instructions, and nothing to read the README for.
import { useEffect, useState } from "react";
import { vscodeApi, copyText, notify } from "../api/client";

export interface SetupCheck {
  id: string;
  label: string;
  state: "ok" | "warn" | "fail" | "pending";
  detail: string;
  fixCommand?: string;
  fixLabel?: string;
  copy?: string;
  optional?: boolean;
}

export interface SetupStatus {
  checks: SetupCheck[];
  ready: boolean;
}

const GLYPH: Record<SetupCheck["state"], { mark: string; cls: string; label: string }> = {
  ok: { mark: "✓", cls: "complete", label: "ready" },
  warn: { mark: "!", cls: "wait", label: "optional" },
  fail: { mark: "✕", cls: "failed", label: "needs setup" },
  pending: { mark: "·", cls: "", label: "checking" },
};

export function useSetupStatus(): SetupStatus | null {
  const [status, setStatus] = useState<SetupStatus | null>(null);
  useEffect(() => {
    const handler = (ev: MessageEvent) => {
      if (ev.data?.type === "setupStatus") {
        setStatus(ev.data.status as SetupStatus);
      }
    };
    window.addEventListener("message", handler);
    vscodeApi.postMessage({ type: "requestSetupStatus" });
    return () => window.removeEventListener("message", handler);
  }, []);
  return status;
}

export default function Onboarding({ status }: { status: SetupStatus }) {
  const [dismissed, setDismissed] = useState(false);
  const blocking = status.checks.filter((c) => c.state === "fail" && !c.optional);
  if (dismissed || blocking.length === 0) {
    return null;
  }
  return (
    <section className="ac-section" aria-label="Setup">
      <div className="ac-card">
        <div className="ac-row between">
          <strong>Finish setting up</strong>
          <button className="ac-icon-btn" aria-label="Dismiss setup checklist" onClick={() => setDismissed(true)}>
            ✕
          </button>
        </div>
        <div className="ac-xs ac-muted ac-mt-1">
          {blocking.length} thing{blocking.length === 1 ? "" : "s"} left before the first run can start.
        </div>
        <ul className="ac-checklist ac-mt-3">
          {status.checks.map((c) => {
            const g = GLYPH[c.state];
            return (
              <li key={c.id}>
                <span className={`ac-glyph ${g.cls}`} aria-hidden="true">
                  {g.mark}
                </span>
                <span className="ac-grow">
                  <span className="ac-block">{c.label}</span>
                  <span className="ac-xs ac-muted ac-block ac-wrap">{c.detail}</span>
                  <span className="ac-sr">
                    {c.label}: {g.label}. {c.detail}
                  </span>
                </span>
                {c.fixCommand && (
                  <button
                    className="ac-btn ac-btn-sm"
                    onClick={() => vscodeApi.postMessage({ type: "cmd", command: c.fixCommand })}
                  >
                    {c.fixLabel ?? "Fix"}
                  </button>
                )}
                {!c.fixCommand && c.copy && (
                  <button
                    className="ac-btn ghost ac-btn-sm"
                    onClick={() => {
                      copyText(c.copy!);
                      notify(`Copied: ${c.copy}`);
                    }}
                  >
                    Copy command
                  </button>
                )}
              </li>
            );
          })}
        </ul>
      </div>
    </section>
  );
}
