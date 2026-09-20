// Package the VSIX: build everything first, then run vsce.
import { spawnSync } from "child_process";
import { existsSync, statSync } from "fs";
import { join } from "path";
import { createRequire } from "module";

const require = createRequire(import.meta.url);
const { version } = require("../package.json");
const { DAEMON_BIN_DIR, DAEMON_BIN_NAME, SHIPPED_PLATFORMS, daemonBinNameFor } = require("../shared/artifacts.cjs");

const root = process.cwd();
function run(cmd, args, opts = {}) {
  const r = spawnSync(cmd, args, { stdio: "inherit", ...opts });
  if (r.status !== 0) {
    process.exit(r.status ?? 1);
  }
}

const hostOnly = process.argv.includes("--host-only");

console.log("== building ==");
run("node", ["scripts/build.mjs", ...(hostOnly ? ["--host-only"] : [])]);

// Fail before packaging rather than shipping a VSIX that cannot run anywhere
// but the build machine. The README advertises Remote SSH onto a Linux VPS.
const binDir = join(root, ...DAEMON_BIN_DIR.split("/"));
const missing = [];
if (!existsSync(join(binDir, DAEMON_BIN_NAME))) {
  missing.push(DAEMON_BIN_NAME);
}
if (!hostOnly) {
  for (const t of SHIPPED_PLATFORMS) {
    const name = daemonBinNameFor(t.goos, t.goarch);
    if (!existsSync(join(binDir, name))) {
      missing.push(name);
    }
  }
}
if (missing.length > 0) {
  console.error(`missing daemon binaries: ${missing.join(", ")}`);
  process.exit(1);
}

const out = `agent-conductor-${version}.vsix`;
console.log(`== packaging ${out} ==`);
run("npx", ["vsce", "package", "--no-dependencies", "-o", out], { shell: process.platform === "win32" });
console.log(`packaged ${out} (${(statSync(join(root, out)).size / 1024 / 1024).toFixed(1)}MB)`);
