import { defineConfig } from "vitest/config";
import { resolve } from "path";

// Extension and webview projects share the VS Code stub and keep their own environments.
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
    projects: [
      { extends: true, test: { name: "extension", include: ["tests/extension/**/*.test.ts"], environment: "node" } },
      { extends: true, test: { name: "webview", include: ["tests/webview/**/*.test.{ts,tsx}"], environment: "jsdom" } },
    ],
  },
});
