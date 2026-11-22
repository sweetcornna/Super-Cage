import assert from "node:assert/strict";
import { brotliCompressSync, brotliDecompressSync, gunzipSync, gzipSync } from "node:zlib";
import { mkdtemp, mkdir, readFile, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { verifyBuild } from "./verify-build.mjs";
import { precompressBuild } from "./precompress.mjs";

const entry = '<div id="root"></div><script type="module" src="/assets/main-AbCd1234.js"></script><link rel="stylesheet" href="/assets/main-EfGh5678.css">';

async function withBuild(body, { script = "export const valid = true;", style = "body { color: black }" } = {}) {
  const directory = await mkdtemp(join(tmpdir(), "cage-web-integrity-"));
  try {
    await mkdir(join(directory, "assets"));
    await writeFile(join(directory, "index.html"), entry);
    await writeFile(join(directory, "mobile.html"), entry);
    await writeFile(join(directory, "assets/main-AbCd1234.js"), script);
    await writeFile(join(directory, "assets/main-EfGh5678.css"), style);
    await body(directory);
  } finally { await rm(directory, { recursive: true, force: true }); }
}

// Large enough to be worth compressing, and valid as a module.
const largeScript = Array.from({ length: 300 }, (_, i) => `export const answer${i} = 42;`).join("\n") + "\n";
const exists = (path) => stat(path).then(() => true, () => false);

test("build verification rejects corrupted or missing frontend resources", async (t) => {
  for (const problem of ["none", "zero JS", "invalid JS", "zero CSS", "missing script", "missing entry"]) {
    await t.test(problem, async () => {
      const directory = await mkdtemp(join(tmpdir(), "cage-web-integrity-"));
      try {
        await mkdir(join(directory, "assets"));
        await writeFile(join(directory, "index.html"), entry);
        await writeFile(join(directory, "mobile.html"), problem === "missing entry" ? '<div id="root"></div>' : entry);
        if (problem !== "missing script") await writeFile(join(directory, "assets/main-AbCd1234.js"), problem === "zero JS" ? Buffer.alloc(100) : problem === "invalid JS" ? "export const =;" : "export const valid = true;");
        await writeFile(join(directory, "assets/main-EfGh5678.css"), problem === "zero CSS" ? Buffer.alloc(10) : "body { color: black }");
        if (problem !== "none") {
          await assert.rejects(verifyBuild(directory), /(?:NUL bytes|Invalid JavaScript|Missing build resource|Missing entry script)/);
        } else {
          assert.equal(await verifyBuild(directory), 4);
          const manifest = JSON.parse(await readFile(join(directory, "build-integrity.json"), "utf8"));
          assert.match(manifest.files["assets/main-AbCd1234.js"], /^[a-f0-9]{64}$/);
        }
      } finally { await rm(directory, { recursive: true, force: true }); }
    });
  }
});

test("everything under assets/ must be named by content hash", async (t) => {
  for (const name of ["assets/main.js", "assets/worker-processor.js", "assets/short-AbC123.js", "assets/nohash"]) {
    await t.test(name, () => withBuild(async (directory) => {
      await writeFile(join(directory, name), "export const x = 1;");
      await assert.rejects(verifyBuild(directory), /without a content hash/);
    }));
  }
  await t.test("hashed names pass, also in a subdirectory and with a dash in the hash", () => withBuild(async (directory) => {
    await mkdir(join(directory, "assets/fonts"));
    await writeFile(join(directory, "assets/fonts/deep-Qw3rTy9_.woff2"), "font bytes");
    await writeFile(join(directory, "assets/x-Ab-d_f12.js"), "export const x = 1;");
    assert.equal(await verifyBuild(directory), 6);
  }));
  await t.test("files outside assets/ may keep their names", () => withBuild(async (directory) => {
    await writeFile(join(directory, "cage.svg"), "<svg/>");
    assert.equal(await verifyBuild(directory), 5);
  }));
});

test("compressible files get brotli and gzip copies that decode to the file", () => withBuild(async (directory) => {
  const count = await verifyBuild(directory);
  const manifest = JSON.parse(await readFile(join(directory, "build-integrity.json"), "utf8"));
  const script = Buffer.from(largeScript);
  assert.ok(brotliDecompressSync(await readFile(join(directory, "assets/main-AbCd1234.js.br"))).equals(script));
  assert.ok(gunzipSync(await readFile(join(directory, "assets/main-AbCd1234.js.gz"))).equals(script));
  // The copies are part of the integrity manifest the executable checks.
  assert.match(manifest.files["assets/main-AbCd1234.js.br"], /^[a-f0-9]{64}$/);
  assert.match(manifest.files["assets/main-AbCd1234.js.gz"], /^[a-f0-9]{64}$/);
  assert.equal(count, Object.keys(manifest.files).length);
  // Tiny files are served as they are.
  assert.equal(await exists(join(directory, "assets/main-EfGh5678.css.br")), false);
  assert.equal(await exists(join(directory, "index.html.br")), false);
}, { script: largeScript }));

test("only compressible and large enough files are precompressed", () => withBuild(async (directory) => {
  const png = Buffer.alloc(5000, 7);
  await writeFile(join(directory, "assets/logo-AbCd1234.png"), png);
  // Random bytes with a compressible name do not shrink, so no copy is kept.
  const noise = Buffer.from(Array.from({ length: 4000 }, (_, i) => (i * 2654435761) >>> 24));
  await writeFile(join(directory, "assets/noise-AbCd1234.json"), brotliCompressSync(noise));
  await verifyBuild(directory);
  assert.equal(await exists(join(directory, "assets/logo-AbCd1234.png.br")), false);
  assert.equal(await exists(join(directory, "assets/logo-AbCd1234.png.gz")), false);
  assert.equal(await exists(join(directory, "assets/noise-AbCd1234.json.br")), false);
  assert.equal(await exists(join(directory, "assets/noise-AbCd1234.json.gz")), false);
  assert.equal(await exists(join(directory, "assets/main-AbCd1234.js.br")), true);
}, { script: largeScript }));

test("running the verification again leaves the build and manifest unchanged", () => withBuild(async (directory) => {
  await verifyBuild(directory);
  const before = {
    manifest: await readFile(join(directory, "build-integrity.json"), "utf8"),
    br: await readFile(join(directory, "assets/main-AbCd1234.js.br")),
    gz: await readFile(join(directory, "assets/main-AbCd1234.js.gz")),
  };
  const kept = await precompressBuild(directory);
  assert.equal(kept, 2);
  await verifyBuild(directory);
  assert.equal(await readFile(join(directory, "build-integrity.json"), "utf8"), before.manifest);
  assert.ok((await readFile(join(directory, "assets/main-AbCd1234.js.br"))).equals(before.br));
  assert.ok((await readFile(join(directory, "assets/main-AbCd1234.js.gz"))).equals(before.gz));
}, { script: largeScript }));

test("a stale copy is rewritten and a build with a bad copy is refused as found", () => withBuild(async (directory) => {
  await verifyBuild(directory);
  const copy = join(directory, "assets/main-AbCd1234.js.gz");
  // Another file's bytes under this file's name: it must not be kept or served.
  await writeFile(copy, gzipSync(Buffer.from("export const other = 1;\n".repeat(100))));
  await assert.rejects(verifyBuild(directory, { precompress: false }), /Compressed copy differs/);
  await verifyBuild(directory);
  assert.ok(gunzipSync(await readFile(copy)).equals(Buffer.from(largeScript)));
  await writeFile(copy, "not gzip at all");
  await assert.rejects(verifyBuild(directory, { precompress: false }), /Unreadable compressed copy/);
  await verifyBuild(directory);
}, { script: largeScript }));

test("a compressed copy without its file is refused", () => withBuild(async (directory) => {
  await writeFile(join(directory, "assets/gone-AbCd1234.js.br"), brotliCompressSync(Buffer.from("x".repeat(2000))));
  await assert.rejects(verifyBuild(directory), /without its file/);
}));
