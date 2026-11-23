#!/usr/bin/env node
// Super Cage brand generator: one script that regenerates every logo and app icon
// from the master SVGs in this directory (the masters themselves come from brand/final.mjs).
//
//   node brand/generate.mjs           write every output (see brand/README.md)
//   node brand/generate.mjs --check   exit 1 when a checked-in output differs from a fresh render
//   node brand/generate.mjs --check-text   the same for SVG, XML and JSON outputs only; needs no browser
//                                     (mobile/build.sh runs it)
//
// Needs Node 22+, and for the PNG outputs a Chrome/Chromium reachable through Playwright:
//   CAGE_PLAYWRIGHT_MODULE=<playwright module path>  (default: "playwright")
//   CAGE_TEST_BROWSER_PATH=<Chrome executable>       (default: Playwright's own browser)
// macOS only, optional: iconutil validates the committed .iconset after it is written.
//
// App icon = ink tile + cream mark + clay light (+ dark glow behind the ribs at large sizes).
// Sizes up to 48 px use the small cut. The composition of the mark inside the tile (scale and
// vertical offset) is read from tile-ink.svg / tile-ink-small.svg, so the design lead's tiles
// stay the single source; other tile shapes only scale that composition.
import { readFileSync, writeFileSync, mkdirSync, existsSync, mkdtempSync, rmSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { execFileSync } from "node:child_process";
import { tmpdir } from "node:os";
import { deflateSync, inflateSync } from "node:zlib";
import { androidVector, info, iosSvg, json, parseSvg } from "../mobile/scripts/brand-assets.mjs";

const root = fileURLToPath(new URL("../", import.meta.url));
const brandDir = join(root, "brand");

export const COLORS = { ink: "#1F1E1D", clay: "#D97757", cream: "#F3EFE5", glow: "#F6E5DC", glowDark: "#4A3329" };
export const SMALL_MAX = 48; // px at or below which the small cut is used
// Tile shapes in the 256 unit canvas of tile-ink.svg: the tile square and whether it is rounded.
export const TILES = {
  bleed: { inset: 0, side: 256, round: true }, // fills the canvas: Windows, Linux, favicons, PNG logos
  mac: { inset: 25, side: 206, round: true }, // 824/1024 tile with margin: macOS
  square: { inset: 0, side: 256, round: false }, // opaque square: iOS, apple-touch-icon
  // Opaque square for the web manifest's "maskable" icon: the mark shrunk around the centre so all of it
  // stays inside the circle of radius 40 % that Android may cut the icon to.
  maskable: { inset: 0, side: 256, round: false, shrink: 0.82 },
};
export const ANDROID = { layer: 108, visible: 72, safe: 66 }; // dp

const fmt = (n) => String(Math.round(n * 1000) / 1000);
const fmt5 = (n) => String(Math.round(n * 100000) / 100000);

// ---------------------------------------------------------------- masters
const attributes = (text) => Object.fromEntries([...text.matchAll(/([\w:-]+)="([^"]*)"/g)].map((m) => [m[1], m[2]]));

// The masters are plain filled paths: <path d=... fill=.../> in paint order.
export function parseShapes(text, name = "master") {
  const root = /<svg\b([^>]*)>/.exec(text);
  if (!root || attributes(root[1]).viewBox !== "0 0 256 256") throw new Error(name + ": viewBox must be 0 0 256 256");
  const bad = text.match(/<(circle|ellipse|line|polyline|polygon|image|use|text)\b|\bstroke=/);
  if (bad) throw new Error(name + ": only filled <path> shapes are supported (found " + bad[0] + ")");
  const shapes = [...text.matchAll(/<path\b([^>]*?)\/?>/g)].map((m) => {
    const a = attributes(m[1]);
    if (!a.d || !a.fill) throw new Error(name + ": every path needs d and fill");
    return { d: a.d, fill: a.fill };
  });
  if (!shapes.length) throw new Error(name + ": no shapes");
  return shapes;
}

// tile-ink*.svg: rounded rect + <g transform="translate(128 ty) scale(s) translate(-128 -128)">.
export function parseTile(text, name = "tile") {
  const rect = /<rect width="256" height="256" rx="([\d.]+)" fill="(#[0-9A-Fa-f]{6})"\/>/.exec(text);
  const group = /<g transform="translate\(128 ([\d.]+)\) scale\(([\d.]+)\) translate\(-128 -128\)">/.exec(text);
  if (!rect || !group) throw new Error(name + ": unexpected tile structure");
  return { rx: Number(rect[1]), fill: rect[2].toUpperCase(), ty: Number(group[1]), scale: Number(group[2]), shapes: parseShapes(text, name) };
}

export function loadMasters() {
  const read = (file) => readFileSync(join(brandDir, file), "utf8");
  const shapesOf = (file) => ({ text: read(file), shapes: parseShapes(read(file), file) });
  return {
    full: shapesOf("mark.svg"),
    fullDark: shapesOf("mark-on-dark.svg"),
    fullMono: shapesOf("mark-mono.svg"),
    small: shapesOf("mark-small.svg"),
    smallDark: shapesOf("mark-small-on-dark.svg"),
    tile: parseTile(read("tile-ink.svg"), "tile-ink.svg"),
    tileSmall: parseTile(read("tile-ink-small.svg"), "tile-ink-small.svg"),
  };
}

// ---------------------------------------------------------------- geometry
// Samples a path (M L H V C Z, absolute or relative) into points on its outline.
export function flattenPath(d) {
  const t = d.match(/[MmLlHhVvCcZz]|[-+]?(?:\d+\.?\d*|\.\d+)(?:[eE][-+]?\d+)?/g) ?? [];
  const points = [];
  let i = 0, cmd = "", x = 0, y = 0, startX = 0, startY = 0;
  const num = () => {
    const v = Number(t[i++]);
    if (!Number.isFinite(v)) throw new Error("Cannot parse path: " + d.slice(0, 80));
    return v;
  };
  const push = (px, py) => { x = px; y = py; points.push([x, y]); };
  while (i < t.length) {
    if (/[A-Za-z]/.test(t[i])) cmd = t[i++];
    const rel = cmd === cmd.toLowerCase();
    const ox = rel ? x : 0, oy = rel ? y : 0;
    switch (cmd.toUpperCase()) {
      case "M": push(ox + num(), oy + num()); startX = x; startY = y; cmd = rel ? "l" : "L"; break;
      case "L": push(ox + num(), oy + num()); break;
      case "H": push(ox + num(), y); break;
      case "V": push(x, oy + num()); break;
      case "Z": push(startX, startY); break;
      case "C": {
        const [x1, y1, x2, y2, x3, y3] = [ox + num(), oy + num(), ox + num(), oy + num(), ox + num(), oy + num()];
        const [x0, y0] = [x, y];
        for (let k = 1; k <= 32; k++) {
          const u = k / 32, v = 1 - u;
          points.push([v ** 3 * x0 + 3 * v * v * u * x1 + 3 * v * u * u * x2 + u ** 3 * x3, v ** 3 * y0 + 3 * v * v * u * y1 + 3 * v * u * u * y2 + u ** 3 * y3]);
        }
        x = x3; y = y3;
        break;
      }
      default: throw new Error("Unsupported path command " + cmd);
    }
  }
  return points;
}

const outline = (shapes) => shapes.flatMap((s) => flattenPath(s.d));

export function measure(shapes) {
  const box = { x0: Infinity, y0: Infinity, x1: -Infinity, y1: -Infinity };
  for (const [x, y] of outline(shapes)) {
    box.x0 = Math.min(box.x0, x); box.x1 = Math.max(box.x1, x);
    box.y0 = Math.min(box.y0, y); box.y1 = Math.max(box.y1, y);
  }
  return box;
}

// Largest distance from (x, y) to any outline point, after x' = scale * x + tx, y' = scale * y + ty.
export function farthest(shapes, { scale, tx, ty }, x, y) {
  let far = 0;
  for (const [px, py] of outline(shapes)) far = Math.max(far, Math.hypot(scale * px + tx - x, scale * py + ty - y));
  return far;
}

// Share of the tile that the mark covers (height, width) and where its centre sits, in tile units.
export function coverage(masters, small) {
  const tile = small ? masters.tileSmall : masters.tile;
  const box = measure((small ? masters.small : masters.full).shapes);
  return {
    height: ((box.y1 - box.y0) * tile.scale) / 256,
    width: ((box.x1 - box.x0) * tile.scale) / 256,
    centreY: tile.ty + (((box.y0 + box.y1) / 2) - 128) * tile.scale,
    top: tile.ty + (box.y0 - 128) * tile.scale,
    bottom: tile.ty + (box.y1 - 128) * tile.scale,
  };
}

// ---------------------------------------------------------------- SVG output
const elements = (shapes) => shapes.map((s) => '<path d="' + s.d + '" fill="' + s.fill + '"/>');

// App icon: ink tile, cream mark. The mark keeps its place in tile-ink.svg, scaled to the tile shape.
export function tileSvg(masters, { small, tile, title = "" }) {
  const { inset, side, round, shrink = 1 } = TILES[tile];
  const layout = small ? masters.tileSmall : masters.tile;
  const k = side / 256;
  const ty = 128 + (layout.ty - 128) * shrink;
  const shapes = (small ? masters.smallDark : masters.fullDark).shapes;
  const rect = '<rect x="' + inset + '" y="' + inset + '" width="' + side + '" height="' + side + '"' + (round ? ' rx="' + fmt(layout.rx * k) + '"' : "") + ' fill="' + layout.fill + '"/>';
  const group = '<g transform="translate(' + fmt(inset + 128 * k) + " " + fmt(inset + ty * k) + ") scale(" + fmt5(layout.scale * k * shrink) + ') translate(-128 -128)">';
  return '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 256 256" fill="none">\n' + (title ? "  <title>" + title + "</title>\n" : "") +
    "  " + rect + "\n  " + group + "\n" + elements(shapes).map((l) => "    " + l).join("\n") + "\n  </g>\n</svg>\n";
}

// ---------------------------------------------------------------- PNG and ICO (deterministic, no dependencies)
const CRC = Array.from({ length: 256 }, (_, n) => { let c = n; for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1; return c >>> 0; });
const crc32 = (buf) => { let c = 0xffffffff; for (const b of buf) c = CRC[(c ^ b) & 255] ^ (c >>> 8); return (c ^ 0xffffffff) >>> 0; };
function chunk(type, data) {
  const out = Buffer.alloc(12 + data.length);
  out.writeUInt32BE(data.length, 0);
  out.write(type, 4, "latin1");
  data.copy(out, 8);
  out.writeUInt32BE(crc32(out.subarray(4, 8 + data.length)), 8 + data.length);
  return out;
}
const paeth = (a, b, c) => { const p = a + b - c, pa = Math.abs(p - a), pb = Math.abs(p - b), pc = Math.abs(p - c); return pa <= pb && pa <= pc ? a : pb <= pc ? b : c; };

// Header facts of a PNG: { width, height, colorType } (6 = RGBA, 2 = RGB).
export function pngInfo(buf) {
  if (!buf.subarray(0, 8).equals(Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]))) throw new Error("Not a PNG");
  return { width: buf.readUInt32BE(16), height: buf.readUInt32BE(20), colorType: buf[25] };
}

