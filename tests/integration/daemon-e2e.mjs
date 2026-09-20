// End-to-end daemon verification: starts the daemon (fake provider),
// exercises the real IPC protocol, verifies lifecycle, secrets handling,
// completion gate bridge, restart persistence, and log redaction.
import * as net from "net";
import { spawn, spawnSync } from "child_process";
import { mkdtempSync, readFileSync, existsSync, rmSync } from "fs";
import { tmpdir } from "os";
import { join } from "path";
import assert from "assert";

const HOME = mkdtempSync(join(tmpdir(), "ac-e2e-"));
const ROOT = join(HOME, ".agent-conductor");
const BIN = join(process.cwd(), "dist", "bin", "agent-conductor");
const SOCK = join(ROOT, "run", "conductor.sock");
const TOKEN_PATH = join(ROOT, "run", "conductor.token");

function wait(ms) { return new Promise((r) => setTimeout(r, ms)); }
async function waitFor(fn, timeoutMs = 15000, what = "condition") {
  const t0 = Date.now();
  while (Date.now() - t0 < timeoutMs) {
    if (await fn()) return;
    await wait(150);
  }
  throw new Error(`timeout waiting for ${what}`);
}

class Client {
  constructor() { this.buf = ""; this.next = 1; this.pending = new Map(); this.events = []; }
  async connect() {
    this.conn = net.createConnection({ path: SOCK });
    await new Promise((res, rej) => {
      this.conn.once("connect", res);
      this.conn.once("error", rej);
    });
    this.conn.on("data", (c) => {
      this.buf += c.toString();
      let i;
      while ((i = this.buf.indexOf("\n")) >= 0) {
        const line = this.buf.slice(0, i); this.buf = this.buf.slice(i + 1);
        if (!line.trim()) continue;
        const m = JSON.parse(line);
        if (m.event) { this.events.push(m); }
        else if (m.id && this.pending.has(m.id)) {
          const p = this.pending.get(m.id); this.pending.delete(m.id);
          m.error ? p.reject(new Error(JSON.stringify(m.error))) : p.resolve(m.result);
        }
      }
    });
    const tok = readFileSync(TOKEN_PATH, "utf8").trim();
    await this.req("hello", { auth: tok });
  }
  req(method, params = {}) {
    return new Promise((resolve, reject) => {
      const id = String(this.next++);
      this.pending.set(id, { resolve, reject });
      this.conn.write(JSON.stringify({ protocol: 1, id, method, params }) + "\n");
    });
  }
  close() { this.conn?.end(); }
}

let daemon;
function startDaemon() {
  daemon = spawn(BIN, ["serve"], { env: { ...process.env, HOME, AC_MOCK: "1" }, stdio: "ignore" });
}

