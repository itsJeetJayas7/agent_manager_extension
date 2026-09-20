// Live provider verification: runs the REAL Claude Code and Codex CLIs
// through the real daemon, real scheduler and real IPC protocol.
//
// Unlike daemon-e2e.mjs (fake provider, always runnable in CI) this test
// costs provider quota and needs both runtimes installed and authenticated,
// so it is opt-in: `npm run test:live`.
//
// What it proves:
//   1. Both runtimes are DISCOVERED and report installed capabilities.
//   2. A real Claude run edits a real file in a real git repo.
//   3. A real Codex run edits the SAME file afterwards.
//   4. The two never ran at the same time — the FIFO queue serialised them,
//      which is the whole point of the product. Each agent signs its line, so
//      the relay file is a human-readable transcript of who did what, when.
//
// Isolation: AC_ROOT points at a scratch directory, so the daemon's database,
// vault and logs never touch ~/.agent-conductor. HOME stays real, because the
// provider CLIs need their real credentials.
import * as net from "net";
import { spawn } from "child_process";
import { mkdtempSync, readFileSync, writeFileSync, existsSync, rmSync, mkdirSync } from "fs";
import { tmpdir, homedir } from "os";
import { join } from "path";
import { execFileSync } from "child_process";
import assert from "assert";

const BIN = join(process.cwd(), "dist", "bin", "agent-conductor");
const SCRATCH = process.env.AC_LIVE_DIR || mkdtempSync(join(tmpdir(), "ac-live-"));
const SANDBOX = join(SCRATCH, "sandbox");
// The daemon root lives in a short path of its own: a Unix socket path is
// capped at ~104 bytes on macOS, and a nested scratch directory blows past it
// with `bind: invalid argument` rather than anything that names the cause.
const ROOT = mkdtempSync(join(tmpdir(), "acd-"));
const RELAY = join(SANDBOX, "RELAY.md");
const SOCK = join(ROOT, "run", "conductor.sock");
const TOKEN_PATH = join(ROOT, "run", "conductor.token");

// A live agent turn is slow. These are ceilings, not expectations.
const RUN_TIMEOUT_MS = Number(process.env.AC_LIVE_RUN_TIMEOUT || 420_000);

const wait = (ms) => new Promise((r) => setTimeout(r, ms));

async function waitFor(fn, timeoutMs, what) {
  const t0 = Date.now();
  let last;
  while (Date.now() - t0 < timeoutMs) {
    last = await fn();
    if (last) return last;
    await wait(500);
  }
  throw new Error(`timeout after ${Math.round((Date.now() - t0) / 1000)}s waiting for ${what}`);
}

class Client {
  constructor() {
    this.buf = "";
    this.next = 1;
    this.pending = new Map();
    this.events = [];
  }
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
        const line = this.buf.slice(0, i);
        this.buf = this.buf.slice(i + 1);
        if (!line.trim()) continue;
        let m;
        try { m = JSON.parse(line); } catch { continue; }
        if (m.event) this.events.push(m);
        else if (m.id && this.pending.has(m.id)) {
          const p = this.pending.get(m.id);
          this.pending.delete(m.id);
          m.error ? p.reject(new Error(JSON.stringify(m.error))) : p.resolve(m.result);
        }
      }
    });
    await this.req("hello", { auth: readFileSync(TOKEN_PATH, "utf8").trim() });
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

const TERMINAL = new Set(["COMPLETED", "FAILED", "CANCELLED", "NEEDS_ATTENTION", "SKIPPED", "BLOCKED", "ORPHANED"]);
// States in which a run is occupying the single managed execution slot.
const ACTIVE = new Set(["PREPARING", "STARTING", "RUNNING", "WAITING_FOR_PROVIDER", "VERIFYING"]);
const WAITING = new Set(["QUEUED", "WAITING_FOR_SLOT", "WAITING_FOR_EXTERNAL_AGENT"]);

function git(...args) {
  return execFileSync("git", args, { cwd: SANDBOX, encoding: "utf8" }).trim();
}

