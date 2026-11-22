import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The phone page as the workbench serves it to phone browsers is an installable web app: these tags take
// the place of mobile.html's PNG favicon. The native apps' copy (mobile/build-ui.mjs, built without this
// config) keeps mobile.html as it is; cage-entry marks the browser page for ui/src/mobile-cli/web-shell.mjs.
const PHONE_FAVICON = '<link rel="icon" href="/src/logo.png" />';
const WEB_PHONE_HEAD = [
  '<meta name="cage-entry" content="phone" />',
  '<meta name="theme-color" content="#faf9f5" media="(prefers-color-scheme: light)" />',
  '<meta name="theme-color" content="#1f1e1d" media="(prefers-color-scheme: dark)" />',
  '<meta name="mobile-web-app-capable" content="yes" />',
  '<meta name="apple-mobile-web-app-capable" content="yes" />',
  '<meta name="apple-mobile-web-app-status-bar-style" content="default" />',
  '<meta name="apple-mobile-web-app-title" content="Super Cage" />',
  '<meta name="format-detection" content="telephone=no" />',
  '<link rel="icon" href="/favicon.ico" sizes="48x48" />',
  '<link rel="icon" type="image/svg+xml" href="/cage.svg" />',
  '<link rel="apple-touch-icon" href="/apple-touch-icon.png" />',
  '<link rel="manifest" href="/manifest.webmanifest" />',
].join("\n    ");
const webPhoneHead = {
  name: "cage-web-phone-head",
  transformIndexHtml: {
    order: "pre",
    handler(html, { path }) {
      if (!path.endsWith("/mobile.html")) return html;
      if (!html.includes(PHONE_FAVICON)) throw new Error("mobile.html no longer links " + PHONE_FAVICON);
      return html.replace(PHONE_FAVICON, WEB_PHONE_HEAD);
    },
  },
};

export default defineConfig({
  plugins: [react(), webPhoneHead],
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
