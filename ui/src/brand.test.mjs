import assert from "node:assert/strict";
import { readFileSync, readdirSync, statSync, existsSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import react from "@vitejs/plugin-react";
import { build } from "vite";

// The Super Cage mark has one source: the themed masters in brand/. The desktop
// (brand.jsx) and the phone (mobile-cli/doodles.jsx) both draw it through
// BrandMark; nothing else in ui/src may carry a copy of its path data.
const ui = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const masters = { full: join(ui, "../brand/mark-themed.svg"), small: join(ui, "../brand/mark-small-themed.svg") };
const read = (path) => readFileSync(path, "utf8");
const firstPath = (svg) => svg.match(/<path d="([^"]{40})/)[1];
const walk = (dir) => readdirSync(dir).flatMap((name) => {
  const path = join(dir, name);
  return statSync(path).isDirectory() ? walk(path) : [path];
});

test("the masters are the plain filled themed paths BrandMark relies on", () => {
  for (const [cut, path] of Object.entries(masters)) {
    const svg = read(path);
    assert.match(svg, /viewBox="0 0 256 256"/, cut);
    assert.ok(!/\bid=|<style|<script|<use|<g[ >]|stroke/.test(svg), cut + " must be bare filled paths without ids, so two symbols never collide");
    assert.ok(svg.includes('fill="currentColor"'), cut + " ink follows currentColor");
    assert.ok(svg.includes("var(--clay,"), cut + " light follows --clay");
  }
  assert.ok(read(masters.full).includes("var(--clay-soft,"), "the full mark has the glow");
});

test("brand.jsx imports the brand/ masters and the ui keeps no second copy", () => {
  const source = read(join(ui, "src/brand.jsx"));
  assert.match(source, /from "\.\.\/\.\.\/brand\/mark-themed\.svg\?raw"/);
  assert.match(source, /from "\.\.\/\.\.\/brand\/mark-small-themed\.svg\?raw"/);
  assert.ok(!existsSync(join(ui, "src/brand")), "ui/src/brand/ held the old copies");
  const prefixes = Object.values(masters).map((path) => firstPath(read(path)));
  for (const path of walk(join(ui, "src")).filter((name) => /\.(jsx?|mjs|css|svg|html)$/.test(name))) {
    const text = read(path);
    for (const prefix of prefixes) assert.ok(!text.includes(prefix), path + " carries the mark's path data");
  }
  const doodles = read(join(ui, "src/mobile-cli/doodles.jsx"));
  assert.match(doodles, /import \{ BrandMark \} from "\.\.\/brand"/);
  assert.ok(!/^ {2}logo: /m.test(doodles), "the phone's old sparkle cage is retired");
});

test("the phone bundle carries each cut of the mark exactly once", async () => {
  const result = await build({
    root: ui,
    configFile: false,
    publicDir: false,
    logLevel: "silent",
    plugins: [react()],
    // Library mode keeps the entry's exports, so Doodle and Logo are not shaken out.
    build: { write: false, lib: { entry: join(ui, "src/mobile-cli/doodles.jsx"), formats: ["es"] } },
  });
  const chunks = [result].flat().flatMap((item) => item.output).filter((item) => item.type === "chunk");
  const code = chunks.map((chunk) => chunk.code).join("\n");
  for (const [cut, path] of Object.entries(masters)) {
    assert.equal(code.split(firstPath(read(path))).length - 1, 1, cut + " path data in the phone bundle");
  }
  assert.ok(code.includes("sc-mark-small") && code.includes("#sc-mark"), "the phone logo is a reference to the shared symbols");
});
