// What the desktop window downloads before it can draw: the files index.html
// names (entry script, module preloads, stylesheets) and the main chunk with
// the static dependencies the entry preloads for it (the __vite__mapDeps list
// of the entry script). Lazy chunks (a settings section, say) are not part of
// it. Sizes are raw bytes and gzip bytes (the .gz copy precompress.mjs wrote
// next to each file; a file under its size floor has none and counts as is).
//
//   node scripts/first-load-bytes.mjs [web-directory]            print JSON
//   node scripts/first-load-bytes.mjs [web-directory] --save f   also write f
//   node scripts/first-load-bytes.mjs [web-directory] --against f [--max-gzip-growth 1024]
//       compare the main chunk's gzip size with the saved run (design 9.1,
//       "Main chunk": at most +1024 bytes) and exit 1 when it grew more.
import { readFile, stat, writeFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const args = process.argv.slice(2);
const option = (name) => { const at = args.indexOf(name); return at < 0 ? undefined : args.splice(at, 2)[1]; };
const save = option("--save");
const against = option("--against");
const limit = Number(option("--max-gzip-growth") ?? 1024);
const directory = resolve(args[0] ?? fileURLToPath(new URL("../../internal/workbench/web/", import.meta.url)));

const names = new Set();
const html = await readFile(join(directory, "index.html"), "utf8");
for (const match of html.matchAll(/<(?:script|link)\b[^>]*\b(?:src|href)=["']\/(assets\/[^"']+)["']/g)) names.add(match[1]);
const entry = [...names].find((name) => /^assets\/desktop-[^/]+\.js$/.test(name));
if (!entry) throw new Error("index.html names no desktop entry script");
const mapDeps = (await readFile(join(directory, entry), "utf8")).match(/m\.f\|\|\(m\.f=(\[[^\]]*\])\)/);
if (!mapDeps) throw new Error("the entry script has no preload list for the main chunk");
const main = JSON.parse(mapDeps[1])[0];
if (!/^assets\/main-/.test(main)) throw new Error("the first preload entry is not the main chunk: " + main);
for (const name of JSON.parse(mapDeps[1])) names.add(name);

const files = {};
for (const name of [...names].sort()) {
  const raw = (await stat(join(directory, name))).size;
  let gzip = raw;
  try { gzip = (await stat(join(directory, name + ".gz"))).size; } catch { /* below the compression floor */ }
  // The hash is part of the name; the stable key is the name without it.
  files[name.replace(/-[A-Za-z0-9_-]{8}(\.[a-z]+)$/, "$1")] = { name, raw, gzip };
}
const total = (key) => Object.values(files).reduce((sum, file) => sum + file[key], 0);
const result = { main: files[main.replace(/-[A-Za-z0-9_-]{8}(\.[a-z]+)$/, "$1")], raw: total("raw"), gzip: total("gzip"), files };
const text = JSON.stringify(result, null, 2) + "\n";
if (save) await writeFile(save, text);
if (against) {
  const before = JSON.parse(await readFile(against, "utf8"));
  const rows = Object.keys({ ...before.files, ...result.files }).sort().map((key) => {
    const was = before.files[key]?.gzip ?? 0, now = result.files[key]?.gzip ?? 0;
    return { file: key, gzip_before: was, gzip_after: now, change: now - was };
  }).filter((row) => row.change !== 0 || !before.files[row.file] || !result.files[row.file]);
  const growth = result.main.gzip - before.main.gzip;
  console.log(JSON.stringify({ total_gzip_before: before.gzip, total_gzip_after: result.gzip, main_gzip_before: before.main.gzip, main_gzip_after: result.main.gzip, main_growth: growth, limit, changed: rows }, null, 2));
  if (growth > limit) { console.error("the main chunk grew by " + growth + " gzip bytes (limit " + limit + ")"); process.exit(1); }
} else console.log(text);