function makeSandbox() {
  rmSync(SANDBOX, { recursive: true, force: true });
  mkdirSync(SANDBOX, { recursive: true });
  // A real git repo, because worktrees, diffs and the agents themselves all
  // behave differently outside one.
  execFileSync("git", ["init", "-q", "-b", "main"], { cwd: SANDBOX });
  execFileSync("git", ["config", "user.email", "live-test@agent-conductor.local"], { cwd: SANDBOX });
  execFileSync("git", ["config", "user.name", "Agent Conductor Live Test"], { cwd: SANDBOX });
  writeFileSync(RELAY, `# Relay log

Each agent appends exactly one numbered line to the list below and signs it.
Do not modify or reorder lines that are already here.

## Log
`);
  writeFileSync(join(SANDBOX, ".gitignore"), "");
  execFileSync("git", ["add", "-A"], { cwd: SANDBOX });
  execFileSync("git", ["commit", "-q", "-m", "baseline"], { cwd: SANDBOX });
}

// relayPrompt asks for an append-only, self-numbering edit. Self-numbering is
// deliberate: if the queue ever let two agents run at once, the second would
// read a stale file and produce a duplicate number or clobber the first line.
function relayPrompt(signature) {
  return [
    `Open the file RELAY.md in this directory.`,
    ``,
    `Under the "## Log" heading, append exactly ONE new line at the end of the file, in this format:`,
    ``,
    `- [N] <ISO-8601 UTC timestamp of right now> (by ${signature})`,
    ``,
    `where N is one greater than the highest number already present in the log (use 1 if the log is empty).`,
    ``,
    `Rules:`,
    `- Do not change, delete or reorder any existing line.`,
    `- Do not create any other file and do not commit anything.`,
    `- Do not run any tests or builds; this is a one-line edit.`,
    `- The literal text "(by ${signature})" must appear at the end of your line.`,
  ].join("\n");
}

// Trust is stored in the user's real ~/.claude.json, because that is the file
// Claude Code reads. The test's sandbox is a throwaway directory, so its entry
// is removed again rather than left to accumulate on every run.
function untrustSandbox() {
  const path = join(homedir(), ".claude.json");
  try {
    const doc = JSON.parse(readFileSync(path, "utf8"));
    if (!doc.projects || !(SANDBOX in doc.projects)) return;
    delete doc.projects[SANDBOX];
    writeFileSync(path, JSON.stringify(doc, null, 2) + "\n", { mode: 0o600 });
    console.log("   removed the sandbox's trust entry from ~/.claude.json");
  } catch {
    // Never let cleanup of a throwaway entry fail the run.
  }
}

let daemon;
function startDaemon() {
  daemon = spawn(BIN, ["serve"], {
    // Real HOME: the provider CLIs need their real credentials. AC_ROOT keeps
    // the daemon's own state out of the user's install.
    env: { ...process.env, AC_ROOT: ROOT },
    stdio: ["ignore", "pipe", "pipe"],
  });
  daemon.stderr.on("data", (d) => process.stderr.write(`[daemon] ${d}`));
}

function readRelay() {
  return existsSync(RELAY) ? readFileSync(RELAY, "utf8") : "";
}

function signedLines(text, signature) {
  return text.split("\n").filter((l) => l.includes(`(by ${signature})`));
}

