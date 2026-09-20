// Single source of truth for build-output locations.
//
// Both the build pipeline (scripts/build.mjs) and the extension host
// (src/extension/configuration.ts) read these. They MUST NOT drift: when they
// did, the extension looked in `bin/` while the build wrote `dist/bin/` and the
// daemon was never found (see AGENT_BRIEF §1.3, §9).
//
// CommonJS on purpose: the extension bundle is CJS and Node's ESM loader can
// still import these named exports from scripts/*.mjs.

/** Directory, relative to the extension root, holding the daemon binary. */
exports.DAEMON_BIN_DIR = "dist/bin";

/** Canonical (platform-agnostic) daemon binary name inside DAEMON_BIN_DIR. */
exports.DAEMON_BIN_NAME = "agent-conductor";

/** Directory, relative to the extension root, holding the webview bundle. */
exports.WEBVIEW_DIR = "dist/webview";

/** Webview entry bundle file name inside WEBVIEW_DIR. */
exports.WEBVIEW_BUNDLE = "webview.js";

/** Staging directory for freshly compiled daemon binaries (not shipped). */
exports.DAEMON_STAGE_DIR = "dist-bin";

/**
 * Platform-specific daemon binary name, e.g. `agent-conductor-darwin-arm64`.
 * `goos`/`goarch` use Go's naming (darwin|linux, arm64|amd64).
 */
exports.daemonBinNameFor = function daemonBinNameFor(goos, goarch) {
  return exports.DAEMON_BIN_NAME + "-" + goos + "-" + goarch;
};

/** Map Node's process.platform/process.arch onto Go's GOOS/GOARCH. */
exports.goPlatform = function goPlatform(nodePlatform, nodeArch) {
  return {
    goos: nodePlatform === "darwin" ? "darwin" : "linux",
    goarch: nodeArch === "arm64" ? "arm64" : "amd64",
  };
};

/** Every platform we ship a daemon binary for. */
exports.SHIPPED_PLATFORMS = [
  { goos: "darwin", goarch: "arm64" },
  { goos: "darwin", goarch: "amd64" },
  { goos: "linux", goarch: "amd64" },
  { goos: "linux", goarch: "arm64" },
];
