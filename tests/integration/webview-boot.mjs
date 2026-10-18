// Boots the REAL built webview bundle inside the REAL generated host HTML.
//
// This is the check that would have caught the three defects that made the
// shipped extension dead on arrival: the acquireVscodeApi typo, the stylesheet
// that was built but never loaded, and a bundle that cannot execute as a
// classic script. Unit tests cannot catch those; only running the artifact can.
import { JSDOM, VirtualConsole } from "jsdom";
import { readFileSync, existsSync } from "fs";
import { join } from "path";
import assert from "assert";
import { createRequire } from "module";

const require = createRequire(import.meta.url);
const { WEBVIEW_DIR, WEBVIEW_BUNDLE } = require("../../shared/artifacts.cjs");

const root = process.cwd();
const bundlePath = join(root, ...WEBVIEW_DIR.split("/"), WEBVIEW_BUNDLE);
assert(existsSync(bundlePath), `no built bundle at ${bundlePath} — run: node scripts/build.mjs --host-only`);
const bundle = readFileSync(bundlePath, "utf8");

// The host's own HTML generator, compiled on the fly by esbuild so the test
// exercises the shipped implementation rather than a copy of it.
const { buildSync } = await import("esbuild");
const built = buildSync({
  entryPoints: [join(root, "src/extension/webviewHtml.ts")],
  bundle: true,
  format: "cjs",
  platform: "node",
  write: false,
  logLevel: "silent",
});
const mod = { exports: {} };
new Function("module", "exports", "require", built.outputFiles[0].text)(mod, mod.exports, require);
const { webviewHtml } = mod.exports;

const html = webviewHtml({
  scriptUri: "webview.js",
  cspSource: "vscode-resource:",
  mode: "sidebar",
});

console.log("1) host HTML sanity…");
assert(html.includes("acquireVsCodeApi("), "the webview never acquires the VS Code API");
assert(!html.includes("acquireVscodeApi"), "the lower-case typo is back");
assert(!html.includes("<link"), "an external stylesheet the host does not serve");
console.log("   acquireVsCodeApi present, no dangling stylesheet link");

console.log("2) booting the real bundle in the real document…");
const errors = [];
const virtualConsole = new VirtualConsole();
virtualConsole.on("jsdomError", (e) => errors.push(e));
virtualConsole.on("error", (...a) => errors.push(new Error(a.join(" "))));

const dom = new JSDOM(html, {
  runScripts: "outside-only",
  pretendToBeVisual: true,
  url: "https://webview.test/",
  virtualConsole,
});
const { window } = dom;

let posted = [];
window.acquireVsCodeApi = () => ({
  postMessage: (m) => {
    posted.push(m);
    if (m.type === "rpc" && m.method === "usageSummary") {
      window.setTimeout(() => window.dispatchEvent(new window.MessageEvent("message", {
        data: {
          type: "rpcResult", requestId: m.requestId,
          result: {
            runs: 0, completed: 0, failed: 0, activeMs: 0, quotaWaitMs: 0,
            continuations: 0, byProviderActiveMs: {}, recent: null,
            note: "Measured wall-clock time only.",
          },
        },
      })), 0);
    }
  },
  getState: () => ({}),
  setState: () => undefined,
});
// The inline bootstrap, run exactly as the browser would.
window.eval("window.__acVscode = window.__acVscode || acquireVsCodeApi();");
assert(window.__acVscode, "bootstrap did not install the VS Code API bridge");

window.eval(bundle);

console.log("3) React mounted and the design system is present…");
await new Promise((r) => setTimeout(r, 200));

assert.deepEqual(errors.map((e) => String(e)), [], `errors while booting: ${errors.join("; ")}`);

const rootEl = window.document.getElementById("root");
assert(rootEl, "#root missing");
assert(rootEl.children.length > 0, "React did not mount anything into #root");

const style = window.document.querySelector('style[data-agent-conductor="design-system"]');
assert(style, "the design system stylesheet was not injected");
assert(style.textContent.includes("--ac-rhythm-1"), "the injected stylesheet is not the design system");
assert(style.textContent.length > 2000, `injected stylesheet looks truncated (${style.textContent.length} bytes)`);
console.log(`   mounted ${rootEl.children.length} root node(s); ${style.textContent.length} bytes of CSS injected`);

console.log("4) it talks to the host…");
const types = new Set(posted.map((m) => m?.type));
assert(types.has("ready"), "the webview never announced itself, so queued host messages are lost");
assert(types.has("requestSnapshot"), "the webview never asked for a snapshot");
console.log("   posted:", [...types].join(", "));

console.log("5) the queue tab actually rendered…");
const text = rootEl.textContent ?? "";
assert(text.includes("Agent Conductor"), "header missing");
assert(/QUEUE/.test(text), "tab strip missing");
// Styled, not an unstyled shell: the tokens resolve to real classes.
assert(window.document.querySelector(".ac-tabs"), "design-system classes are not applied");
console.log("   header, tabs and design-system classes present");

console.log("6) opening Control with empty usage history…");
window.document.querySelector('[role="tab"][data-tab="CONTROL"]').click();
await new Promise((r) => setTimeout(r, 200));
assert.deepEqual(errors.map((e) => String(e)), [], `errors opening Control: ${errors.join("; ")}`);
assert(rootEl.textContent.includes("0 · 0 completed · 0 needed you"), "empty usage did not render");
assert(rootEl.textContent.includes("Measured wall-clock time only."), "usage response was not rendered");
assert(!rootEl.textContent.includes("Longest runs"), "empty history should not render a run list");
window.document.querySelector('[role="tab"][data-tab="QUEUE"]').click();
await new Promise((r) => setTimeout(r, 100));
assert(window.document.querySelector('[data-tab="QUEUE"][aria-selected="true"]'), "sidebar navigation broke");
console.log("   null recent history renders safely and tab navigation still works");

console.log("\nWEBVIEW BOOT OK");
// React's scheduler and the store's reconciliation timer keep the loop alive.
dom.window.close();
process.exit(0);
