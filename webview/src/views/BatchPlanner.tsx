// Wide batch planner: rows of LINEAR/PARALLEL cards with keyboard reordering.
import { useState } from "react";
import type { ConductorState } from "../state/store";
import { useConductor } from "../state/store";
import { rpc } from "../api/client";

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
    } catch (e: any) {
      setError(String(e.message ?? e));
    }
  };

  return (
    <div className="ac-planner">
      <div className="ac-row between" style={{ marginBottom: 12 }}>
        <h1 style={{ fontSize: 16, margin: 0 }}>Batch Planner</h1>
        <button className="ac-btn" onClick={() => void queueBatch()}>Queue Batch</button>
      </div>
      <div className="ac-row" style={{ gap: 12, marginBottom: 4 }}>
        <div className="ac-grow">
          <label className="ac-label" htmlFor="bp-title">Batch title</label>
          <input id="bp-title" className="ac-input" value={title} onChange={(e) => setTitle(e.target.value)} placeholder="Authentication Rewrite" />
        </div>
        <div className="ac-grow">
          <label className="ac-label">Base workspace</label>
          <div className="ac-input ac-mono ac-small" style={{ opacity: 0.8 }}>{workspace || "(open a folder first)"}</div>
        </div>
      </div>
      {error && <div className="ac-banner err" role="alert">{error}</div>}
      {ok && <div className="ac-banner" role="status">{ok}</div>}

      <div className="ac-planner-grid" style={{ marginTop: 12 }}>
        {rows.map((row, i) => (
          <article key={row.id} className="ac-card" aria-label={`Row ${i + 1}, ${row.mode}`}>
            <div className="ac-row between" style={{ marginBottom: 8 }}>
              <div className="ac-row">
                <span className="ac-rownum ac-num">{i + 1}</span>
                <div className="ac-seg" role="group" aria-label="Row type">
                  {(["LINEAR", "PARALLEL"] as const).map((m) => (
                    <button
                      key={m}
                      aria-pressed={row.mode === m}
                      onClick={() => mutate((d) => {
                        const r = d[i];
                        if (m === "LINEAR") {
                          if (r.cards.length > 1 && !confirm("Switch to LINEAR keeps only the first card. Continue?")) {
                            return;
                          }
                          r.cards = r.cards.slice(0, 1);
                        }
                        r.mode = m;
                      })}
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
              <button className="ac-btn ac-btn-sm secondary" style={{ marginTop: 8 }}
                onClick={() => mutate((d) => { d[i].cards.push(newCard(d[i].cards.length % 2 ? "codex" : "claude")); })}>
                + Card
              </button>
            )}
          </article>
        ))}
      </div>

      <div className="ac-row" style={{ marginTop: 12, gap: 8 }}>
        <button className="ac-btn secondary" onClick={() => mutate((d) => { d.push(newRow("LINEAR")); })}>+ Linear Row</button>
        <button className="ac-btn secondary" onClick={() => mutate((d) => {
          const r = newRow("PARALLEL");
          r.cards = [newCard("claude"), newCard("codex")];
          d.push(r);
        })}>+ Parallel Row</button>
        <div className="ac-grow" />
        <button className="ac-btn" onClick={() => void queueBatch()}>Queue Batch</button>
      </div>
      <p className="ac-small ac-muted" style={{ marginTop: 16 }}>
        Linear rows run one run at a time; parallel rows run concurrently in isolated Git worktrees
        (clean repository required) and merge into an internal integration branch. Nothing touches your
        original branch until you apply the batch.
      </p>
    </div>
  );
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
      <div className="ac-row between" style={{ marginBottom: 6 }}>
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
      <textarea className="ac-textarea" style={{ minHeight: 90 }} value={card.prompt}
        placeholder="Paste this card's task…" onChange={(e) => onPrompt(e.target.value)} />
      <div className="ac-row" style={{ gap: 6, marginTop: 6, alignItems: "flex-start" }}>
        <div className="ac-grow">
          <label className="ac-label">Model</label>
          <select className="ac-select" value={card.model} onChange={(e) => onPatch({ model: e.target.value })}>
            <option value="">(provider default)</option>
            {models.map((m) => <option key={m.id} value={m.id}>{m.name || m.id}</option>)}
          </select>
        </div>
        <div className="ac-grow">
          <label className="ac-label">Effort</label>
          <select className="ac-select" value={card.effort} onChange={(e) => onPatch({ effort: e.target.value })}>
            {efforts.map((f) => <option key={f} value={f}>{f}</option>)}
          </select>
        </div>
      </div>
      <div className="ac-row" style={{ gap: 6, marginTop: 6, alignItems: "flex-start" }}>
        <div className="ac-grow">
          <label className="ac-label">Permission</label>
          <select className="ac-select" value={card.permission} onChange={(e) => onPatch({ permission: e.target.value })}>
            <option value="bypass">Bypass</option>
            <option value="acceptEdits">Accept edits</option>
            <option value="default">Ask</option>
            <option value="plan">Plan only</option>
          </select>
        </div>
        <div style={{ paddingTop: 20 }}>
          <label className="ac-check" style={{ margin: 0 }}>
            <input type="checkbox" checked={card.attachSecrets} onChange={(e) => onPatch({ attachSecrets: e.target.checked })} />
            Credentials
          </label>
          <label className="ac-check" style={{ margin: 0 }}>
            <input type="checkbox" checked={card.staged} onChange={(e) => onPatch({ staged: e.target.checked })} />
            Staged
          </label>
        </div>
      </div>
    </div>
  );
}