export function decodePng(buf) {
  const { width, height, colorType: type } = pngInfo(buf);
  let pos = 8;
  const idat = [];
  while (pos < buf.length) {
    const length = buf.readUInt32BE(pos), name = buf.toString("latin1", pos + 4, pos + 8), data = buf.subarray(pos + 8, pos + 8 + length);
    if (name === "IHDR" && (data[8] !== 8 || (type !== 2 && type !== 6) || data[12] !== 0)) throw new Error("Unsupported PNG layout (need 8-bit RGB/RGBA, not interlaced)");
    if (name === "IDAT") idat.push(data);
    pos += 12 + length;
  }
  const bpp = type === 6 ? 4 : 3, stride = width * bpp;
  const raw = inflateSync(Buffer.concat(idat));
  const rgba = Buffer.alloc(width * height * 4, 255);
  let prev = Buffer.alloc(stride);
  for (let row = 0; row < height; row++) {
    const filter = raw[row * (stride + 1)];
    const line = Buffer.from(raw.subarray(row * (stride + 1) + 1, (row + 1) * (stride + 1)));
    for (let i = 0; i < stride; i++) {
      const a = i >= bpp ? line[i - bpp] : 0, b = prev[i], c = i >= bpp ? prev[i - bpp] : 0;
      line[i] = (line[i] + (filter === 1 ? a : filter === 2 ? b : filter === 3 ? (a + b) >> 1 : filter === 4 ? paeth(a, b, c) : 0)) & 255;
    }
    for (let x = 0; x < width; x++) for (let k = 0; k < bpp; k++) rgba[(row * width + x) * 4 + k] = line[x * bpp + k];
    prev = line;
  }
  return { width, height, rgba };
}

