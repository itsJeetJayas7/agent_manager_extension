import { constants, promises as fs } from "fs";
import * as path from "path";
import { createHash, randomUUID } from "crypto";
import { TextDecoder } from "util";
import type { BatchSpecInput, PromptSequencePreview, SequenceSettings } from "./protocol";

export const SEQUENCE_LIMITS = { files: 500, fileBytes: 256 * 1024, totalBytes: 8 * 1024 * 1024 };
const FILE_PATTERN = /^(\d{3,})-([a-z0-9]+(?:-[a-z0-9]+)*)\.md$/;

/** Runs on the workspace extension host, including on a Remote SSH host. */
export async function readPromptSequence(folder: string): Promise<PromptSequencePreview> {
  if (!path.isAbsolute(folder)) throw new Error("Select an absolute prompt folder path.");
  const preview: PromptSequencePreview = {
    id: randomUUID(), folder, title: path.basename(folder), files: [], ignored: [], errors: [], totalBytes: 0,
  };
  const entries = (await fs.readdir(folder, { withFileTypes: true })).sort((a, b) => a.name.localeCompare(b.name));
  const candidates: { name: string; order: number; regular: boolean }[] = [];
  for (const entry of entries) {
    const match = FILE_PATTERN.exec(entry.name);
    if (!match) {
      if (/^\d.*\.md$/i.test(entry.name)) {
        preview.errors.push(`${entry.name}: use a name like 001-short-title.md (lowercase, at least three digits).`);
      } else {
        preview.ignored.push(entry.name + (entry.isDirectory() ? "/" : ""));
      }
      continue;
    }
    const order = Number(match[1]);
    if (!Number.isSafeInteger(order) || order < 1) {
      preview.errors.push(`${entry.name}: numbering must start at 001.`);
      continue;
    }
    candidates.push({ name: entry.name, order, regular: entry.isFile() });
  }
  candidates.sort((a, b) => a.order - b.order || a.name.localeCompare(b.name));
  if (!candidates.length) preview.errors.push("No numbered prompt files found. Add 001-short-title.md to this folder.");
  if (candidates.length > SEQUENCE_LIMITS.files) {
    preview.errors.push(`Too many prompts. Import at most ${SEQUENCE_LIMITS.files} files at a time.`);
    return preview;
  }
  const seen = new Set<number>();
  let expected = 1;
  for (const entry of candidates) {
    if (seen.has(entry.order)) preview.errors.push(`${entry.name}: duplicate step number ${entry.order}.`);
    else {
      if (entry.order !== expected) preview.errors.push(`${entry.name}: expected step ${String(expected).padStart(3, "0")}; numbering must be consecutive.`);
      seen.add(entry.order);
      expected = entry.order + 1;
    }
    if (!entry.regular) {
      preview.errors.push(`${entry.name}: prompts must be regular files, not folders or symbolic links.`);
      continue;
    }
    try {
      // No following symlinks, even if an entry changes after readdir. Limit
      // the read itself so a growing file cannot exhaust extension memory.
      const handle = await fs.open(path.join(folder, entry.name), constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
      try {
        const stat = await handle.stat();
        if (!stat.isFile()) throw new Error("not a regular file");
        if (stat.size > SEQUENCE_LIMITS.fileBytes) throw new Error("exceeds the 256 KiB per-file limit");
        const buffer = Buffer.alloc(SEQUENCE_LIMITS.fileBytes + 1);
        let bytes = 0;
        while (bytes < buffer.length) {
          const read = await handle.read(buffer, bytes, buffer.length - bytes, null);
          if (!read.bytesRead) break;
          bytes += read.bytesRead;
        }
        if (bytes > SEQUENCE_LIMITS.fileBytes) throw new Error("exceeds the 256 KiB per-file limit");
        preview.totalBytes += bytes;
        if (preview.totalBytes > SEQUENCE_LIMITS.totalBytes) {
          preview.errors.push("The sequence exceeds the 8 MiB total limit.");
          break;
        }
        // Preserve BOM, whitespace and line endings: the file is the prompt.
        const prompt = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(buffer.subarray(0, bytes));
        if (!prompt.trim()) throw new Error("prompt is empty");
        if (prompt.includes("\0")) throw new Error("contains binary data; save as UTF-8 text");
        preview.files.push({ name: entry.name, order: entry.order, bytes, prompt });
      } finally {
        await handle.close();
      }
    } catch (error) {
      preview.errors.push(`${entry.name}: ${error instanceof Error ? error.message : String(error)}.`);
    }
  }
  return preview;
}

function fingerprint(preview: PromptSequencePreview): string {
  return createHash("sha256").update(JSON.stringify(preview.files)).digest("hex");
}

type Submission = { spec: BatchSpecInput; promise?: Promise<{ batchId: string }>; completed?: boolean };

/** Keeps reviewed text in memory only; the daemon encrypts it on import. */
export class PromptSequenceImports {
  private previews = new Map<string, { preview: PromptSequencePreview; submission?: Submission }>();

  async preview(folder: string): Promise<PromptSequencePreview> {
    const preview = await readPromptSequence(folder);
    // Bound memory when someone repeatedly browses folders. Keep submissions
    // so a lost RPC response can be retried using the same durable import key.
    for (const [id, record] of this.previews) {
      if (!record.submission || record.submission.completed) this.previews.delete(id);
    }
    this.previews.set(preview.id, { preview });
    return preview;
  }

  async refresh(id: string): Promise<PromptSequencePreview> {
    const record = this.get(id);
    if (record.submission) throw new Error("This import was already submitted. Retry it to recover its queue status.");
    return this.preview(record.preview.folder);
  }

  async import(id: string, title: string, cwd: string, settings: SequenceSettings,
    enqueue: (spec: BatchSpecInput) => Promise<{ batchId: string }>): Promise<{ batchId: string }> {
    const record = this.get(id);
    if (record.submission?.promise) return record.submission.promise;
    if (!record.submission) {
      if (record.preview.errors.length) throw new Error("Fix the folder errors and refresh the preview before importing.");
      if (!path.isAbsolute(cwd) || !(await fs.stat(cwd)).isDirectory()) throw new Error("Choose the working folder where agents should make changes.");
      if (!["claude", "codex"].includes(settings.provider)) throw new Error("Choose Claude or Codex.");
      if (!["bypass", "acceptEdits", "default", "plan"].includes(settings.permission)) throw new Error("Choose a valid permission mode.");
      const fresh = await readPromptSequence(record.preview.folder);
      if (fresh.errors.length || fingerprint(fresh) !== fingerprint(record.preview)) {
        throw new Error("Prompt files changed since preview. Refresh and review the sequence before importing.");
      }
      // Recheck after awaits: duplicate requests share one immutable submission.
      record.submission ??= { spec: {
        importKey: id, title: title.trim() || record.preview.title, cwd,
        rows: record.preview.files.map((file) => ({ mode: "LINEAR", runs: [{
          provider: settings.provider, model: settings.model, effort: settings.effort,
          permission: settings.permission, staged: !!settings.staged, attachSecrets: !!settings.attachSecrets,
          title: file.name, prompt: file.prompt,
        }] })),
      } };
    }
    const submission = record.submission;
    submission.promise ??= enqueue(submission.spec).then((result) => {
      submission.completed = true;
      // A completed retry only needs the result; release the raw prompt text.
      submission.spec = { ...submission.spec, rows: [] };
      record.preview = { ...record.preview, files: [] };
      return result;
    }).catch((error) => {
      submission.promise = undefined;
      throw error;
    });
    return submission.promise;
  }

  private get(id: string) {
    const record = this.previews.get(id);
    if (!record) throw new Error("Preview expired. Select the prompt folder again.");
    return record;
  }
}
