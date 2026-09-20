import { defineConfig, Plugin } from "vite";
import react from "@vitejs/plugin-react";
import { resolve } from "path";

/**
 * Fold the emitted stylesheet into the JS bundle.
 *
 * The extension host serves one script and nothing else; a separate
 * `assets/main.css` was emitted but never linked, so the whole design system
 * was inert (AGENT_BRIEF §1.2). Inlining gives one artifact, no CSP juggling
 * and no flash of unstyled content.
 */
function inlineCss(): Plugin {
  return {
    name: "agent-conductor:inline-css",
    apply: "build",
    enforce: "post",
    generateBundle(_options, bundle) {
      let css = "";
      for (const [fileName, asset] of Object.entries(bundle)) {
        if (asset.type === "asset" && fileName.endsWith(".css")) {
          css += typeof asset.source === "string" ? asset.source : Buffer.from(asset.source).toString("utf8");
          delete bundle[fileName];
        }
      }
      if (!css) {
        return;
      }
      const entry = Object.values(bundle).find(
        (c): c is import("rollup").OutputChunk => c.type === "chunk" && c.isEntry
      );
      if (!entry) {
        this.error("inline-css: no entry chunk to inline the stylesheet into");
        return;
      }
      const injector =
        `(function(){try{` +
        `var s=document.createElement("style");` +
        `s.setAttribute("data-agent-conductor","design-system");` +
        `s.textContent=${JSON.stringify(css)};` +
        `document.head.appendChild(s);` +
        `}catch(e){console.error("agent-conductor: stylesheet injection failed",e);}})();\n`;
      entry.code = injector + entry.code;
    },
  };
}

export default defineConfig({
  plugins: [react(), inlineCss()],
  build: {
    outDir: "dist/webview",
    target: "es2022",
    emptyOutDir: true,
    cssCodeSplit: false,
    rollupOptions: {
      input: resolve(__dirname, "webview/src/main.tsx"),
      output: {
        // Loaded by a classic <script nonce src=...> tag, so it must not be an
        // ES module.
        format: "iife",
        inlineDynamicImports: true,
        entryFileNames: "webview.js",
        assetFileNames: "assets/[name][extname]",
      },
    },
  },
});