// alpha: false writes 8-bit RGB (the App Store rejects icons with an alpha channel).
export function encodePng({ width, height, rgba }, { alpha = true } = {}) {
  const bpp = alpha ? 4 : 3, stride = width * bpp;
  const rows = [];
  let prev = Buffer.alloc(stride);
  for (let y = 0; y < height; y++) {
    const line = Buffer.alloc(stride);
    for (let x = 0; x < width; x++) {
      if (!alpha && rgba[(y * width + x) * 4 + 3] !== 255) throw new Error("Opaque PNG requested but the image has transparency");
      for (let k = 0; k < bpp; k++) line[x * bpp + k] = rgba[(y * width + x) * 4 + k];
    }
    // Pick the filter with the smallest sum of absolute residuals (deterministic).
    let best = null;
    for (let filter = 0; filter < 5; filter++) {
      const out = Buffer.alloc(stride + 1);
      out[0] = filter;
      let score = 0;
      for (let i = 0; i < stride; i++) {
        const a = i >= bpp ? line[i - bpp] : 0, b = prev[i], c = i >= bpp ? prev[i - bpp] : 0;
        const v = (line[i] - (filter === 1 ? a : filter === 2 ? b : filter === 3 ? (a + b) >> 1 : filter === 4 ? paeth(a, b, c) : 0)) & 255;
        out[i + 1] = v;
        score += v < 128 ? v : 256 - v;
      }
      if (!best || score < best.score) best = { score, out };
    }
    rows.push(best.out);
    prev = line;
  }
  const header = Buffer.alloc(13);
  header.writeUInt32BE(width, 0); header.writeUInt32BE(height, 4);
  header[8] = 8; header[9] = alpha ? 6 : 2;
  return Buffer.concat([
    Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), chunk("IHDR", header), chunk("sRGB", Buffer.from([0])),
    chunk("IDAT", deflateSync(Buffer.concat(rows), { level: 9 })), chunk("IEND", Buffer.alloc(0)),
  ]);
}

