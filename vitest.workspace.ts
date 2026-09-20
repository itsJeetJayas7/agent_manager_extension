import { defineWorkspace } from "vitest/config";

export default defineWorkspace([
  {
    extends: "./vitest.config.ts",
    test: {
      name: "extension",
      include: ["tests/extension/**/*.test.ts"],
      environment: "node",
    },
  },
  {
    extends: "./vitest.config.ts",
    test: {
      name: "webview",
      include: ["tests/webview/**/*.test.{ts,tsx}"],
      environment: "jsdom",
    },
  },
]);
