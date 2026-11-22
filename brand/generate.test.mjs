// node --test brand/
// Checks that need no browser: masters, layout, vector outputs and the structure of committed rasters.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import {
  ANDROID, COLORS, FAVICON_SIZES, LINUX_SIZES, MAC_SIZES, PATHS, SMALL_MAX, WEB_DIRS, WIN_SIZES,
  androidLayout, buildIco, coverage, decodePng, encodePng, loadMasters, pngInfo, textOutputs,
} from "./generate.mjs";

const root = fileURLToPath(new URL("../", import.meta.url));
const read = (path) => readFileSync(join(root, path));
const masters = loadMasters();

test("the tile masters carry the same art as the on-dark marks", () => {
  assert.deepEqual(masters.tile.shapes, masters.fullDark.shapes);
  assert.deepEqual(masters.tileSmall.shapes, masters.smallDark.shapes);
  assert.equal(masters.tile.fill, COLORS.ink);
  assert.deepEqual(masters.full.shapes.map((s) => s.fill), [COLORS.glow, COLORS.ink, COLORS.clay]);
  assert.deepEqual(masters.fullDark.shapes.map((s) => s.fill), [COLORS.glowDark, COLORS.cream, COLORS.clay]);
  assert.deepEqual(masters.fullMono.shapes.map((s) => s.fill), [COLORS.ink, COLORS.ink]);
  assert.deepEqual(masters.small.shapes.map((s) => s.fill), [COLORS.ink, COLORS.clay]);
});

test("final.mjs reproduces every master byte for byte", () => {
  const out = mkdtempSync(join(tmpdir(), "cage-masters-"));
  try {
    execFileSync(process.execPath, [join(root, "brand/final.mjs")], { env: { ...process.env, MASTERS_OUT: out } });
    for (const name of ["mark", "mark-on-dark", "mark-mono", "mark-themed", "mark-small", "mark-small-on-dark", "mark-small-themed", "tile-ink", "tile-ink-small"])
      assert.ok(readFileSync(join(out, name + ".svg")).equals(read("brand/" + name + ".svg")), name + ".svg differs from final.mjs output");
  } finally {
    rmSync(out, { recursive: true, force: true });
  }
});

test("the mark sits inside its tile with room around it", () => {
  for (const small of [false, true]) {
    const c = coverage(masters, small);
    assert.ok(c.height > 0.55 && c.height < 0.7, "height share " + c.height);
    assert.ok(c.top > 0.12 * 256 && c.bottom < 0.88 * 256, "margins " + c.top + " / " + c.bottom);
    assert.ok(Math.abs(c.centreY - 128) < 8, "centre " + c.centreY);
  }
});

test("the Android icon art stays inside the 66 dp safe zone", () => {
  const { frame, reach } = androidLayout(masters);
  assert.equal(frame[0], ANDROID.layer);
  assert.ok(reach <= ANDROID.safe / 2, "reach " + reach);
  assert.ok(reach > 20, "the art should not be tiny: " + reach);
});