// ICO with PNG frames (Vista and later), ascending sizes. 256 is stored as 0.
export function buildIco(frames) {
  const sorted = [...frames].sort((a, b) => a.size - b.size);
  const head = Buffer.alloc(6 + 16 * sorted.length);
  head.writeUInt16LE(1, 2); head.writeUInt16LE(sorted.length, 4);
  let offset = head.length;
  sorted.forEach(({ size, png }, i) => {
    const e = 6 + 16 * i;
    head[e] = size === 256 ? 0 : size; head[e + 1] = size === 256 ? 0 : size;
    head.writeUInt16LE(1, e + 4); head.writeUInt16LE(32, e + 6);
    head.writeUInt32LE(png.length, e + 8); head.writeUInt32LE(offset, e + 12);
    offset += png.length;
  });
  return Buffer.concat([head, ...sorted.map((f) => f.png)]);
}

// ---------------------------------------------------------------- outputs
export const PATHS = {
  macIconset: "packaging/macos/Cage.iconset",
  winIco: "packaging/windows/Cage.ico",
  linuxIcons: "packaging/linux/icons",
  linuxName: "io.github.lee_xeo.Cage",
  catalog: "mobile/ios/CageMobile/Assets.xcassets",
  drawables: "mobile/android/res/drawable",
  mobileLogo: "mobile/assets/brand/logo.svg",
  // The tenant portal's own icons (served by internal/tenant from its embedded copy).
  portalIcons: "internal/tenant/portal-icons",
};
export const MAC_SIZES = [[16, "icon_16x16"], [32, "icon_16x16@2x"], [32, "icon_32x32"], [64, "icon_32x32@2x"], [128, "icon_128x128"], [256, "icon_128x128@2x"], [256, "icon_256x256"], [512, "icon_256x256@2x"], [512, "icon_512x512"], [1024, "icon_512x512@2x"]];
export const WIN_SIZES = [16, 20, 24, 32, 40, 48, 64, 128, 256];
export const LINUX_SIZES = [16, 22, 24, 32, 48, 64, 128, 256, 512];
export const FAVICON_SIZES = [16, 32, 48];
// Web icon sets: the PNG logo name, the SVG favicon, and further copies of the PNG logo.
export const WEB_DIRS = [
  { dir: "ui/public", png: "cage.png", svg: "cage.svg", extra: ["ui/src/logo.png"] },
];
// Add to Home Screen (web manifest) icons for the web apps that are installed from a phone: the workbench
// (ui/public, its manifest.webmanifest) and the portal (PATHS.portalIcons). 192 px and the 512 px logo for
// "any", a 512 px opaque square for "maskable".
export const MANIFEST_DIRS = ["ui/public", PATHS.portalIcons];

