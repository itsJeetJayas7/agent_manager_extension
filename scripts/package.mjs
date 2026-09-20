// Package the VSIX: builds everything first, then runs vsce.
import { spawnSync } from "child_process";
import { writeFileSync, readFileSync } from "fs";
import { join } from "path";

const root = process.cwd();
function run(cmd, args, opts = {}) {
  const r = spawnSync(cmd, args, { stdio: "inherit", ...opts });
  if (r.status !== 0) {
    process.exit(r.status ?? 1);
  }
}

console.log("== building ==");
run("node", ["scripts/build.mjs"]);

console.log("== packaging vsix ==");
// .vscodeignore is honored by vsce
run("npx", ["vsce", "package", "--no-dependencies", "-o", "agent-conductor-0.1.0.vsix"], { shell: process.platform === "win32" });
console.log("packaged agent-conductor-0.1.0.vsix");
