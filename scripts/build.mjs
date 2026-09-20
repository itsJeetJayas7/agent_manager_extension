// Full build: daemon (host), webview bundle, extension bundle.
import { spawnSync } from "child_process";
import { existsSync, mkdirSync, copyFileSync, cpSync } from "fs";
import { join } from "path";

const root = process.cwd();
function run(cmd, args, opts = {}) {
  const r = spawnSync(cmd, args, { stdio: "inherit", ...opts });
  if (r.status !== 0) {
    process.exit(r.status ?? 1);
  }
}

// 1. Go daemon for the host platform
const host = process.platform === "darwin" ? "darwin" : "linux";
const arch = process.arch === "arm64" ? "arm64" : "amd64";
const goBin = existsSync("/opt/homebrew/bin/go") ? "/opt/homebrew/bin/go" : "go";
const daemonOut = join(root, "dist-bin");
mkdirSync(daemonOut, { recursive: true });
console.log(`building daemon (${host}-${arch})…`);
run(goBin, ["build", "-trimpath", "-ldflags", "-s -w", "-o", join(daemonOut, `agent-conductor-${host}-${arch}`), "./cmd/agent-conductor"], { cwd: join(root, "daemon") });

// 2. webview
console.log("building webview…");
run("npm", ["run", "build:webview"], { shell: process.platform === "win32" });

// 3. extension host bundle
console.log("building extension…");
run("node", ["esbuild.mjs", "--production"]);

// 4. place daemon where the extension expects it
mkdirSync(join(root, "dist", "bin"), { recursive: true });
copyFileSync(join(daemonOut, `agent-conductor-${host}-${arch}`), join(root, "dist", "bin", "agent-conductor"));
console.log("build complete.");