// Adaptive icon placement: the tile-ink composition inside the 72 dp visible area of the 108 dp layer.
export function androidLayout(masters) {
  const k = ANDROID.visible / 256, c = ANDROID.layer / 2;
  const scale = masters.tile.scale * k;
  const frame = { scale: Math.round(scale * 100000) / 100000, tx: Math.round((c - 128 * scale) * 1000) / 1000, ty: Math.round((c + (masters.tile.ty - 128) * k - 128 * scale) * 1000) / 1000 };
  const reach = farthest(masters.fullDark.shapes, frame, c, c);
  if (reach > ANDROID.safe / 2) throw new Error("Android icon art reaches " + fmt(reach) + " dp from the centre; the safe zone radius is " + ANDROID.safe / 2 + " dp");
  return { frame: [ANDROID.layer, frame.scale, frame.tx, frame.ty], reach };
}

// Everything that does not need a browser: SVG, Android vectors, iOS catalog entries.
export function textOutputs(masters) {
  const out = new Map();
  const put = (path, text) => out.set(path, text);
  put(PATHS.mobileLogo, masters.full.text);

  // In-app logo. iOS: one flat SVG per appearance. Android: ink and glow follow the theme, clay is fixed.
  const imageset = join(PATHS.catalog, "Logo.imageset");
  put(join(imageset, "Logo.svg"), iosSvg(parseSvg(masters.full.text), COLORS.ink));
  put(join(imageset, "Logo-dark.svg"), iosSvg(parseSvg(masters.fullDark.text), COLORS.cream));
  put(join(imageset, "Contents.json"), json({
    images: [
      { filename: "Logo.svg", idiom: "universal" },
      { appearances: [{ appearance: "luminosity", value: "dark" }], filename: "Logo-dark.svg", idiom: "universal" },
    ],
    info, properties: { "preserves-vector-representation": true },
  }));
  put(join(PATHS.catalog, "AppIcon.appiconset/Contents.json"), json({ images: [{ filename: "AppIcon.png", idiom: "universal", platform: "ios", size: "1024x1024" }], info }));
  const note = "Generated by brand/generate.mjs from the masters in brand/. Do not edit.";
  put(join(PATHS.drawables, "cage_logo.xml"), androidVector(parseSvg(masters.full.text), { note, colors: { [COLORS.ink]: "?attr/cageInk", [COLORS.glow]: "?attr/cageGlow" } }));

  // Android adaptive icon: ink background (colour resource), cream mark with glow inside the safe zone,
  // and a one-colour monochrome layer without the glow.
  const { frame } = androidLayout(masters);
  put(join(PATHS.drawables, "cage_icon_foreground.xml"), androidVector(parseSvg(masters.fullDark.text), { frame, note }));
  put(join(PATHS.drawables, "cage_icon_monochrome.xml"), androidVector(parseSvg(masters.fullMono.text), { all: "#FFFFFFFF", frame, note }));

  // Scalable SVGs: full mark for hicolor, small cut for favicons.
  put(join(PATHS.linuxIcons, "hicolor/scalable/apps", PATHS.linuxName + ".svg"), tileSvg(masters, { small: false, tile: "bleed", title: "Super Cage" }));
  for (const { dir, svg } of WEB_DIRS) put(join(dir, svg), tileSvg(masters, { small: true, tile: "bleed", title: "Super Cage" }));
  put(join(PATHS.portalIcons, "icon.svg"), tileSvg(masters, { small: true, tile: "bleed", title: "Super Cage" }));
  return out;
}

