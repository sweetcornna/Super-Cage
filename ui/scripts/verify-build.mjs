import { createHash } from "node:crypto";
import { readFile, stat, writeFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";
import { HASHED_ASSET, VARIANTS, listBuildFiles, precompressBuild, variantSource } from "./precompress.mjs";

const defaultDirectory = fileURLToPath(new URL("../../internal/workbench/web/", import.meta.url));
// precompress: write the .br/.gz copies the server prefers before checking.
// Tests of the checks themselves turn it off to present a build as found.
export async function verifyBuild(directory = defaultDirectory, { precompress = true } = {}) {
  if (precompress) await precompressBuild(directory);
  const files = {};
  const names = await listBuildFiles(directory);
  const present = new Set(names);
  for (const name of names) {
    if (name === "README" || name === "build-integrity.json") continue;
    const bytes = await readFile(join(directory, name));
    if (bytes.length === 0) throw new Error(`Empty build resource: ${name}`);
    const source = variantSource(name);
    if (source !== null) {
      // A compressed copy is served in place of its file, so it must decode to
      // exactly that file; one without a file would never be asked for.
      if (!present.has(source)) throw new Error(`Compressed copy without its file: ${name}`);
      const variant = VARIANTS.find(({ suffix }) => name.endsWith(suffix));
      let decoded;
      try { decoded = variant.decode(bytes); } catch (error) { throw new Error(`Unreadable compressed copy: ${name}`, { cause: error }); }
      if (!decoded.equals(await readFile(join(directory, source)))) throw new Error(`Compressed copy differs from its file: ${name}`);
    } else {
      if (name.startsWith("assets/") && !HASHED_ASSET.test(name)) throw new Error(`Build resource without a content hash in its name: ${name}`);
      if (/\.(?:js|css|html)$/.test(name)) {
        if (bytes.includes(0)) throw new Error(`NUL bytes in build resource: ${name}`);
        if (name.endsWith(".js")) {
          const parsed = spawnSync(process.execPath, ["--check", "--input-type=module"], { input: bytes, encoding: "utf8" });
          if (parsed.error) throw parsed.error;
          if (parsed.status !== 0) throw new Error(`Invalid JavaScript build resource: ${name}\n${parsed.stderr}`);
        }
      }
    }
    files[name] = createHash("sha256").update(bytes).digest("hex");
  }
  for (const entry of ["index.html", "mobile.html"]) {
    const html = await readFile(join(directory, entry), "utf8");
    const scripts = [...html.matchAll(/<script\b[^>]*\bsrc=["']([^"']+)["']/gi)];
    if (!scripts.length) throw new Error(`Missing entry script: ${entry}`);
    for (const match of html.matchAll(/<(?:script|link)\b[^>]*\b(?:src|href)=["']([^"']+)["']/gi)) {
      const url = new URL(match[1], "http://cage.local/" + entry);
      if (url.origin !== "http://cage.local") throw new Error(`External build resource: ${match[1]}`);
      const resource = decodeURIComponent(url.pathname).slice(1);
      if (!files[resource] || !(await stat(join(directory, resource))).isFile()) throw new Error(`Missing build resource: ${resource}`);
    }
  }
  await writeFile(join(directory, "build-integrity.json"), JSON.stringify({ version: 1, files }, null, 2) + "\n");
  return Object.keys(files).length;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const count = await verifyBuild(process.argv[2] ? resolve(process.argv[2]) : defaultDirectory);
  console.log(`Verified ${count} frontend build resources.`);
}
