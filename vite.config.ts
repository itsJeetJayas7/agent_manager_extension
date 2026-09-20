import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { resolve } from "path";

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "dist/webview",
    target: "es2022",
    emptyOutDir: true,
    rollupOptions: {
      input: resolve(__dirname, "webview/src/main.tsx"),
      output: {
        entryFileNames: "webview.js",
        chunkFileNames: "chunks/[name].js",
        assetFileNames: "assets/[name][extname]",
      },
    },
  },
});