// One headless Chrome renders every PNG natively at its target size.
async function openRenderer() {
  const spec = process.env.CAGE_PLAYWRIGHT_MODULE || "playwright";
  let playwright;
  try {
    playwright = await import(spec.startsWith("/") ? pathToFileURL(spec).href : spec);
  } catch (error) {
    throw new Error("Cannot load Playwright from " + spec + " (" + error.message + "). Set CAGE_PLAYWRIGHT_MODULE to the playwright module.");
  }
  const chromium = playwright.chromium ?? playwright.default?.chromium;
  if (!chromium) throw new Error(spec + " does not export chromium");
  let browser;
  try {
    browser = await chromium.launch({
      headless: true,
      executablePath: process.env.CAGE_TEST_BROWSER_PATH || undefined,
      args: ["--force-color-profile=srgb", "--disable-gpu", "--hide-scrollbars"],
    });
  } catch (error) {
    throw new Error("Cannot start Chrome (" + error.message.split("\n")[0] + "). Set CAGE_TEST_BROWSER_PATH to a Chrome or Chromium executable.");
  }
  const page = await browser.newPage({ viewport: { width: 1024, height: 1024 }, deviceScaleFactor: 1 });
  // Renders are queued: one page cannot show two images at once.
  let queue = Promise.resolve();
  const render = (svg, px) => {
    const run = queue.then(() => draw(svg, px));
    queue = run.catch(() => {});
    return run;
  };
  const draw = async (svg, px) => {
    const sized = svg.replace("<svg ", '<svg width="' + px + '" height="' + px + '" style="display:block" ');
    await page.setContent('<!doctype html><meta charset="utf-8"><style>html,body{margin:0;background:transparent}</style>' + sized);
    const shot = await page.screenshot({ clip: { x: 0, y: 0, width: px, height: px }, omitBackground: true, type: "png" });
    const image = decodePng(shot);
    if (image.width !== px || image.height !== px) throw new Error("Chrome returned a " + image.width + "x" + image.height + " screenshot for " + px + " px");
    return image;
  };
  return { render, close: () => browser.close() };
}

const pixel = (image, x, y) => [...image.rgba.subarray((y * image.width + x) * 4, (y * image.width + x) * 4 + 4)];

// Where the clay light sits in a rendered tile, in pixels.
function lightAt(masters, px, { small, tile }) {
  const { inset, side } = TILES[tile], k = side / 256, layout = small ? masters.tileSmall : masters.tile;
  const lamp = measure((small ? masters.smallDark : masters.fullDark).shapes.slice(-1));
  return [((inset + 128 * k + ((lamp.x0 + lamp.x1) / 2 - 128) * layout.scale * k) * px) / 256, ((inset + layout.ty * k + ((lamp.y0 + lamp.y1) / 2 - 128) * layout.scale * k) * px) / 256];
}

// Catches a render that shows the wrong image or size: the light must be clay-coloured where the layout says.
function assertLight(masters, image, spec) {
  const [x, y] = lightAt(masters, image.width, spec).map(Math.round);
  let best = -Infinity;
  for (let dy = -1; dy <= 1; dy++) for (let dx = -1; dx <= 1; dx++) { const [r, , b] = pixel(image, x + dx, y + dy); best = Math.max(best, r - b); }
  if (best < 70) throw new Error("The " + image.width + " px " + spec.tile + " render has no clay light at (" + x + ", " + y + "); the browser drew something else");
}