test("checked-in vector outputs match the masters", () => {
  const files = textOutputs(masters);
  assert.ok(files.size >= 10);
  for (const [path, text] of files) assert.equal(read(path).toString("utf8"), text, path + " is stale; run node brand/generate.mjs");
  const foreground = files.get(PATHS.drawables + "/cage_icon_foreground.xml");
  assert.doesNotMatch(foreground, /strokeColor/);
  assert.match(foreground, /#FFF3EFE5/);
  assert.match(foreground, /#FF4A3329/);
  assert.match(foreground, /#FFD97757/);
  const mono = files.get(PATHS.drawables + "/cage_icon_monochrome.xml");
  assert.deepEqual([...new Set([...mono.matchAll(/fillColor="([^"]*)"/g)].map((m) => m[1]))], ["#FFFFFFFF"]);
  assert.doesNotMatch(mono, /4A3329|D97757/);
  // aapt2 stores each attribute as one string; stay well under its 32767 byte cap.
  for (const [path, text] of files)
    if (path.endsWith(".xml")) for (const m of text.matchAll(/android:pathData="([^"]*)"/g)) assert.ok(Buffer.byteLength(m[1]) < 32767, path + " has a pathData of " + m[1].length + " bytes");
  const logo = files.get(PATHS.drawables + "/cage_logo.xml");
  assert.match(logo, /\?attr\/cageInk/);
  assert.match(logo, /\?attr\/cageGlow/);
  const ios = files.get(PATHS.catalog + "/Logo.imageset/Logo-dark.svg");
  assert.doesNotMatch(ios, /<g|<circle/);
  assert.match(ios, /fill="#F3EFE5"/);
});

test("Android resources: ink adaptive background and a glow attribute in both themes", () => {
  const themes = read("mobile/android/res/values/themes.xml").toString("utf8");
  assert.match(themes, /<color name="cage_icon_background">#FF1F1E1D<\/color>/);
  assert.match(themes, /<attr name="cageGlow" format="color" \/>/);
  assert.match(themes, /name="cageGlow">#FFF6E5DC</);
  assert.match(themes, /name="cageGlow">#FF4A3329</);
});

function frames(buf) {
  const count = buf.readUInt16LE(4);
  return Array.from({ length: count }, (_, i) => {
    const e = 6 + i * 16, offset = buf.readUInt32LE(e + 12), size = buf.readUInt32LE(e + 8);
    return { size: buf[e] || 256, png: buf.subarray(offset, offset + size) };
  });
}

test("Windows and favicon ICO files hold unique PNG frames of the expected sizes", () => {
  const checks = [[PATHS.winIco, WIN_SIZES], ...WEB_DIRS.map(({ dir }) => [dir + "/favicon.ico", FAVICON_SIZES])];
  for (const [path, sizes] of checks) {
    const ico = frames(read(path));
    assert.deepEqual(ico.map((f) => f.size), sizes, path);
    for (const f of ico) assert.deepEqual([pngInfo(f.png).width, pngInfo(f.png).height, pngInfo(f.png).colorType], [f.size, f.size, 6], path + " " + f.size);
  }
});

test("PNG outputs have the expected size and channels", () => {
  const expect = (path, px, colorType) => {
    const info = pngInfo(read(path));
    assert.deepEqual([info.width, info.height, info.colorType], [px, px, colorType], path);
  };
  for (const [px, name] of MAC_SIZES) expect(PATHS.macIconset + "/" + name + ".png", px, 6);
  expect(PATHS.logo, 1024, 6);
  for (const px of LINUX_SIZES) expect(PATHS.linuxIcons + "/hicolor/" + px + "x" + px + "/apps/" + PATHS.linuxName + ".png", px, 6);
  expect(PATHS.catalog + "/AppIcon.appiconset/AppIcon.png", 1024, 2); // RGB: the App Store rejects an alpha channel
  for (const { dir, png, extra } of WEB_DIRS) {
    expect(dir + "/" + png, 512, 6);
    expect(dir + "/apple-touch-icon.png", 180, 2);
    for (const file of extra) expect(file, 512, 6);
  }
});

test("the icon uses the small cut up to SMALL_MAX and the full mark above", () => {
  assert.equal(SMALL_MAX, 48);
  assert.ok(WIN_SIZES.filter((s) => s <= SMALL_MAX).join() === "16,20,24,32,40,48");
});

test("PNG and ICO writers round-trip", () => {
  const rgba = Buffer.alloc(4 * 4 * 3);
  for (let i = 0; i < rgba.length; i++) rgba[i] = (i * 37) & 255;
  for (let i = 3; i < rgba.length; i += 4) rgba[i] = 255;
  const image = { width: 4, height: 3, rgba };
  assert.ok(decodePng(encodePng(image)).rgba.equals(rgba));
  assert.ok(decodePng(encodePng(image, { alpha: false })).rgba.equals(rgba));
  assert.equal(pngInfo(encodePng(image, { alpha: false })).colorType, 2);
  rgba[3] = 0;
  assert.throws(() => encodePng(image, { alpha: false }), /transparency/);
  const ico = buildIco([{ size: 256, png: Buffer.from("b") }, { size: 16, png: Buffer.from("a") }]);
  assert.deepEqual(frames(ico).map((f) => f.size), [16, 256]);
});