async function main() {
  console.log("1) starting daemon with fake provider…");
  startDaemon();
  await waitFor(() => existsSync(SOCK), 10000, "socket");
  const c = new Client();
  await c.connect();

  const health = await c.req("daemonHealth", {});
  assert(health.protocol === 1);
  console.log("   daemonHealth ok:", health.version);

  console.log("2) wrong token rejected…");
  {
    const c2 = new Client();
    await new Promise((res, rej) => {
      c2.conn = net.createConnection({ path: SOCK });
      c2.conn.once("connect", res); c2.conn.once("error", rej);
    });
    const bad = new Promise((res) => c2.conn.once("data", (d) => res(d.toString())));
    c2.conn.write(JSON.stringify({ protocol: 1, id: "x", method: "hello", auth: "wrong" }) + "\n");
    const resp = await bad;
    assert(resp.includes("UNAUTHORIZED"));
    c2.conn.destroy();
    console.log("   rejected as expected");
  }

  console.log("3) enqueue run + full lifecycle via real scheduler…");
  const { runId, batchId } = await c.req("enqueueRun", { prompt: "E2E simple task", provider: "fake", cwd: HOME });
  console.log("   enqueued", runId);
  await waitFor(async () => (await c.req("getSnapshot", {})).runs.find((r) => r.id === runId)?.state === "COMPLETED", 20000, "run completion");
  const snap = await c.req("getSnapshot", {});
  const run = snap.runs.find((r) => r.id === runId);
  assert.equal(run.finalOutcome, "COMPLETE");
  assert(run.providerSessionId);
  console.log("   run COMPLETED with native session", run.providerSessionId.slice(0, 16) + "…");

  console.log("4) secret block: encrypted storage + effective prompt attach…");
  await c.req("updateSecretBlock", { text: "sudo pw: E2E-SECRET-hunter42" });
  const { runId: r2 } = await c.req("enqueueRun", { prompt: "use credentials", provider: "fake", attachSecrets: true, cwd: HOME });
  await waitFor(async () => (await c.req("getSnapshot", {})).runs.find((r) => r.id === r2)?.state === "COMPLETED", 20000, "secret run completion");
  const eff = await c.req("revealEffectivePrompt", { runId: r2 });
  assert(eff.text.includes("E2E-SECRET-hunter42"), "secret attached to effective prompt");
  assert(eff.containsSecrets === true);
  const snap2 = await c.req("getSnapshot", {});
  const run2 = snap2.runs.find((r) => r.id === r2);
  assert(!run2.promptPreview.includes("E2E-SECRET"), "preview must not contain secret");
  console.log("   attached at dispatch, masked in previews");

  console.log("5) log redaction…");
  await waitFor(() => {
    try {
      const log = readFileSync(join(ROOT, "logs", "daemon.jsonl"), "utf8");
      return log.length > 0;
    } catch { return false; }
  }, 5000, "log file");
  const logData = readFileSync(join(ROOT, "logs", "daemon.jsonl"), "utf8");
  assert(!logData.includes("E2E-SECRET-hunter42"), "SECRET LEAKED INTO LOGS");
  console.log("   secrets absent from daemon logs");

  console.log("6) completion gate bridge (managed stop without marker)…");
  {
    const hookInput = JSON.stringify({
      hook_event_name: "Stop", session_id: "e2e-sess", stop_hook_active: false,
      last_assistant_message: "I think I'm done",
    });
    const r = spawnSync(BIN, ["hook", "claude", "--gate"], {
      input: hookInput,
      env: { ...process.env, HOME, AGENT_CONDUCTOR_RUN_ID: runId },
      encoding: "utf8", timeout: 10000,
    });
    const out = r.stdout.trim();
    const parsed = JSON.parse(out);
    assert.equal(parsed.decision, "block");
    assert(parsed.reason.includes("<AC_RESULT>COMPLETE</AC_RESULT>"));
    console.log("   gate blocks marker-less stop with continuation instruction");
    // second stop with stop_hook_active=true must NOT block
    const r2 = spawnSync(BIN, ["hook", "claude", "--gate"], {
      input: JSON.stringify({ hook_event_name: "Stop", session_id: "e2e-sess", stop_hook_active: true, last_assistant_message: "still no marker" }),
      env: { ...process.env, HOME, AGENT_CONDUCTOR_RUN_ID: runId },
      encoding: "utf8", timeout: 10000,
    });
    assert.equal(r2.stdout.trim(), "", "second stop must be allowed (no infinite loop)");
    console.log("   second stop (stop_hook_active) allowed");
    // external session: no run id, never gated
    const r3 = spawnSync(BIN, ["hook", "claude", "--gate"], {
      input: JSON.stringify({ hook_event_name: "Stop", session_id: "ext-sess", last_assistant_message: "external chat" }),
      env: { ...process.env, HOME },
      encoding: "utf8", timeout: 10000,
    });
    assert.equal(r3.stdout.trim(), "", "external sessions must never be gated");
    console.log("   external session never gated");
    // The globally installed hook is awareness-only: without --gate it never
    // blocks, so Claude merging both configs cannot fire two decisions.
    const r4 = spawnSync(BIN, ["hook", "claude"], {
      input: JSON.stringify({ hook_event_name: "Stop", session_id: "e2e-sess-2", stop_hook_active: false, last_assistant_message: "no marker here" }),
      env: { ...process.env, HOME, AGENT_CONDUCTOR_RUN_ID: runId },
      encoding: "utf8", timeout: 10000,
    });
    assert.equal(r4.stdout.trim(), "", "the global hook must be awareness-only");
    console.log("   global hook never gates (no double block decision)");
  }

  console.log("7) external awareness blocks FIFO queue…");
  {
    spawnSync(BIN, ["hook", "claude"], {
      input: JSON.stringify({ hook_event_name: "UserPromptSubmit", session_id: "ext-busy", cwd: "/srv/manual", prompt: "manual work" }),
      env: { ...process.env, HOME },
      encoding: "utf8", timeout: 10000,
    });
    const { runId: r3 } = await c.req("enqueueRun", { prompt: "should wait", provider: "fake", cwd: HOME });
    await wait(1200);
    const s3 = await c.req("getSnapshot", {});
    const st = s3.runs.find((r) => r.id === r3).state;
    assert(["QUEUED", "WAITING_FOR_EXTERNAL_AGENT", "WAITING_FOR_SLOT"].includes(st), `state ${st}`);
    console.log("   queued run waits while external agent busy:", st);
    // external stop unblocks
    spawnSync(BIN, ["hook", "claude"], {
      input: JSON.stringify({ hook_event_name: "Stop", session_id: "ext-busy" }),
      env: { ...process.env, HOME },
      encoding: "utf8", timeout: 10000,
    });
    await waitFor(async () => (await c.req("getSnapshot", {})).runs.find((r) => r.id === r3)?.state === "COMPLETED", 20000, "unblocked run");
    console.log("   run dispatched after external stop");
  }

  console.log("8) pause with a continuation timer…");
  {
    // Indefinite pause holds the queue.
    const st = await c.req("pauseQueue", { reason: "e2e hold" });
    assert.equal(st.paused, true);
    assert.equal(st.scope, "QUEUE_ONLY");
    assert.equal(st.resumeAt, undefined, "no timer was requested");

    const { runId: held } = await c.req("enqueueRun", { prompt: "held by pause", provider: "fake", cwd: HOME });
    await wait(1500);
    let s = await c.req("getSnapshot", {});
    assert.equal(s.pause.paused, true, "snapshot carries the pause state");
    assert.equal(s.pause.reason, "e2e hold");
    const heldState = s.runs.find((r) => r.id === held).state;
    assert(["QUEUED", "WAITING_FOR_SLOT"].includes(heldState), `run dispatched while paused: ${heldState}`);
    console.log("   queue held; run stayed", heldState);

    // Extending and clearing the timer.
    let timed = await c.req("pauseQueue", { durationMs: 60_000 });
    assert(timed.resumeAt, "a timer was requested");
    const first = new Date(timed.resumeAt).getTime();
    timed = await c.req("extendPause", { deltaMs: 60_000 });
    assert.equal(new Date(timed.resumeAt).getTime() - first, 60_000, "extend moves the deadline");
    timed = await c.req("clearPauseTimer", {});
    assert.equal(timed.paused, true, "clearing the timer keeps the pause");
    assert.equal(timed.resumeAt, undefined);
    console.log("   extendPause and clearPauseTimer behave");

    // Validation.
    for (const bad of [{ durationMs: -1 }, { durationMs: 8 * 24 * 3600_000 }, { resumeAt: new Date(Date.now() - 1000).toISOString() }]) {
      let threw = false;
      try { await c.req("pauseQueue", bad); } catch { threw = true; }
      assert(threw, `invalid pause accepted: ${JSON.stringify(bad)}`);
    }
    console.log("   invalid durations rejected");

    // A short timer really does resume on its own.
    await c.req("pauseQueue", { durationMs: 2000 });
    await waitFor(async () => (await c.req("getPauseState", {})).paused === false, 15000, "auto-resume");
    await waitFor(async () => (await c.req("getSnapshot", {})).runs.find((r) => r.id === held)?.state === "COMPLETED", 25000, "held run after auto-resume");
    console.log("   timer fired and the held run completed");
  }

  console.log("9) Run Next really runs next…");
  {
    await c.req("pauseQueue", {});
    const { runId: a } = await c.req("enqueueRun", { prompt: "ordinary A", provider: "fake", cwd: HOME });
    const { runId: b } = await c.req("enqueueRun", { prompt: "ordinary B", provider: "fake", cwd: HOME });
    const { runId: urgent } = await c.req("enqueueRun", { prompt: "urgent", provider: "fake", cwd: HOME, runNext: true });
    const s = await c.req("getSnapshot", {});
    const waiting = s.runs.filter((r) => ["QUEUED", "WAITING_FOR_SLOT"].includes(r.state)).map((r) => r.id);
    assert.equal(waiting[0], urgent, `Run Next did not go first: ${JSON.stringify(waiting)}`);
    assert(waiting.indexOf(a) < waiting.indexOf(b), "existing order preserved behind it");
    await c.req("resumeQueue", {});
    await waitFor(async () => (await c.req("getSnapshot", {})).runs.find((r) => r.id === urgent)?.state === "COMPLETED", 25000, "urgent run");
    console.log("   urgent run dispatched ahead of the queue");
    await waitFor(async () => {
      const snap = await c.req("getSnapshot", {});
      return [a, b].every((id) => snap.runs.find((r) => r.id === id)?.state === "COMPLETED");
    }, 30000, "remaining runs");
  }

  console.log("10) cancelling a queued run frees its slot…");
  {
    await c.req("pauseQueue", {});
    const { runId: x } = await c.req("enqueueRun", { prompt: "cancel me", provider: "fake", cwd: HOME });
    const { runId: y } = await c.req("enqueueRun", { prompt: "cancel me too", provider: "fake", cwd: HOME });
    const { runId: survivor } = await c.req("enqueueRun", { prompt: "must still run", provider: "fake", cwd: HOME });
    await c.req("cancelRun", { runId: x });
    await c.req("cancelRun", { runId: y });
    const s = await c.req("getSnapshot", {});
    for (const id of [x, y]) {
      assert.equal(s.runs.find((r) => r.id === id).state, "CANCELLED", "cancelled queued run must settle immediately");
    }
    await c.req("resumeQueue", {});
    await waitFor(async () => (await c.req("getSnapshot", {})).runs.find((r) => r.id === survivor)?.state === "COMPLETED", 25000, "survivor run");
    console.log("   two cancels did not kill the queue");
  }

  console.log("11) deleting the shared block really deletes it…");
  {
    let s = await c.req("getSnapshot", {});
    assert.equal(s.hasSecretBlock, true, "precondition: a block is stored");
    await c.req("deleteSecretBlock", {});
    s = await c.req("getSnapshot", {});
    assert.equal(s.hasSecretBlock, false, "snapshot still reports a block after delete");
    let revealed = true;
    try { await c.req("revealSecretBlock", {}); } catch { revealed = false; }
    assert.equal(revealed, false, "a deleted block is still revealable");

    const { runId: after } = await c.req("enqueueRun", { prompt: "after delete", provider: "fake", attachSecrets: true, cwd: HOME });
    await waitFor(async () => (await c.req("getSnapshot", {})).runs.find((r) => r.id === after)?.state === "COMPLETED", 25000, "post-delete run");
    const eff = await c.req("revealEffectivePrompt", { runId: after });
    assert(!eff.text.includes("E2E-SECRET-hunter42"), "DELETED SECRET STILL SHIPPED TO THE PROVIDER");
    assert(!eff.text.includes("SHARED TESTING / SUDO BLOCK"), "empty secret section still rendered");
    assert.equal(eff.containsSecrets, false);
    console.log("   deleted block is gone from storage and from new prompts");
  }

  console.log("12) queue survives daemon restart…");
  {
    const { runId: r4 } = await c.req("enqueueRun", { prompt: "survives restart", provider: "fake", cwd: HOME });
    c.close();
    daemon.kill("SIGTERM");
    // wait for the old socket to disappear, then start fresh
    await waitFor(() => !existsSync(SOCK), 10000, "old socket removal");
    startDaemon();
    await waitFor(() => new Promise((res) => {
      const probe = net.createConnection({ path: SOCK });
      probe.once("connect", () => { probe.destroy(); res(true); });
      probe.once("error", () => res(false));
    }), 10000, "connectable socket after restart");
    const c2 = new Client();
    await c2.connect();
    await waitFor(async () => (await c2.req("getSnapshot", {})).runs.find((r) => r.id === r4)?.state === "COMPLETED", 25000, "post-restart run");
    const s = await c2.req("getSnapshot", {});
    const kept = s.runs.find((r) => r.id === runId);
    assert.equal(kept.finalOutcome, "COMPLETE");
    const quota = await c2.req("getQuotaState", {});
    assert(quota.claude !== undefined && quota.codex !== undefined);
    console.log("   persisted queue completed after restart; earlier runs intact");
    c2.close();
  }

  daemon.kill("SIGTERM");
  console.log("\nALL E2E CHECKS PASSED");
  rmSync(HOME, { recursive: true, force: true });
}

main().catch((e) => {
  console.error("E2E FAILED:", e.message);
  daemon?.kill("SIGTERM");
  process.exit(1);
});