export async function rasterOutputs(masters, renderer) {
  const out = new Map();
  const cache = new Map();
  // The cut follows the pixel size: small at or below SMALL_MAX.
  const png = async ({ px, tile, alpha }) => {
    const small = px <= SMALL_MAX;
    const key = [px, tile, alpha].join("|");
    if (!cache.has(key)) {
      const image = await renderer.render(tileSvg(masters, { small, tile }), px);
      assertLight(masters, image, { small, tile });
      cache.set(key, { image, png: encodePng(image, { alpha }) });
    }
    return cache.get(key);
  };

  // macOS iconset: 824/1024 tile, transparent margin.
  for (const [px, name] of MAC_SIZES) out.set(join(PATHS.macIconset, name + ".png"), (await png({ px, tile: "mac", alpha: true })).png);

  // Windows: full-bleed rounded square, PNG frames.
  out.set(PATHS.winIco, buildIco(await Promise.all(WIN_SIZES.map(async (size) => ({ size, png: (await png({ px: size, tile: "bleed", alpha: true })).png })))));

  // Linux hicolor.
  for (const px of LINUX_SIZES) out.set(join(PATHS.linuxIcons, "hicolor", px + "x" + px, "apps", PATHS.linuxName + ".png"), (await png({ px, tile: "bleed", alpha: true })).png);

  // iOS: 1024 opaque square, no rounding. Check the colours Chrome produced are exact.
  const ios = await png({ px: 1024, tile: "square", alpha: false });
  const lampAt = lightAt(masters, 1024, { small: false, tile: "square" }).map(Math.round);
  const expect = (label, got, want) => {
    if (got.join() !== want.join()) throw new Error(label + " rendered as rgba(" + got + ") instead of rgba(" + want + "); the browser changed colours, check --force-color-profile=srgb");
  };
  expect("Tile ink", pixel(ios.image, 8, 512), [0x1f, 0x1e, 0x1d, 255]);
  expect("Clay light", pixel(ios.image, ...lampAt), [0xd9, 0x77, 0x57, 255]);
  out.set(join(PATHS.catalog, "AppIcon.appiconset/AppIcon.png"), ios.png);

  // Web: rounded PNG logo, touch icon (opaque square), ICO (small cut).
  const faviconIco = buildIco(await Promise.all(FAVICON_SIZES.map(async (size) => ({ size, png: (await png({ px: size, tile: "bleed", alpha: true })).png }))));
  const rounded = await png({ px: 512, tile: "bleed", alpha: true });
  if (pixel(rounded.image, 0, 0)[3] !== 0) throw new Error("Rounded tile corner is not transparent");
  const touch = (await png({ px: 180, tile: "square", alpha: false })).png;
  for (const { dir, png: name, extra } of WEB_DIRS) {
    out.set(join(dir, name), rounded.png);
    out.set(join(dir, "favicon.ico"), faviconIco);
    out.set(join(dir, "apple-touch-icon.png"), touch);
    for (const file of extra) out.set(file, rounded.png);
  }
  const icon192 = (await png({ px: 192, tile: "bleed", alpha: true })).png;
  const maskable = await png({ px: 512, tile: "maskable", alpha: false });
  expect("Maskable tile corner", pixel(maskable.image, 4, 4), [0x1f, 0x1e, 0x1d, 255]);
  for (const dir of MANIFEST_DIRS) {
    out.set(join(dir, "icon-192.png"), icon192);
    out.set(join(dir, "icon-maskable-512.png"), maskable.png);
  }
  out.set(join(PATHS.portalIcons, "icon-512.png"), rounded.png);
  out.set(join(PATHS.portalIcons, "favicon.ico"), faviconIco);
  out.set(join(PATHS.portalIcons, "apple-touch-icon.png"), touch);
  return out;
}

export async function allOutputs() {
  const masters = loadMasters();
  const out = textOutputs(masters);
  let renderer;
  try {
    renderer = await openRenderer();
    for (const [path, data] of await rasterOutputs(masters, renderer)) out.set(path, data);
  } finally {
    await renderer?.close();
  }
  return out;
}

function validateIconset() {
  if (process.platform !== "darwin") return "iconset not validated (iconutil is macOS only)";
  const work = mkdtempSync(join(tmpdir(), "cage-iconset-"));
  try {
    execFileSync("iconutil", ["-c", "icns", join(root, PATHS.macIconset), "-o", join(work, "Cage.icns")], { stdio: "pipe" });
    return "iconutil accepted the iconset";
  } catch (error) {
    throw new Error("iconutil rejected " + PATHS.macIconset + ": " + (error.stderr?.toString() || error.message));
  } finally {
    rmSync(work, { recursive: true, force: true });
  }
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const mode = process.argv[2];
  if (mode && mode !== "--check" && mode !== "--check-text") { console.error("Usage: node brand/generate.mjs [--check | --check-text]"); process.exit(2); }
  try {
    const stale = [];
    let written = 0;
    const outputs = mode === "--check-text" ? textOutputs(loadMasters()) : await allOutputs();
    for (const [rel, data] of [...outputs].sort(([a], [b]) => (a < b ? -1 : 1))) {
      const path = join(root, rel), bytes = Buffer.isBuffer(data) ? data : Buffer.from(data);
      if (existsSync(path) && readFileSync(path).equals(bytes)) continue;
      if (mode) { stale.push(rel); continue; }
      mkdirSync(dirname(path), { recursive: true });
      writeFileSync(path, bytes);
      written++;
    }
    if (stale.length) { console.error("Stale brand outputs; run node brand/generate.mjs:\n" + stale.join("\n")); process.exit(1); }
    console.log(mode ? outputs.size + " outputs are up to date" : outputs.size + " outputs, " + written + " written; " + validateIconset());
  } catch (error) {
    console.error("brand/generate.mjs: " + error.message);
    process.exit(1);
  }
}
