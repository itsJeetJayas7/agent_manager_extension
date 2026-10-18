import { useEffect, useRef, useState } from "react";
import type { ConductorState } from "../state/store";
import type { PromptSequencePreview, SequenceSettings } from "../../../src/extension/protocol";
import { copyText, openDetail, rpc } from "../api/client";

export const PROMPT_SEQUENCE_TEMPLATE = `Break the task above into prompt files in <FOLDER> for Agent Conductor, a VS Code extension that queues Claude Code/Codex coding agents for unattended execution.

Create a flat folder of UTF-8 Markdown files named 001-short-title.md, 002-short-title.md, etc. Use lowercase kebab-case titles and consecutive numbers starting at 001. Each file becomes one sequential step, and its entire contents become the prompt. Keep supporting documents outside this folder.

Make each prompt a focused, independently verifiable task. Include the objective, repository context and paths, prerequisites from earlier steps, implementation instructions, constraints, and acceptance checks. Assume a fresh agent conversation for every step: earlier work is available through repository files, not chat memory. Agents execute in the working folder selected during import, which may differ from this prompt folder.

Tell each agent to inspect current code, preserve earlier work, implement and verify its task, and finish with <AC_RESULT>COMPLETE</AC_RESULT> or <AC_RESULT>BLOCKED: reason</AC_RESULT>. A blocked step holds later steps until resolved or explicitly skipped.

Cover the full request without gaps or duplicated work, ending with integration verification. Create only the prompt files now; do not execute their tasks.`;

