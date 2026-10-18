// Real IPC/load test with free fake providers. No production state is touched.
import { spawn, execFileSync } from 'node:child_process';
import { mkdtempSync, existsSync, readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import net from 'node:net';
import assert from 'node:assert/strict';
import { performance } from 'node:perf_hooks';

const root = mkdtempSync('/tmp/ac-stress-');
const daemon = spawn(join(process.cwd(), 'dist/bin/agent-conductor'), ['serve'], {
  env: { ...process.env, AC_ROOT: root, AC_MOCK: '1' }, stdio: ['ignore', 'ignore', 'pipe'],
});
let log = '', socket, next = 0, buf = '';
daemon.stderr.on('data', (d) => { log = (log + d).slice(-8000); });
const pending = new Map();
const wait = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(fn, timeout = 60000) {
  const end = Date.now() + timeout;
  while (Date.now() < end) { const value = await fn(); if (value) return value; await wait(20); }
  throw new Error('stress test timed out');
}
function req(method, params = {}) {
  return new Promise((resolve, reject) => {
    const id = String(++next);
    const timer = setTimeout(() => { pending.delete(id); reject(new Error(`RPC timeout: ${method}`)); }, 15000);
    pending.set(id, { resolve: (v) => { clearTimeout(timer); resolve(v); }, reject: (e) => { clearTimeout(timer); reject(e); } });
    socket.write(JSON.stringify({ protocol: 1, id, method, params }) + '\n');
  });
}
function rss() { return Number(execFileSync('ps', ['-o', 'rss=', '-p', String(daemon.pid)], { encoding: 'utf8' }).trim()); }
try {
  await until(() => existsSync(join(root, 'run/conductor.sock')));
  socket = net.createConnection(join(root, 'run/conductor.sock'));
  await new Promise((r, j) => { socket.once('connect', r); socket.once('error', j); });
  socket.on('error', (e) => { for (const p of pending.values()) p.reject(e); pending.clear(); });
  socket.on('data', (d) => {
    buf += d;
    for (let i; (i = buf.indexOf('\n')) >= 0;) {
      const m = JSON.parse(buf.slice(0, i)); buf = buf.slice(i + 1);
      const p = pending.get(m.id); if (!p) continue; pending.delete(m.id);
      m.error ? p.reject(new Error(JSON.stringify(m.error))) : p.resolve(m.result);
    }
  });
  await req('hello', { auth: readFileSync(join(root, 'run/conductor.token'), 'utf8').trim() });
  await req('getProviderCapabilities');
  await req('pauseQueue');
  const ids = [];
  const count = Number(process.env.AC_STRESS_RUNS || 100);
  const enqueueAt = performance.now();
  for (let i = 0; i < count; i++) ids.push((await req('enqueueRun', { prompt: `load task ${i}`, provider: 'fake', cwd: root })).runId);
  const enqueueMs = performance.now() - enqueueAt;
  const samples = [], beforeKB = rss();
  for (let wave = 0; wave < 20; wave++) {
    await Promise.all(Array.from({ length: 25 }, async () => {
      const t = performance.now(); const snap = await req('getSnapshot'); samples.push(performance.now() - t);
      assert.equal(snap.runs.length, count);
      assert(snap.runs.every((r) => r.state === 'QUEUED'));
    }));
  }
  await req('resumeQueue');
  const runAt = performance.now(); let peak = 0;
  const active = new Set(['RUNNING', 'STARTING', 'PREPARING', 'VERIFYING', 'AUTO_RESUMING', 'WAITING_FOR_PROVIDER']);
  await until(async () => {
    const snap = await req('getSnapshot');
    peak = Math.max(peak, snap.runs.filter((r) => active.has(r.state)).length);
    assert(peak <= 1, `serial queue overlap: ${peak}`);
    const failures = snap.runs.filter((r) => ['FAILED','NEEDS_ATTENTION','BLOCKED'].includes(r.state));
    assert.equal(failures.length, 0, JSON.stringify(failures));
    return snap.runs.every((r) => r.state === 'COMPLETED');
  }, 120000);
  samples.sort((a,b) => a-b);
  const result = { runs: count, snapshotRequests: samples.length, concurrentRequests: 25,
    enqueueMs: Math.round(enqueueMs), completionMs: Math.round(performance.now()-runAt),
    snapshotP50Ms: +samples[Math.floor(samples.length*.5)].toFixed(2),
    snapshotP95Ms: +samples[Math.floor(samples.length*.95)].toFixed(2),
    snapshotMaxMs: +samples.at(-1).toFixed(2), peakConcurrentRuns: peak,
    daemonRSSBeforeKB: beforeKB, daemonRSSAfterKB: rss() };
  assert(result.snapshotP95Ms < 3000, 'snapshot p95 exceeded 3 seconds');
  assert(!log.includes('refused: illegal transition'), 'illegal transition under load');
  console.log(JSON.stringify(result, null, 2));
  console.log('DAEMON STRESS CHECKS PASSED');
} catch (e) { console.error(log); throw e; }
finally {
  socket?.destroy(); daemon.kill('SIGTERM');
  await new Promise((resolve) => { if (daemon.exitCode !== null) return resolve(); daemon.once('exit', resolve); });
  rmSync(root, { recursive: true, force: true });
}
