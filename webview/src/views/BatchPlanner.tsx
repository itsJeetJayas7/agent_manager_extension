// Wide batch planner: rows of LINEAR/PARALLEL cards with keyboard reordering.
import { useState } from "react";
import type { ConductorState } from "../state/store";
import { useConductor } from "../state/store";
import { rpc, confirm, notify } from "../api/client";

interface CardDraft {
  prompt: string;
  provider: string;
  model: string;
  effort: string;
  permission: string;
  attachSecrets: boolean;
  staged: boolean;
  title: string;
}
interface RowDraft {
  id: string;
  mode: "LINEAR" | "PARALLEL";
  cards: CardDraft[];
}

let uid = 1;
const newCard = (provider = "claude"): CardDraft => ({
  prompt: "", provider, model: "", effort: "high", permission: "bypass",
  attachSecrets: false, staged: false, title: "",
});
const newRow = (mode: "LINEAR" | "PARALLEL"): RowDraft => ({
  id: `row-${uid++}`, mode,
  cards: [newCard()],
});

export default function BatchPlanner() {
  const state = useConductor();
  const workspace = state.workspace || "";
  const [title, setTitle] = useState("");
  const [rows, setRows] = useState<RowDraft[]>([newRow("LINEAR")]);
  const [error, setError] = useState("");
  const [ok, setOk] = useState("");
  const [dragRow, setDragRow] = useState<string | null>(null);
  const [overRow, setOverRow] = useState<string | null>(null);


  const mutate = (fn: (draft: RowDraft[]) => RowDraft[] | void) => {
    setRows((prev) => {
      const next = structuredClone(prev);
      const out = fn(next);
      return out ?? next;
    });
  };

  const moveRow = (i: number, dir: -1 | 1) => {
    const j = i + dir;
    if (j < 0 || j >= rows.length) {
      return;
    }
    mutate((d) => {
      const [r] = d.splice(i, 1);
      d.splice(j, 0, r);
    });
  };
  const dropRow = (targetId: string) => {
    const from = rows.findIndex((r) => r.id === dragRow);
    const to = rows.findIndex((r) => r.id === targetId);
    setDragRow(null);
    setOverRow(null);
    if (from < 0 || to < 0 || from === to) {
      return;
    }
    mutate((d) => {
      const [r] = d.splice(from, 1);
      d.splice(to, 0, r);
    });
  };

  const moveCard = (rowIdx: number, cardIdx: number, dir: -1 | 1) => {
    mutate((d) => {
      const row = d[rowIdx];
      const j = cardIdx + dir;
      if (j < 0 || j >= row.cards.length) {
        return;
      }
      const [c] = row.cards.splice(cardIdx, 1);
      row.cards.splice(j, 0, c);
    });
  };

  const queueBatch = async () => {
    setError("");
    setOk("");
    const spec = {
      title: title || "Batch",
      cwd: workspace,
      rows: rows.map((r) => ({
        mode: r.mode,
        runs: r.cards.map((c) => ({
          prompt: c.prompt, provider: c.provider, model: c.model, effort: c.effort,
          permission: c.permission, attachSecrets: c.attachSecrets, staged: c.staged, title: c.title,
        })),
      })),
    };
    try {
      const res = await rpc<{ batchId: string }>("enqueueBatch", spec);
      setOk(`Batch queued (${res.batchId.slice(0, 12)}…). See the Batches tab.`);
      notify("Batch queued.");
    } catch (e: any) {
      setError(String(e.message ?? e));
    }
  };

  return (
    <div className="ac-planner">
      <div className="ac-row between ac-mb-3">
        <h1 className="ac-display ac-h1">Batch planner</h1>
        <button className="ac-btn" onClick={() => void queueBatch()}>Queue batch</button>
      </div>
      <div className="ac-row ac-gap-3">
        <div className="ac-grow">
          <label className="ac-label" htmlFor="bp-title">Batch title</label>
          <input id="bp-title" className="ac-input" value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Authentication Rewrite" />
        </div>
        <div className="ac-grow">
          <label className="ac-label">Base workspace</label>
          <div className="ac-input ac-mono ac-small ac-muted ac-trunc">{workspace || "open a folder first"}</div>
        </div>
      </div>
      {error && <div className="ac-banner err" role="alert">{error}</div>}
      {ok && <div className="ac-banner" role="status">{ok}</div>}

      <div className="ac-plan-summary ac-mt-3" role="status">{summarise(rows, workspace)}</div>

      <div className="ac-planner-grid ac-mt-3">
        {rows.map((row, i) => (
          <article
            key={row.id}
            className="ac-card"
            aria-label={`Row ${i + 1}, ${row.mode}`}
            data-dragging={dragRow === row.id}
            data-dropbefore={overRow === row.id && dragRow !== row.id}
            onDragOver={(e) => {
              if (!dragRow) {
                return;
              }
              e.preventDefault();
              if (overRow !== row.id) {
                setOverRow(row.id);
              }
            }}
            onDrop={(e) => {
              e.preventDefault();
              dropRow(row.id);
            }}
          >
            <div className="ac-row between ac-mb-2">
              <div className="ac-row">
                <span
                  className="ac-drag"
                  draggable
                  role="button"
                  tabIndex={0}
                  aria-label={`Drag row ${i + 1} to reorder, or use the arrow buttons`}
                  title="Drag to reorder"
                  onDragStart={(e) => {
                    setDragRow(row.id);
                    e.dataTransfer.effectAllowed = "move";
                    e.dataTransfer.setData("text/plain", row.id);
                  }}
                  onDragEnd={() => {
                    setDragRow(null);
                    setOverRow(null);
                  }}
                >
                  ⠿
                </span>
                <span className="ac-rownum ac-num">{i + 1}</span>
                <div className="ac-seg" role="group" aria-label="Row type">
                  {(["LINEAR", "PARALLEL"] as const).map((m) => (
                    <button
                      key={m}
                      aria-pressed={row.mode === m}
                      onClick={async () => {
                        // window.confirm is a no-op inside a sandboxed webview,
                        // so this used to discard cards without asking.
                        if (m === "LINEAR" && rows[i].cards.length > 1) {
                          const okToDrop = await confirm(
                            `Switch row ${i + 1} to LINEAR?`,
                            {
                              detail: `Only the first of its ${rows[i].cards.length} cards is kept.`,
                              confirmLabel: "Keep first card",
                            }
                          );
                          if (!okToDrop) {
                            return;
                          }
                        }
                        mutate((d) => {
                          const r = d[i];
                          if (m === "LINEAR") {
                            r.cards = r.cards.slice(0, 1);
                          }
                          r.mode = m;
                        });
                      }}
                    >
                      {m}
                    </button>
                  ))}
                </div>
              </div>
              <div className="ac-row" role="group" aria-label="Row order">
                <button className="ac-icon-btn" aria-label="Move row up" onClick={() => moveRow(i, -1)} disabled={i === 0}>↑</button>
                <button className="ac-icon-btn" aria-label="Move row down" onClick={() => moveRow(i, 1)} disabled={i === rows.length - 1}>↓</button>
                <button className="ac-icon-btn" aria-label={`Remove row ${i + 1}`} onClick={() => mutate((d) => { d.splice(i, 1); })}>✕</button>
              </div>
            </div>
            <div className="ac-cards">
              {row.cards.map((card, ci) => (
                <CardEditor
                  key={ci}
                  card={card}
                  state={state}
                  isParallel={row.mode === "PARALLEL"}
                  onRemove={row.cards.length > (row.mode === "PARALLEL" ? 2 : 1) ? () => mutate((d) => { d[i].cards.splice(ci, 1); }) : undefined}
                  onDuplicate={() => mutate((d) => { d[i].cards.splice(ci + 1, 0, structuredClone(card)); })}
                  onMove={(dir) => moveCard(i, ci, dir)}
                  moveDisabled={(dir) => ci + dir < 0 || ci + dir >= row.cards.length}
                  onPrompt={(p) => mutate((d) => { d[i].cards[ci].prompt = p; })}
                  onPatch={(patch) => mutate((d) => { Object.assign(d[i].cards[ci], patch); })}
                />
              ))}
            </div>
            {row.mode === "PARALLEL" && (
              <button className="ac-btn ghost ac-btn-sm ac-mt-2"
                onClick={() => mutate((d) => { d[i].cards.push(newCard(d[i].cards.length % 2 ? "codex" : "claude")); })}>
                Add card
              </button>
            )}
          </article>
        ))}
      </div>

      <div className="ac-row ac-mt-3">
        <button className="ac-btn ghost" onClick={() => mutate((d) => { d.push(newRow("LINEAR")); })}>Add linear row</button>
        <button className="ac-btn ghost" onClick={() => mutate((d) => {
          const r = newRow("PARALLEL");
          r.cards = [newCard("claude"), newCard("codex")];
          d.push(r);
        })}>Add parallel row</button>
        <div className="ac-grow" />
        <button className="ac-btn" onClick={() => void queueBatch()}>Queue batch</button>
      </div>
      <p className="ac-small ac-muted ac-mt-4">
        Linear rows run one run at a time; parallel rows run concurrently in isolated Git worktrees
        (clean repository required) and merge into an internal integration branch. Nothing touches your
        original branch until you apply the batch.
      </p>
    </div>
  );
}

