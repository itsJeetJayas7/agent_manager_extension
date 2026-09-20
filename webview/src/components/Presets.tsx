// Saved composer configurations (§7.3).
//
// Most real usage is the same shape repeated, so the shape is worth naming.
// Presets live in the webview's own per-workspace state: they are a
// convenience, never something a run depends on.
import { useCallback, useEffect, useState } from "react";
import { vscodeApi } from "../api/client";

export interface Preset {
  name: string;
  provider: string;
  model: string;
  effort: string;
  permission: string;
  staged: boolean;
  attachSecrets: boolean;
}

function read(workspace: string): Preset[] {
  try {
    const all = vscodeApi.getState()?.presets ?? {};
    const list = all[workspace];
    return Array.isArray(list) ? list : [];
  } catch {
    return [];
  }
}

function write(workspace: string, presets: Preset[]): void {
  try {
    const st = vscodeApi.getState() ?? {};
    vscodeApi.setState({ ...st, presets: { ...(st.presets ?? {}), [workspace]: presets } });
  } catch {
    // never block the composer on storage
  }
}

export function usePresets(workspace: string) {
  const [presets, setPresets] = useState<Preset[]>(() => read(workspace));
  useEffect(() => setPresets(read(workspace)), [workspace]);

  const save = useCallback(
    (preset: Preset) => {
      setPresets((cur) => {
        const next = [...cur.filter((p) => p.name !== preset.name), preset].sort((a, b) =>
          a.name.localeCompare(b.name)
        );
        write(workspace, next);
        return next;
      });
    },
    [workspace]
  );

  const remove = useCallback(
    (name: string) => {
      setPresets((cur) => {
        const next = cur.filter((p) => p.name !== name);
        write(workspace, next);
        return next;
      });
    },
    [workspace]
  );

  return { presets, save, remove };
}

export default function PresetBar({
  presets,
  onApply,
  onSave,
  onRemove,
}: {
  presets: Preset[];
  onApply: (p: Preset) => void;
  onSave: (name: string) => void;
  onRemove: (name: string) => void;
}) {
  const [naming, setNaming] = useState(false);
  const [name, setName] = useState("");

  const commit = () => {
    const trimmed = name.trim();
    if (trimmed === "") {
      return;
    }
    onSave(trimmed);
    setName("");
    setNaming(false);
  };

  return (
    <div className="ac-row wrap ac-gap-1 ac-mt-2" aria-label="Saved setups">
      {presets.map((p) => (
        <span key={p.name} className="ac-preset">
          <button className="ac-chip" onClick={() => onApply(p)} title={`${p.provider} · ${p.model || "default"} · ${p.effort}`}>
            {p.name}
          </button>
          <button
            className="ac-icon-btn ac-preset-x"
            aria-label={`Forget the “${p.name}” setup`}
            onClick={() => onRemove(p.name)}
          >
            ✕
          </button>
        </span>
      ))}
      {naming ? (
        <span className="ac-row ac-gap-1">
          <label className="ac-sr" htmlFor="ac-preset-name">
            Name this setup
          </label>
          <input
            id="ac-preset-name"
            className="ac-input ac-input-sm"
            autoFocus
            placeholder="name this setup"
            value={name}
            onChange={(e) => setName(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                commit();
              } else if (e.key === "Escape") {
                setNaming(false);
                setName("");
              }
            }}
          />
          <button className="ac-chip" onClick={commit}>
            Save
          </button>
        </span>
      ) : (
        <button className="ac-chip" onClick={() => setNaming(true)} title="Save the current provider, model, effort and permission">
          + Save setup
        </button>
      )}
    </div>
  );
}
