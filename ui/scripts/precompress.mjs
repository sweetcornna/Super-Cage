import { brotliCompress, brotliDecompressSync, constants, gunzipSync, gzip } from "node:zlib";
import { readFile, readdir, rm, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { promisify } from "node:util";

// Precompressed copies of the UI build. The workbench server (see
// internal/workbench/static_assets.go) answers Accept-Encoding from these
// files instead of compressing per request: assets/main-X.js.br and
// assets/main-X.js.gz sit next to assets/main-X.js and are embedded with it.
// Keep MIN_COMPRESS_BYTES and COMPRESSIBLE in sync with the Go side.
export const MIN_COMPRESS_BYTES = 1024;
export const COMPRESSIBLE = new Set([".html", ".js", ".mjs", ".css", ".json", ".map", ".webmanifest", ".txt", ".xml", ".svg", ".wasm", ".ttf", ".otf"]);

// Everything under assets/ must be named by content hash: the server serves
// it with "immutable" and a year of lifetime. Same rule as hashedStaticAsset in Go.
export const HASHED_ASSET = /^assets\/(?:[^/]+\/)*[^/]+-[A-Za-z0-9_-]{8}(?:\.[A-Za-z0-9]+)+$/;

export const VARIANTS = [
  {
    suffix: ".br",
    encode: (bytes) => promisify(brotliCompress)(bytes, {
      params: {
        [constants.BROTLI_PARAM_QUALITY]: constants.BROTLI_MAX_QUALITY,
        [constants.BROTLI_PARAM_MODE]: constants.BROTLI_MODE_TEXT,
        [constants.BROTLI_PARAM_SIZE_HINT]: bytes.length,
      },
    }),
    decode: brotliDecompressSync,
  },
  { suffix: ".gz", encode: (bytes) => promisify(gzip)(bytes, { level: 9 }), decode: gunzipSync },
];

/** The file a ".br" or ".gz" name is a copy of, or null for any other name. */
export function variantSource(name) {
  const variant = VARIANTS.find(({ suffix }) => name.endsWith(suffix));
  return variant ? name.slice(0, -variant.suffix.length) : null;
}

export function worthCompressing(name, bytes) {
  const dot = name.lastIndexOf(".");
  return bytes.length >= MIN_COMPRESS_BYTES && dot >= 0 && COMPRESSIBLE.has(name.slice(dot).toLowerCase());
}

/** Every file below directory as a sorted, "/"-separated relative name. */
export async function listBuildFiles(directory, relative = "") {
  const names = [];
  for (const entry of await readdir(join(directory, relative), { withFileTypes: true })) {
    const name = [relative, entry.name].filter(Boolean).join("/");
    if (entry.isDirectory()) names.push(...await listBuildFiles(directory, name));
    else names.push(name);
  }
  return names.sort();
}

async function readOptional(path) {
  try { return await readFile(path); } catch (error) { if (error.code === "ENOENT") return null; throw error; }
}

function decodesTo(variant, encoded, source) {
  try { return variant.decode(encoded).equals(source); } catch { return false; }
}

/**
 * Makes every compressible file of the build have a brotli and a gzip copy.
 * A copy that already decodes to the file is kept, so running this again on
 * the same build is cheap and changes nothing; one that is stale is rewritten,
 * and one that would not be smaller than the file is not kept.
 */
export async function precompressBuild(directory) {
  const names = await listBuildFiles(directory);
  const sources = names.filter((name) => variantSource(name) === null && name !== "build-integrity.json");
  let kept = 0;
  await Promise.all(sources.flatMap((name) => VARIANTS.map(async (variant) => {
    const source = await readFile(join(directory, name));
    const target = join(directory, name + variant.suffix);
    const existing = await readOptional(target);
    if (!worthCompressing(name, source)) {
      if (existing) await rm(target);
      return;
    }
    if (existing && existing.length < source.length && decodesTo(variant, existing, source)) { kept++; return; }
    const encoded = await variant.encode(source);
    if (encoded.length >= source.length) {
      if (existing) await rm(target);
      return;
    }
    await writeFile(target, encoded);
    kept++;
  })));
  return kept;
}
