import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
export default defineConfig({
  plugins: [react()],
  base: "/",
  build: {
    outDir: "../internal/workbench/web",
    emptyOutDir: true,
    rolldownOptions: {
      input: { desktop: "index.html", mobile: "mobile.html" },
      // The workbench server serves everything under assets/ as immutable, which
      // is only safe while each name carries an 8-character content hash (the
      // defaults, spelled out here so they cannot drift). verify-build.mjs
      // rejects a build that breaks this.
      output: {
        // Lucide icons in one chunk: split per lazy view they made ~30 files
        // of a few hundred bytes each on the first paint.
        codeSplitting: { groups: [{ name: "icons", test: /node_modules[\\/]lucide-react[\\/]/ }] },
        entryFileNames: "assets/[name]-[hash:8].js",
        chunkFileNames: "assets/[name]-[hash:8].js",
        assetFileNames: "assets/[name]-[hash:8][extname]",
      },
    },
  },
  server: { proxy: { "/api": { target: "http://localhost:17777", ws: true } } },
});
