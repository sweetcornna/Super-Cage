// Build only the phone entry and its imported assets into the app bundle.
import { copyFile } from "node:fs/promises";
import { createRequire } from "node:module";
import { fileURLToPath, pathToFileURL } from "node:url";
import { resolve } from "node:path";

const ui = fileURLToPath(new URL("../ui/", import.meta.url));
const outDir = fileURLToPath(new URL("./.build/resources/mobile-ui/", import.meta.url));
const require = createRequire(new URL("../ui/package.json", import.meta.url));
// The phone terminal uses the same IME-patched xterm bundle as the workbench.
await import(pathToFileURL(resolve(ui, "scripts/prepare-xterm.mjs")).href);
const { build } = await import(pathToFileURL(require.resolve("vite")));
const { default: react } = await import(pathToFileURL(require.resolve("@vitejs/plugin-react")));
await build({
  root: ui,
  configFile: false,
  publicDir: false,
  plugins: [react()],
  build: {
    outDir,
    emptyOutDir: true,
    rolldownOptions: { input: resolve(ui, "mobile.html") },
  },
});
await copyFile(resolve(ui, "public/harness-marks-notice.txt"), resolve(outDir, "harness-marks-notice.txt"));
