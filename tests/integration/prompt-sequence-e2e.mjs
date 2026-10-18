// Real folder reader -> extension import service -> IPC -> persistent daemon.
// All agents are fake and all state lives under a disposable AC_ROOT.
import { spawn } from 'node:child_process';
import { mkdtemp, mkdir, writeFile, rm } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { join } from 'node:path';
import { createRequire } from 'node:module';
import { buildSync } from 'esbuild';
import assert from 'node:assert/strict';

const require = createRequire(import.meta.url);
function compile(file) {
  const result = buildSync({ entryPoints: [file], bundle: true, platform: 'node', format: 'cjs', write: false });
  const mod = { exports: {} };
  new Function('module', 'exports', 'require', result.outputFiles[0].text)(mod, mod.exports, require);
  return mod.exports;
}
const { PromptSequenceImports } = compile('src/extension/promptSequence.ts');
const { DaemonClient } = compile('src/extension/daemonClient.ts');
const root = await mkdtemp('/tmp/ac-sequence-e2e-');
const prompts = join(root, 'prompts');
const workspace = join(root, 'project');
const socketPath = join(root, 'run/conductor.sock');
DaemonClient.tokenPath = () => join(root, 'run/conductor.token');
let daemon, client;
const wait = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(fn, timeout = 15000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) { if (await fn()) return; await wait(20); }
  throw new Error('Sequence E2E timed out');
}
async function start() {
  daemon = spawn(join(process.cwd(), 'dist/bin/agent-conductor'), ['serve'], {
    env: { ...process.env, AC_ROOT: root, AC_MOCK: '1',
      AC_CODEX_BIN: join(root, 'no-real-codex'), AC_CLAUDE_BIN: join(root, 'no-real-claude') }, stdio: 'ignore',
  });
  await until(() => existsSync(socketPath));
  client = new DaemonClient(socketPath);
  await client.connect();
}
async function stop() {
  client?.close();
  if (daemon && daemon.exitCode === null) {
    const exited = new Promise(resolve => daemon.once('exit', resolve));
    daemon.kill('SIGTERM');
    await exited;
  }
}
try {
  await mkdir(prompts); await mkdir(workspace);
  const contents = ['# Build\r\nPreserve UTF-8: café 😀.\r\n', '# Add feature\nUse the previous files.\n', '# Verify\nRun integration checks.\n'];
  for (let i = 0; i < contents.length; i++) await writeFile(join(prompts, `00${i + 1}-step.md`), contents[i]);
  await writeFile(join(prompts, 'README.md'), 'Supporting document: never queued');
  await start();
  await client.request('pauseQueue');
  const imports = new PromptSequenceImports();
  const preview = await imports.preview(prompts);
  assert.deepEqual(preview.errors, []);
  assert.equal(preview.files.length, 3);
  let submitted;
  const queued = await imports.import(preview.id, 'E2E folder sequence', workspace, {
    provider: 'codex', model: '', effort: '', permission: 'bypass', staged: false, attachSecrets: false,
  }, spec => {
    // AC_MOCK adds a provider named "fake"; it does not replace codex/claude.
    // Substitute only the execution adapter at the test's IPC boundary.
    submitted = { ...spec, rows: spec.rows.map(row => ({ ...row, runs: row.runs.map(run => ({ ...run, provider: 'fake' })) })) };
    return client.request('enqueuePromptSequence', submitted);
  });
  let snap = await client.request('getSnapshot');
  assert.equal(snap.runs.length, 3);
  assert(snap.runs.every(r => r.state === 'QUEUED' && r.cwd === workspace));
  assert(snap.rows[queued.batchId].every(row => row.mode === 'LINEAR'));
  await writeFile(join(prompts, '001-step.md'), 'Edited after import; must not replace queued work');
  await stop(); await start();
  const replay = await client.request('enqueuePromptSequence', submitted);
  assert.deepEqual(replay, queued, 'restart/replay must return the same batch');
  snap = await client.request('getSnapshot');
  assert.equal(snap.runs.length, 3, 'retry created duplicate runs');
  assert(snap.paused, 'pause must survive restart');
  await client.request('resumeQueue');
  await until(async () => {
    snap = await client.request('getSnapshot');
    const runs = [...snap.runs].sort((a, b) => a.title.localeCompare(b.title));
    for (let i = 1; i < runs.length; i++) {
      if (runs[i].providerSessionId) assert.equal(runs[i - 1].state, 'COMPLETED', 'later prompt started before prerequisite completed');
    }
    return runs.every(r => r.state === 'COMPLETED');
  });
  const runs = [...snap.runs].sort((a, b) => a.title.localeCompare(b.title));
  assert.equal(new Set(runs.map(r => r.providerSessionId)).size, 3, 'each step needs its own native conversation');
  for (let i = 0; i < runs.length; i++) {
    const effective = await client.request('revealEffectivePrompt', { runId: runs[i].id });
    assert(effective.text.includes(contents[i]), 'imported text changed');
    if (i) assert(Date.parse(runs[i].startedAt) >= Date.parse(runs[i - 1].finishedAt));
  }
  console.log('SEQUENCE E2E PASSED: folder preview, atomic import, separate workspace, verbatim text, restart/retry deduplication, serial execution and fresh conversations.');
} finally {
  await stop();
  await rm(root, { recursive: true, force: true });
}
