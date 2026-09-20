// OPT-IN real-provider smoke test (consumes a tiny amount of allowance).
// Verifies: subscription runtime launch, native session creation, hook ID
// capture, result marker completion, and same-session resume availability.
// Usage: node tests/integration/smoke-claude.mjs
import { spawn } from "child_process";
import { existsSync, readFileSync } from "fs";
import net from "net";
import { join } from "path";

const BIN = join(process.cwd(), "dist", "bin", "agent-conductor");
const ROOT = process.env.AC_SMOKE_ROOT ?? join(process.env.HOME, ".agent-conductor-smoke");
const SOCK = join(ROOT, "run", "conductor.sock");

const wait = (ms) => new Promise((r) => setTimeout(r, ms));

function client(sock) {
  return new Promise((resolve, reject) => {
    const conn = net.createConnection({ path: sock });
    let buf = "";
    let next = 1;
    const pending = new Map();
    conn.on("connect", () => {
      const tok = readFileSync(join(ROOT, "run", "conductor.token"), "utf8").trim();
      const api = {
        req(method, params = {}) {
          return new Promise((res2, rej2) => {
            const id = String(next++);
            pending.set(id, { resolve: res2, reject: rej2 });
            conn.write(JSON.stringify({ protocol: 1, id, method, params }) + "\n");
            setTimeout(() => rej2(new Error(`timeout ${method}`)), 240000);
          });
        },
        close: () => conn.end(),
      };
      conn.write(JSON.stringify({ protocol: 1, id: "0", method: "hello", params: { auth: tok } }) + "\n");
      resolve(api);
    });
    conn.on("error", reject);
    conn.on("data", (c) => {
      buf += c.toString();
      let i;
      while ((i = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, i);
        buf = buf.slice(i + 1);
        if (!line.trim()) continue;
        const m = JSON.parse(line);
        if (m.id && pending.has(m.id)) {
          const p = pending.get(m.id);
          pending.delete(m.id);
          m.error ? p.reject(new Error(JSON.stringify(m.error))) : p.resolve(m.result);
        }
      }
    });
  });
}

const daemon = spawn(BIN, ["serve"], { env: { ...process.env, AC_ROOT: ROOT }, stdio: ["ignore", "inherit", "inherit"] });
try {
  for (let i = 0; i < 40 && !existsSync(SOCK); i++) {
    await wait(500);
  }
  if (!existsSync(SOCK)) {
    throw new Error("smoke daemon socket never appeared");
  }
  const c = await client(SOCK);
  console.log("[smoke] daemon up");

  const { runId } = await c.req("enqueueRun", {
    prompt: "Do not modify files. Reply with exactly one sentence confirming you received this, then end with the required completion marker on its own line.",
    provider: "claude",
    model: "haiku",
    effort: "low",
    permission: "bypass",
    cwd: process.env.HOME,
  });
  console.log("[smoke] enqueued", runId);

  let run;
  for (let i = 0; i < 240; i++) {
    await wait(2000);
    const snap = await c.req("getSnapshot", {});
    run = snap.runs.find((r) => r.id === runId);
    if (run && ["COMPLETED", "FAILED", "NEEDS_ATTENTION", "BLOCKED"].includes(run.state)) {
      break;
    }
  }
  if (run.state === "NEEDS_ATTENTION" && run.errorCode === "WORKSPACE_UNTRUSTED") {
    console.log("[smoke] workspace untrusted — trusting via explicit API action, then continuing");
    await c.req("trustWorkspace", { path: process.env.HOME });
    await c.req("continueRun", { runId });
    for (let i = 0; i < 240; i++) {
      await wait(2000);
      const snap = await c.req("getSnapshot", {});
      run = snap.runs.find((r) => r.id === runId);
      if (run && ["COMPLETED", "FAILED", "NEEDS_ATTENTION", "BLOCKED"].includes(run.state)) {
        break;
      }
    }
  }
  console.log("[smoke] final state:", run.state);
  console.log("[smoke] outcome:", run.finalOutcome || run.errorMsg);
  console.log("[smoke] native claude session id:", run.providerSessionId);
  if (!run.providerSessionId) {
    throw new Error("no native session id captured");
  }
  if (run.state !== "COMPLETED") {
    throw new Error(`smoke run ended in ${run.state}: ${run.errorMsg}`);
  }
  console.log("[smoke] PASS — session resumable in the official Claude Code extension via:");
  console.log(`        claude --resume ${run.providerSessionId}`);
  c.close();
} finally {
  daemon.kill("SIGTERM");
}
