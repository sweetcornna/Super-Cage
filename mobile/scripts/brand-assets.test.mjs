// node --test mobile/scripts/brand-assets.test.mjs
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { androidVector, INK, iosSvg, outputs, parseSvg } from "./brand-assets.mjs";

const sample = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 90" fill="none" stroke="currentColor" stroke-linecap="round" stroke-linejoin="round">
<path d="M1 2c3 4 5 6 7 8" stroke-width="2.3" opacity=".6"/>
<path d="M9 9l1 1z" fill="var(--clay, #D97757)" fill-opacity=".9" stroke="var(--clay, #D97757)" stroke-width="1.1"/></svg>`;

test("parses inherited stroke, ink and clay paints", () => {
  const svg = parseSvg(sample);
  assert.equal(svg.width, 120);
  assert.equal(svg.height, 90);
  assert.deepEqual(svg.paths.map((p) => [p.fill, p.stroke, p.cap, p.strokeAlpha]), [
    [null, "ink", "round", 0.6],
    ["#D97757", "#D97757", "round", 1],
  ]);
  assert.equal(svg.paths[1].fillAlpha, 0.9);
});

test("Android vectors take the ink from the theme and keep clay fixed", () => {
  const xml = androidVector(parseSvg(sample));
  assert.match(xml, /android:viewportWidth="120" android:viewportHeight="90"/);
  assert.match(xml, /android:strokeColor="\?attr\/cageInk"[\s\S]*android:strokeAlpha="0.6"/);
  assert.match(xml, /android:fillColor="#FFD97757"\s+android:fillAlpha="0.9"/);
  assert.doesNotMatch(xml, /currentColor|var\(/);
  const icon = androidVector(parseSvg(sample), { ink: "#FF1F1E1D", frame: [108, 0.5, 24, 24] });
  assert.match(icon, /android:viewportWidth="108"[\s\S]*<group android:scaleX="0.5"/);
  assert.doesNotMatch(icon, /\?attr/);
});

test("iOS variants differ only in ink", () => {
  const svg = parseSvg(sample);
  const light = iosSvg(svg, INK.light), dark = iosSvg(svg, INK.dark);
  assert.match(light, /stroke="#1F1E1D"[^>]*stroke-opacity="0.6"/);
  assert.match(dark, /stroke="#F3EFE5"/);
  assert.equal(light.replaceAll(INK.light, INK.dark), dark);
  assert.match(dark, /fill="#D97757" fill-opacity="0.9" stroke="#D97757"/);
});

test("Android vectors can map fixed paints to theme attributes and carry their own provenance note", () => {
  const svg = parseSvg('<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 8 8" fill="none"><path d="M0 0h8v8z" fill="#F6E5DC"/><path d="M1 1h2v2z" fill="#D97757"/></svg>');
  const xml = androidVector(svg, { colors: { "#F6E5DC": "?attr/cageGlow" }, note: "made elsewhere" });
  assert.match(xml, /<!-- made elsewhere -->/);
  assert.match(xml, /android:fillColor="\?attr\/cageGlow"/);
  assert.match(xml, /android:fillColor="#FFD97757"/);
});

test("checked-in native assets match the brand SVGs", () => {
  const files = outputs();
  assert.ok([...files.keys()].some((path) => path.endsWith("DoodleSun.imageset/DoodleSun-dark.svg")));
  assert.ok(![...files.keys()].some((path) => /Logo|AppIcon|cage_logo|cage_icon_/.test(path)), "brand/generate.mjs owns the logo and icons");
  assert.ok(![...files.keys()].some((path) => path.endsWith("DoodleSquiggle-dark.svg")), "clay-only art needs no dark variant");
  for (const [path, text] of files) assert.equal(readFileSync(path, "utf8"), text, path + " is stale");
});