/** A live, plain-language description of what queueing will actually do. */
function summarise(rows: RowDraft[], workspace: string): string {
  const runs = rows.reduce((n, r) => n + r.cards.length, 0);
  const parallel = rows.filter((r) => r.mode === "PARALLEL").length;
  const empty = rows.reduce((n, r) => n + r.cards.filter((c) => c.prompt.trim() === "").length, 0);
  const parts = [
    `${rows.length} row${rows.length === 1 ? "" : "s"}`,
    `${runs} run${runs === 1 ? "" : "s"}`,
  ];
  if (parallel > 0) {
    parts.push(`${parallel} parallel · clean Git tree required`);
  }
  if (!workspace) {
    parts.push("no workspace folder open");
  }
  if (empty > 0) {
    parts.push(`${empty} card${empty === 1 ? "" : "s"} still empty`);
  }
  return parts.join(" · ");
}

function CardEditor({
  card, state, isParallel, onRemove, onDuplicate, onMove, moveDisabled, onPrompt, onPatch,
}: {
  card: CardDraft;
  state: ConductorState;
  isParallel: boolean;
  onRemove?: () => void;
  onDuplicate: () => void;
  onMove: (dir: -1 | 1) => void;
  moveDisabled: (dir: -1 | 1) => boolean;
  onPrompt: (p: string) => void;
  onPatch: (patch: Partial<CardDraft>) => void;
}) {
  const models = (state.snapshot?.providers?.[card.provider]?.models ?? []).filter((m) => !m.hidden);
  const efforts = models.find((m) => m.id === card.model)?.efforts ?? ["low", "medium", "high", "max"];
  return (
    <div className="ac-card-edit">
      <div className="ac-row between ac-mb-2">
        <div className="ac-seg" role="group" aria-label="Card provider">
          {["claude", "codex"].map((p) => (
            <button key={p} aria-pressed={card.provider === p} onClick={() => onPatch({ provider: p, model: "" })}>
              {p === "claude" ? "Claude" : "Codex"}
            </button>
          ))}
        </div>
        {isParallel && (
          <div role="group" aria-label="Card order">
            <button className="ac-icon-btn" aria-label="Move card left" onClick={() => onMove(-1)} disabled={moveDisabled(-1)}>←</button>
            <button className="ac-icon-btn" aria-label="Move card right" onClick={() => onMove(1)} disabled={moveDisabled(1)}>→</button>
          </div>
        )}
        <div role="group" aria-label="Card actions">
          <button className="ac-icon-btn" aria-label="Duplicate card" onClick={onDuplicate}>⧉</button>
          {onRemove && <button className="ac-icon-btn" aria-label="Remove card" onClick={onRemove}>✕</button>}
        </div>
      </div>
      <label className="ac-sr">Prompt</label>
      <textarea className="ac-textarea ac-textarea-sm" value={card.prompt}
        placeholder="Paste this card's task…" onChange={(e) => onPrompt(e.target.value)} />
      <div className="ac-inline-controls ac-mt-2">
        <div>
          <label className="ac-label">Model</label>
          <select className="ac-select" value={card.model} onChange={(e) => onPatch({ model: e.target.value })}>
            <option value="">(provider default)</option>
            {models.map((m) => <option key={m.id} value={m.id}>{m.name || m.id}</option>)}
          </select>
        </div>
        <div>
          <label className="ac-label">Effort</label>
          <select className="ac-select" value={card.effort} onChange={(e) => onPatch({ effort: e.target.value })}>
            {efforts.map((f) => <option key={f} value={f}>{f}</option>)}
          </select>
        </div>
      </div>
      <div className="ac-inline-controls ac-mt-2">
        <div>
          <label className="ac-label">Permission</label>
          <select className="ac-select" value={card.permission} onChange={(e) => onPatch({ permission: e.target.value })}>
            <option value="bypass">Bypass</option>
            <option value="acceptEdits">Accept edits</option>
            <option value="default">Ask</option>
            <option value="plan">Plan only</option>
          </select>
        </div>
        <div>
          <label className="ac-check">
            <input type="checkbox" checked={card.attachSecrets} onChange={(e) => onPatch({ attachSecrets: e.target.checked })} />
            <span>Credentials</span>
          </label>
          <label className="ac-check">
            <input type="checkbox" checked={card.staged} onChange={(e) => onPatch({ staged: e.target.checked })} />
            <span>Staged</span>
          </label>
        </div>
      </div>
    </div>
  );
}
