import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { promises as fs } from "fs";
import * as os from "os";
import * as path from "path";
import { PromptSequenceImports, readPromptSequence, SEQUENCE_LIMITS } from "../../src/extension/promptSequence";
import type { BatchSpecInput, SequenceSettings } from "../../src/extension/protocol";

let folder: string;
const settings: SequenceSettings = { provider: "codex", model: "test-model", effort: "high", permission: "bypass", staged: false, attachSecrets: false };
beforeEach(async () => { folder = await fs.mkdtemp(path.join(os.tmpdir(), "ac-sequence-test-")); });
afterEach(async () => { await fs.rm(folder, { recursive: true, force: true }); });
const write = (name: string, text: string | Buffer = "Implement and verify this step.") => fs.writeFile(path.join(folder, name), text);

describe("folder sequence validation", () => {
  it("sorts by number and preserves every character, including BOM, CRLF and Unicode", async () => {
    const text = "\ufeff# Task 😀\r\n\r\nKeep indentation.  \r\n";
    await write("002-check.md");
    await write("001-build.md", text);
    await write("README.md");
    await fs.mkdir(path.join(folder, "support"));
    await fs.writeFile(path.join(folder, "support", "003-nested.md"), "not imported");
    const preview = await readPromptSequence(folder);
    expect(preview.errors).toEqual([]);
    expect(preview.files.map(f => f.name)).toEqual(["001-build.md", "002-check.md"]);
    expect(preview.files[0].prompt).toBe(text);
    expect(preview.files[0].bytes).toBe(Buffer.byteLength(text));
    expect(preview.ignored).toEqual(["README.md", "support/"]);
  });

  it("sorts numerically even with different padding widths", async () => {
    await write("0002-check.md");
    await write("001-build.md");
    expect((await readPromptSequence(folder)).files.map(f => f.order)).toEqual([1, 2]);
  });

  it("blocks gaps, duplicate numbers, invalid names and zero", async () => {
    for (const name of ["001-one.md", "0001-duplicate.md", "003-gap.md", "000-zero.md", "4-unpadded.md", "005-Upper.MD"]) await write(name);
    const errors = (await readPromptSequence(folder)).errors.join("\n");
    expect(errors).toContain("duplicate step number 1");
    expect(errors).toContain("expected step 002");
    expect(errors).toContain("numbering must start at 001");
    expect(errors).toContain("4-unpadded.md");
    expect(errors).toContain("005-Upper.MD");
  });

  it("blocks empty, binary and malformed UTF-8 prompts", async () => {
    await write("001-empty.md", "\ufeff \r\n");
    await write("002-binary.md", "text\0data");
    await write("003-encoding.md", Buffer.from([0xff, 0xfe, 0x61]));
    const preview = await readPromptSequence(folder);
    expect(preview.files).toHaveLength(0);
    expect(preview.errors).toHaveLength(3);
    expect(preview.errors.join(" ")).toMatch(/empty.*binary.*encoded/s);
  });

  it("rejects symbolic links and numbered directories", async () => {
    await write("source.txt");
    await fs.symlink(path.join(folder, "source.txt"), path.join(folder, "001-linked.md"));
    await fs.mkdir(path.join(folder, "002-folder.md"));
    const preview = await readPromptSequence(folder);
    expect(preview.files).toEqual([]);
    expect(preview.errors).toHaveLength(2);
    expect(preview.errors.every(e => e.includes("regular files"))).toBe(true);
  });

  it("rejects an oversized file and an oversized sequence", async () => {
    await write("001-large.md", "x".repeat(SEQUENCE_LIMITS.fileBytes + 1));
    expect((await readPromptSequence(folder)).errors.join(" ")).toContain("256 KiB");
    await write("001-large.md", "x".repeat(SEQUENCE_LIMITS.fileBytes));
    for (let i = 2; i <= 33; i++) await write(`${String(i).padStart(3, "0")}-step.md`, "x".repeat(SEQUENCE_LIMITS.fileBytes));
    expect((await readPromptSequence(folder)).errors.join(" ")).toContain("8 MiB");
  });

  it("rejects more than 500 files before loading contents", async () => {
    await Promise.all(Array.from({ length: 501 }, (_, i) => write(`${String(i + 1).padStart(3, "0")}-step.md`)));
    const preview = await readPromptSequence(folder);
    expect(preview.files).toHaveLength(0);
    expect(preview.errors.join(" ")).toContain("at most 500");
  });

  it("explains an empty folder and rejects relative or missing folders", async () => {
    expect((await readPromptSequence(folder)).errors.join(" ")).toContain("No numbered prompt files");
    await expect(readPromptSequence("relative")).rejects.toThrow("absolute");
    await expect(readPromptSequence(path.join(folder, "missing"))).rejects.toThrow();
  });
});

