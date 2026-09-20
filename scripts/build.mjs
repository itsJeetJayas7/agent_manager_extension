// Full build: daemon binaries, webview bundle, extension bundle.
//
// Output locations come from shared/artifacts.cjs so that the extension host
// and this script can never disagree about where the daemon lives (§1.3, §9).
import { spawnSync } from "child_process";
import { existsSync, mkdirSync, copyFileSync, chmodSync, statSync } from "fs";
import { join } from "path";
import { createRequire } from "module";

const require = createRequire(import.meta.url);
// package.json is the single source of truth for the version; the daemon has
// it stamped in at build time rather than duplicating the literal.
const { version } = require("../package.json");
const {
  DAEMON_BIN_DIR,
  DAEMON_BIN_NAME,
  DAEMON_STAGE_DIR,
  daemonBinNameFor,
  goPlatform,
  SHIPPED_PLATFORMS,
} = require("../shared/artifacts.cjs");

const root = process.cwd();
const hostOnly = process.argv.includes("--host-only");

function run(cmd, args, opts = {}) {
  const r = spawnSync(cmd, args, { stdio: "inherit", ...opts });
  if (r.status !== 0) {
    process.exit(r.status ?? 1);
  }
}

// ---- 1. Go daemon ----------------------------------------------------------
const host = goPlatform(process.platform, process.arch);
const goBin = existsSync("/opt/homebrew/bin/go") ? "/opt/homebrew/bin/go" : "go";
const stageDir = join(root, DAEMON_STAGE_DIR);
mkdirSync(stageDir, { recursive: true });

// The README advertises Remote SSH onto a Linux VPS, so a VSIX built on a Mac
// has to carry a Linux daemon too. modernc.org/sqlite is pure Go, so every
// target cross-compiles with CGO disabled.
const targets = hostOnly
  ? [host]
  : SHIPPED_PLATFORMS.slice().sort((a, b) =>
      a.goos === host.goos && a.goarch === host.goarch ? -1 : b.goos === host.goos && b.goarch === host.goarch ? 1 : 0
    );

for (const t of targets) {
  const out = join(stageDir, daemonBinNameFor(t.goos, t.goarch));
  console.log(`building daemon (${t.goos}-${t.goarch})…`);
  run(goBin, ["build", "-trimpath", "-ldflags", `-s -w -X main.Version=${version}`, "-o", out, "./cmd/agent-conductor"], {
    cwd: join(root, "daemon"),
    env: { ...process.env, GOOS: t.goos, GOARCH: t.goarch, CGO_ENABLED: "0" },
  });
  chmodSync(out, 0o755);
}

// ---- 2. webview ------------------------------------------------------------
console.log("building webview…");
run("npm", ["run", "build:webview"], { shell: process.platform === "win32" });

// ---- 3. extension host bundle ---------------------------------------------
console.log("building extension…");
run("node", ["esbuild.mjs", "--production"]);

// ---- 4. stage daemons where the extension looks for them -------------------
const binDir = join(root, ...DAEMON_BIN_DIR.split("/"));
mkdirSync(binDir, { recursive: true });
for (const t of targets) {
  const src = join(stageDir, daemonBinNameFor(t.goos, t.goarch));
  const dst = join(binDir, daemonBinNameFor(t.goos, t.goarch));
  copyFileSync(src, dst);
  chmodSync(dst, 0o755);
}
// Canonical name resolves first and points at the host build.
const hostBin = join(stageDir, daemonBinNameFor(host.goos, host.goarch));
const canonical = join(binDir, DAEMON_BIN_NAME);
copyFileSync(hostBin, canonical);
chmodSync(canonical, 0o755);

const mb = (p) => (statSync(p).size / 1024 / 1024).toFixed(1);
console.log(`build complete — v${version}, daemon ${mb(canonical)}MB × ${targets.length} target(s) in ${DAEMON_BIN_DIR}/`);