async function main() {
  mkdirSync(ROOT, { recursive: true });
  makeSandbox();
  console.log(`sandbox:      ${SANDBOX}`);
  console.log(`daemon root:  ${ROOT}\n`);

  console.log("1) starting the real daemon (no mock providers)…");
  startDaemon();
  await waitFor(() => existsSync(SOCK) && existsSync(TOKEN_PATH), 20_000, "daemon socket");
  const c = new Client();
  await c.connect();
  const health = await c.req("daemonHealth", {});
  console.log(`   daemon ${health.version}, protocol ${health.protocol}\n`);

  console.log("2) provider discovery — both runtimes must report installed…");
  const caps = await c.req("getProviderCapabilities", {});
  for (const p of ["claude", "codex"]) {
    const cp = caps[p];
    assert(cp, `no capability record for ${p}`);
    assert.equal(cp.installed, true,
      `${p} reported NOT installed (notes: ${JSON.stringify(cp.notes || [])})`);
    const models = (cp.models || []).length;
    console.log(`   ${p.padEnd(7)} ${String(cp.version || "installed").padEnd(28)} auth=${cp.authState} models=${models} realtimeQuota=${!!cp.realTimeQuota}`);
    if (cp.notes?.length) cp.notes.forEach((n) => console.log(`           note: ${n}`));
  }
  console.log();

  console.log("3) enqueuing both runs at once (claude first, then codex)…");
  // Pausing first guarantees both rows exist before anything dispatches, so
  // the ordering assertion below is about the queue and not about a race
  // between two enqueue calls.
  await c.req("pauseQueue", { reason: "stage both runs" });
  const { runId: claudeRun } = await c.req("enqueueRun", {
    prompt: relayPrompt("claude"), provider: "claude", permission: "bypass",
    cwd: SANDBOX, title: "relay-claude",
  });
  // Claude Code's headless mode skips its own trust dialog, so the daemon
  // parks an untrusted run and asks. Answering now — the daemon only accepts a
  // folder a queued run actually uses, which is why this follows the enqueue —
  // keeps the queue's own ordering as the only thing under test below. The
  // handler in step 4 still covers the case where this did not take.
  await c.req("trustWorkspace", { path: SANDBOX });
  const { runId: codexRun } = await c.req("enqueueRun", {
    prompt: relayPrompt("codex"), provider: "codex", permission: "bypass",
    cwd: SANDBOX, title: "relay-codex",
  });
  console.log(`   claude run: ${claudeRun}`);
  console.log(`   codex run:  ${codexRun}`);

  const staged = await c.req("getSnapshot", {});
  const order = staged.runs
    .filter((r) => !TERMINAL.has(r.state))
    .sort((a, b) => (a.sortKey ?? 0) - (b.sortKey ?? 0))
    .map((r) => r.id);
  assert.deepEqual(order, [claudeRun, codexRun], `queue order wrong: ${JSON.stringify(order)}`);
  console.log("   both rows queued, claude ahead of codex\n");

  console.log("4) releasing the queue and watching for overlap…");
  await c.req("resumeQueue", {});

  // The core assertion: poll continuously and record every moment two managed
  // runs were simultaneously executing. There must be none.
  const overlaps = [];
  const seenStates = { [claudeRun]: new Set(), [codexRun]: new Set() };
  let trusted = false;

  const settled = await waitFor(async () => {
    const snap = await c.req("getSnapshot", {});
    const rows = snap.runs.filter((r) => r.id === claudeRun || r.id === codexRun);
    for (const r of rows) seenStates[r.id].add(r.state);

    const active = rows.filter((r) => ACTIVE.has(r.state));
    if (active.length > 1) overlaps.push(active.map((r) => `${r.id}:${r.state}`).join(" + "));

    // Claude's headless mode skips its own trust dialog, so the daemon parks
    // the run and asks. Answer it the way the sidebar button does.
    const untrusted = rows.find((r) => r.errorCode === "WORKSPACE_UNTRUSTED");
    if (untrusted && !trusted) {
      trusted = true;
      await c.req("trustWorkspace", { path: SANDBOX });
      await c.req("continueRun", { runId: untrusted.id });
      console.log("   workspace trust requested by the daemon and granted");
      // The parked run still reads NEEDS_ATTENTION in this tick's snapshot;
      // keep polling rather than mistaking it for a terminal result.
      return null;
    }

    return rows.length === 2 && rows.every((r) => TERMINAL.has(r.state)) ? rows : null;
  }, RUN_TIMEOUT_MS, "both live runs to reach a terminal state");

  for (const r of settled) {
    const label = r.id === claudeRun ? "claude" : "codex";
    console.log(`   ${label.padEnd(7)} ${r.state}  outcome=${r.finalOutcome || "-"}  session=${(r.providerSessionId || "none").slice(0, 24)}`);
    if (r.errorMsg) console.log(`           error: ${r.errorMsg}`);
  }
  console.log();

  console.log("5) queue serialisation…");
  assert.equal(overlaps.length, 0,
    `two managed runs executed concurrently: ${JSON.stringify(overlaps.slice(0, 5))}`);
  // The second run must have been observed waiting, not merely finished late.
  const codexWaited = [...seenStates[codexRun]].some((s) => WAITING.has(s));
  assert(codexWaited, `codex never waited for a slot; observed ${JSON.stringify([...seenStates[codexRun]])}`);
  console.log(`   never more than one run executing`);
  console.log(`   codex observed waiting for the slot: ${JSON.stringify([...seenStates[codexRun]])}\n`);

  console.log("6) both agents really edited the shared file…");
  const relay = readRelay();
  const claudeLines = signedLines(relay, "claude");
  const codexLines = signedLines(relay, "codex");
  assert.equal(claudeLines.length, 1, `expected exactly one claude line, got ${claudeLines.length}`);
  assert.equal(codexLines.length, 1, `expected exactly one codex line, got ${codexLines.length}`);

  // Self-numbering is the real proof. Two agents editing the same file under a
  // working queue produce [1] then [2]; concurrent execution against a stale
  // read produces two [1]s or loses one line entirely.
  const numbers = relay.split("\n")
    .map((l) => /^-\s*\[(\d+)\]/.exec(l.trim()))
    .filter(Boolean)
    .map((m) => Number(m[1]));
  assert.deepEqual(numbers, [1, 2], `log is not a clean 1,2 sequence: ${JSON.stringify(numbers)}`);

  // The file order must match the order the daemon actually ran them in, which
  // is what "the second agent built on the first's result" means.
  const byStart = [...settled].sort((a, b) => Date.parse(a.startedAt) - Date.parse(b.startedAt));
  const ranFirst = byStart[0].id === claudeRun ? "claude" : "codex";
  const firstSigned = relay.indexOf(signedLines(relay, ranFirst)[0]);
  const secondSigned = relay.indexOf(signedLines(relay, ranFirst === "claude" ? "codex" : "claude")[0]);
  assert(firstSigned < secondSigned,
    `${ranFirst} ran first but its line is not first — the second agent overwrote rather than appended`);
  console.log(`   execution order: ${byStart.map((r) => (r.id === claudeRun ? "claude" : "codex")).join(" -> ")}`);
  console.log("   both signatures present, numbered 1 then 2, neither overwrote the other\n");

  console.log("7) the daemon's own view of the work (diff + usage)…");
  for (const [label, id] of [["claude", claudeRun], ["codex", codexRun]]) {
    try {
      const diff = await c.req("runDiff", { runId: id });
      const files = diff.files?.length ?? 0;
      console.log(`   ${label.padEnd(7)} runDiff: ${files} file(s) ${diff.truncated ? "(truncated)" : ""}`);
    } catch (e) {
      console.log(`   ${label.padEnd(7)} runDiff unavailable: ${e.message}`);
    }
  }
  const usage = await c.req("usageSummary", {});
  console.log(`   usageSummary: ${JSON.stringify(usage).slice(0, 300)}`);
  const quota = await c.req("getQuotaState", {});
  console.log(`   quota:        ${JSON.stringify(quota).slice(0, 300)}\n`);

  console.log("8) native sessions were kept for inspection…");
  for (const [label, r] of [["claude", settled.find((x) => x.id === claudeRun)], ["codex", settled.find((x) => x.id === codexRun)]]) {
    assert(r.providerSessionId, `${label} run kept no native session id`);
    console.log(`   ${label.padEnd(7)} ${r.providerSessionId}`);
  }
  console.log();

  console.log("--- RELAY.md ---");
  console.log(relay.trimEnd());
  console.log("----------------\n");
  console.log("git status in the sandbox:");
  console.log(git("status", "--short") || "  (clean)");
  console.log();

  const bad = settled.filter((r) => r.state !== "COMPLETED");
  assert.equal(bad.length, 0, `runs did not complete: ${bad.map((r) => `${r.id}=${r.state}`).join(", ")}`);

  c.close();
  untrustSandbox();
  console.log("ALL LIVE PROVIDER CHECKS PASSED");
}

main()
  .then(() => { daemon?.kill("SIGTERM"); process.exit(0); })
  .catch((e) => {
    console.error("\nLIVE TEST FAILED:", e.message);
    try {
      const log = join(ROOT, "logs", "daemon.jsonl");
      if (existsSync(log)) {
        const lines = readFileSync(log, "utf8").trim().split("\n").slice(-25);
        console.error("\nlast daemon log lines:\n" + lines.join("\n"));
      }
    } catch {}
    console.error(`\nsandbox left in place for inspection: ${SANDBOX}`);
    untrustSandbox();
    daemon?.kill("SIGTERM");
    process.exit(1);
  });