describe("reviewed sequence imports", () => {
  it("imports all prompts as one batch with ordered LINEAR rows and chosen working folder", async () => {
    await write("001-first.md", "  first\r\n");
    await write("002-second.md", "second");
    const imports = new PromptSequenceImports();
    const preview = await imports.preview(folder);
    const enqueue = vi.fn(async (_spec: BatchSpecInput) => ({ batchId: "batch" }));
    const cwd = await fs.mkdtemp(path.join(folder, "workspace-"));
    await imports.import(preview.id, "My sequence", cwd, settings, enqueue);
    const spec = enqueue.mock.calls[0][0];
    expect(spec).toMatchObject({ importKey: preview.id, title: "My sequence", cwd });
    expect(spec.rows.map(r => r.mode)).toEqual(["LINEAR", "LINEAR"]);
    expect(spec.rows.map(r => r.runs[0].prompt)).toEqual(["  first\r\n", "second"]);
    expect(spec.rows[0].runs[0]).toMatchObject({ ...settings, title: "001-first.md" });
  });

  it.each(["edit", "add", "delete"])("requires refresh when files %s after preview", async (change) => {
    await write("001-first.md");
    const imports = new PromptSequenceImports();
    const preview = await imports.preview(folder);
    if (change === "edit") await write("001-first.md", "changed");
    if (change === "add") await write("002-next.md");
    if (change === "delete") await fs.unlink(path.join(folder, "001-first.md"));
    const enqueue = vi.fn();
    await expect(imports.import(preview.id, "", folder, settings, enqueue)).rejects.toThrow("changed since preview");
    expect(enqueue).not.toHaveBeenCalled();
    const refreshed = await imports.refresh(preview.id);
    expect(refreshed.id).not.toBe(preview.id);
  });

  it("coalesces concurrent clicks and recovers a lost response using the original submission", async () => {
    await write("001-first.md");
    const imports = new PromptSequenceImports();
    const preview = await imports.preview(folder);
    const enqueue = vi.fn(async (_spec: BatchSpecInput) => { throw new Error("connection lost"); });
    await expect(imports.import(preview.id, "Original", folder, settings, enqueue)).rejects.toThrow("connection lost");
    await write("001-first.md", "later edits cannot affect a submitted import");
    const retry = vi.fn(async (_spec: BatchSpecInput) => ({ batchId: "original-batch" }));
    const results = await Promise.all([
      imports.import(preview.id, "Changed", folder, { ...settings, provider: "claude" }, retry),
      imports.import(preview.id, "Changed", folder, settings, retry),
    ]);
    expect(results).toEqual([{ batchId: "original-batch" }, { batchId: "original-batch" }]);
    expect(retry).toHaveBeenCalledTimes(1);
    expect(retry.mock.calls[0][0]).toEqual(enqueue.mock.calls[0][0]);
  });

  it("never submits invalid previews or unknown tokens", async () => {
    await write("001-empty.md", " ");
    const imports = new PromptSequenceImports();
    const preview = await imports.preview(folder);
    const enqueue = vi.fn();
    await expect(imports.import(preview.id, "", folder, settings, enqueue)).rejects.toThrow("Fix the folder errors");
    await expect(imports.import("missing", "", folder, settings, enqueue)).rejects.toThrow("expired");
    expect(enqueue).not.toHaveBeenCalled();
  });
});
