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
    const r = spawnSync(BIN, ["hook", "claude"], {
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
    const r2 = spawnSync(BIN, ["hook", "claude"], {
      input: JSON.stringify({ hook_event_name: "Stop", session_id: "e2e-sess", stop_hook_active: true, last_assistant_message: "still no marker" }),
      env: { ...process.env, HOME, AGENT_CONDUCTOR_RUN_ID: runId },
      encoding: "utf8", timeout: 10000,
    });
    assert.equal(r2.stdout.trim(), "", "second stop must be allowed (no infinite loop)");
    console.log("   second stop (stop_hook_active) allowed");
    // external session: no run id, never gated
    const r3 = spawnSync(BIN, ["hook", "claude"], {
      input: JSON.stringify({ hook_event_name: "Stop", session_id: "ext-sess", last_assistant_message: "external chat" }),
      env: { ...process.env, HOME },
      encoding: "utf8", timeout: 10000,
    });
    assert.equal(r3.stdout.trim(), "", "external sessions must never be gated");
    console.log("   external session never gated");
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

  console.log("8) queue survives daemon restart…");
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