export default function PromptSequenceImport({ state, settings, visible, onBusy }: {
  state: ConductorState;
  settings: SequenceSettings;
  visible: boolean;
  onBusy: (busy: boolean) => void;
}) {
  const [preview, setPreview] = useState<PromptSequencePreview | null>(null);
  const [cwd, setCwd] = useState(state.workspace);
  const [title, setTitle] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<{ batchId: string; count: number } | null>(null);
  const [copied, setCopied] = useState(false);
  const [retryOnly, setRetryOnly] = useState(false);
  const initialized = useRef(false);
  const inFlight = useRef(false);
  const workFolderPicked = useRef(false);

  useEffect(() => { if (!workFolderPicked.current) setCwd(state.workspace); }, [state.workspace]);

  const perform = async (action: () => Promise<void>) => {
    if (inFlight.current) return;
    inFlight.current = true;
    setBusy(true);
    onBusy(true);
    setError("");
    let keepSettingsLocked = retryOnly;
    try { await action(); keepSettingsLocked = false; setRetryOnly(false); }
    catch (e) {
      const message = e instanceof Error ? e.message : String(e);
      setError(message);
      if (/^(Import status could not be confirmed:|Update and restart the Agent Conductor|request timeout: promptSequence.import)/.test(message)) {
        keepSettingsLocked = true;
        setRetryOnly(true);
      }
    }
    finally { inFlight.current = false; setBusy(false); onBusy(keepSettingsLocked); }
  };

  const pick = () => perform(async () => {
    const next = await rpc<PromptSequencePreview | null>("promptSequence.pick", {}, 300_000);
    if (next) { setPreview(next); setTitle(next.title); setResult(null); }
  });

  useEffect(() => {
    if (visible && !initialized.current) {
      initialized.current = true;
      void pick();
    }
  }, [visible]);

  return (
    <div hidden={!visible} className="ac-sequence" aria-label="Import prompt sequence">
      <div className="ac-row between ac-mt-3">
        <span className="ac-section-title">Prompt sequence</span>
        <button className="ac-btn ghost ac-btn-sm" onClick={() => { copyText(PROMPT_SEQUENCE_TEMPLATE); setCopied(true); }}>
          {copied ? "Template copied" : "Copy generator prompt"}
        </button>
      </div>
      <p className="ac-small ac-muted">One numbered Markdown file per step. Each step waits for the previous one to succeed or be explicitly skipped.</p>
      <div className="ac-row ac-mt-2">
        <button className="ac-btn ghost" disabled={busy || retryOnly} onClick={() => void pick()}>{preview ? "Change prompt folder" : "Choose prompt folder"}</button>
        {preview && !result && <button className="ac-btn ghost" disabled={busy || retryOnly} onClick={() => void perform(async () => {
          const next = await rpc<PromptSequencePreview>("promptSequence.refresh", { id: preview.id });
          setPreview(next);
        })}>Refresh preview</button>}
      </div>
      {busy && <div className="ac-small ac-muted ac-mt-2" role="status">Working…</div>}
      {error && <div className="ac-banner err ac-mt-2" role="alert">{error}</div>}
      {result && <div className="ac-banner ac-mt-2" role="status">
        {result.count} prompts queued in order. {state.snapshot?.paused ? "The queue is paused." : "They will start when the scheduler has a slot."}
        <div className="ac-mt-2"><button className="ac-btn ghost" onClick={() => openDetail(result.batchId)}>View sequence</button></div>
      </div>}
      {preview && !result && <>
        <dl className="ac-sequence-paths ac-mt-3">
          <dt className="ac-label">Prompt folder</dt><dd>{preview.folder}</dd>
          <dt className="ac-label ac-mt-2">Working folder</dt><dd>{cwd || "Choose where agents should make changes"}</dd>
        </dl>
        <button className="ac-btn ghost ac-btn-sm" disabled={busy || retryOnly} onClick={() => void perform(async () => {
          const folder = await rpc<string | null>("promptSequence.pickWorkspace", {}, 300_000);
          if (folder) { workFolderPicked.current = true; setCwd(folder); }
        })}>Change working folder</button>
        <p className="ac-xs ac-muted">Agents make changes in the working folder. Prompt files are read once on import; later edits do not change queued work.</p>
        <label className="ac-label ac-mt-3" htmlFor="ac-sequence-title">Sequence title</label>
        <input id="ac-sequence-title" className="ac-input" value={title} disabled={busy || retryOnly} onChange={(e) => setTitle(e.target.value)} />
        {!!preview.errors.length && <div className="ac-banner err ac-mt-2" role="alert">
          <strong>Fix these files, then refresh</strong>
          <ul>{preview.errors.map((message, index) => <li key={index}>{message}</li>)}</ul>
        </div>}
        <div className="ac-row between ac-mt-3">
          <strong className="ac-small">{preview.files.length} steps in execution order</strong>
          <span className="ac-xs ac-muted">{Math.ceil(preview.totalBytes / 1024)} KiB</span>
        </div>
        <ol className="ac-sequence-files">
          {preview.files.map((file) => <li key={file.name}>
            <details>
              <summary><span className="ac-num ac-muted">{String(file.order).padStart(3, "0")}</span><span>{file.name.replace(/^\d+-/, "").replace(/\.md$/, "").replace(/-/g, " ")}</span></summary>
              <div className="ac-xs ac-muted ac-sequence-filename">{file.name} · {file.bytes.toLocaleString()} bytes</div>
              <pre className="ac-sequence-prompt" tabIndex={0} aria-label={`Prompt ${file.name}`}>{file.prompt}</pre>
            </details>
          </li>)}
        </ol>
        {!!preview.ignored.length && <details className="ac-small ac-muted ac-mt-2">
          <summary>{preview.ignored.length} other entries ignored</summary>
          <ul className="ac-sequence-ignored">{preview.ignored.map((name) => <li key={name}>{name}</li>)}</ul>
        </details>}
        <p className="ac-xs ac-muted">Uses {settings.provider === "claude" ? "Claude" : "Codex"} and the settings above for every step. Each step starts a fresh conversation and sees earlier work through files.</p>
        <button className="ac-btn ac-mt-2" disabled={busy || !state.daemonHealthy || !cwd || !!preview.errors.length || !preview.files.length} onClick={() => void perform(async () => {
          const queued = await rpc<{ batchId: string }>("promptSequence.import", { id: preview.id, title, cwd, settings }, 90_000);
          setResult({ ...queued, count: preview.files.length });
          setPreview(null);
        })}>{retryOnly ? "Recover import status" : `Queue ${preview.files.length} prompts in order`}</button>
        {!state.daemonHealthy && <p className="ac-xs ac-muted">Reconnect the daemon to queue this sequence.</p>}
      </>}
    </div>
  );
}
