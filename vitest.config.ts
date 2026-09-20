import { defineConfig } from "vitest/config";
import { resolve } from "path";

// Shared defaults. The per-environment split lives in vitest.workspace.ts so
// that a bare `npx vitest` does the right thing for both suites; previously
// this declared `environment: "node"` globally and only worked because the npm
// scripts passed --environment explicitly (§9).
export default defineConfig({
  resolve: {
    alias: {
      // Extension-host modules import `vscode`, which only exists inside VS
      // Code. The stub lets the real code be tested instead of a copy of it.
      vscode: resolve(__dirname, "tests/support/vscode.ts"),
    },
  },
  test: {
    globals: false,
  },
});
