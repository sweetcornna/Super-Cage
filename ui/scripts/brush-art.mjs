// Generates the desktop's hand-drawn art in the brush hand of the Super Cage
// mark (brand/design-language.md §4): each centre line becomes a filled
// stroke that is thick in the middle and thin at its ends, with a little
// irregular pressure. Clay shapes stay plain fills.
//
//   CAGE_TEST_BROWSER_PATH=... node scripts/brush-art.mjs   # rewrites src/drawings-art.mjs
//
// Sources are the phone doodles (src/mobile-cli/doodles.jsx, unchanged) plus
// the desktop-only drawings below, all written as ordinary stroked SVG.
import { createRequire } from "node:module";
import { readFile, writeFile } from "node:fs/promises";

const require = createRequire(import.meta.url);
const extra = {
  // A key on a tag: empty account lists.
  key: '<svg viewBox="0 0 120 90"><path d="M38.6 30.4c8.2-.3 14.6 6.3 14.3 14.6-.3 8-6.9 14.1-14.8 13.9-8.1-.2-14.3-6.8-14-14.9.3-7.6 6.7-13.4 14.5-13.6z" stroke-width="2.4"/><path d="M37.8 41.2c2.3-.1 4 1.7 3.9 3.9-.1 2.1-1.9 3.7-4 3.6-2.2-.1-3.8-1.9-3.7-4 .1-2 1.8-3.4 3.8-3.5z" stroke-width="1.8" opacity=".6"/><path d="M53.2 44.8c13.4-.7 26.8-.5 40.2.4" stroke-width="2.4"/><path d="M79.8 45.4c.4 3.6.5 7.2.3 10.8M88.6 45.6c.3 2.6.3 5.2.1 7.8" stroke-width="2.6"/><path d="M27.8 34.6c-4.6-5-8.4-9.8-11.2-14.8" stroke-width="1.7" opacity=".6"/><path d="M8.6 13.4c4.8-1.8 9.6-2.9 14.4-3.5l3.6 9.8c-4.6 1-9.2 2.3-13.8 4z" stroke-width="1.8" opacity=".7"/><path d="M98.4 18.4c.6 3.4 1.8 4.8 5.1 5.4-3.3.6-4.5 2-5.1 5.5-.6-3.5-1.9-4.9-5.3-5.5 3.4-.6 4.7-2 5.3-5.4z" fill="clay"/></svg>',
  // Unplugged cable: lost connections.
  plug: '<svg viewBox="0 0 120 90"><path d="M20.6 34.2c7.4-.5 14.8-.4 22.2.2 1.2.1 2 1 2 2.2l-.2 17.4c0 1.2-1 2.1-2.2 2.2-7.3.4-14.6.4-21.9-.1-1.2-.1-2.1-1-2.1-2.2l.1-17.4c0-1.2.9-2.2 2.1-2.3z" stroke-width="2.4"/><path d="M45.2 39.8c2.9-.2 5.8-.3 8.6-.2M45.4 50.6c2.8 0 5.6 0 8.4.1" stroke-width="2.4"/><path d="M18.6 45.6c-5.6.3-9.6 3.4-11.8 8.6" stroke-width="2"/><path d="M76.4 32.6c7.6-.4 15.2-.3 22.8.3 1.3.1 2.2 1.1 2.2 2.4l-.3 19.6c0 1.3-1.1 2.3-2.4 2.3-7.5.3-15 .2-22.5-.2-1.3-.1-2.3-1.1-2.2-2.4l.2-19.6c0-1.3 1-2.3 2.2-2.4z" stroke-width="2.4"/><path d="M81.2 40.2c1.4 0 2.8 0 4.2-.1M81.4 50.6c1.5 0 2.9 0 4.4.1" stroke-width="2.6"/><path d="M101.8 45.2c5.2.5 8.6 3.4 10.4 8.2" stroke-width="2"/><path d="M63.6 35.2c-.6-1.5-1.2-2.9-1.9-4.3M64.8 45c1.5-.1 2.9-.2 4.4-.4M63.4 54.8c-.5 1.5-1 2.9-1.6 4.4" stroke-width="2.2" stroke="clay"/></svg>',
  // A tray with an arrow: nothing downloaded yet.
  tray: '<svg viewBox="0 0 120 90"><path d="M18.6 52.4c3.4-.1 6.8-.2 10.2-.3l6.4 9.4c16.4.6 32.8.6 49.2-.1l6.6-9.6c3.5 0 7.1.1 10.6.2l-.4 17.8c0 2-1.6 3.6-3.6 3.6-25 .8-50 .7-75-.3-2-.1-3.5-1.7-3.5-3.7z" stroke-width="2.4"/><path d="M59.8 14.6c.4 10.4.4 20.8.2 31.2" stroke-width="2.4"/><path d="M50.2 37.4c3.6 3 6.8 6.1 9.8 9.4 3.2-3.4 6.6-6.6 10.2-9.6" stroke-width="2.4"/><path d="M88.4 16.4c.6 3.4 1.8 4.8 5.1 5.4-3.3.6-4.5 2-5.1 5.5-.6-3.5-1.9-4.9-5.3-5.5 3.4-.6 4.7-2 5.3-5.4z" fill="clay"/></svg>',
  // A cup of tea: quiet lists and idle moments.
  cup: '<svg viewBox="0 0 120 90"><path d="M34.4 38.6c14.6-.8 29.4-.7 44 .2-.4 9.4-1.6 17.2-4.4 23.4-2.2 4.8-6.4 7.6-11.6 7.8-6.2.3-12.4.2-18.6-.2-5.2-.4-9.2-3.4-11.2-8.4-2.6-6.4-3.6-14-4.2-22.8z" stroke-width="2.4"/><path d="M78.2 44.2c5.6-1.2 9.8 1 10.2 5.4.4 4.6-3.6 8.2-10.6 9.6" stroke-width="2.2"/><path d="M24.6 74.6c21.2 1.4 42.6 1.4 63.8 0" stroke-width="2.2"/><path d="M48.4 31.2c-3.4-3.6-3.2-7 .4-10.4 3.4-3.2 3.6-6.4.6-9.8M60.6 31.6c-3-3.2-2.8-6.2.4-9.2 3-2.8 3.2-5.6.6-8.6" stroke-width="1.8" opacity=".6"/><path d="M94.4 22.4c.6 3.4 1.8 4.8 5.1 5.4-3.3.6-4.5 2-5.1 5.5-.6-3.5-1.9-4.9-5.3-5.5 3.4-.6 4.7-2 5.3-5.4z" fill="clay"/></svg>',
};

async function phoneArt() {
  const source = await readFile(new URL("../src/mobile-cli/doodles.jsx", import.meta.url), "utf8");
  const body = source.slice(source.indexOf("const art = {"), source.indexOf("};", source.indexOf("const art = {")));
  const art = {};
  for (const match of body.matchAll(/^\s*([a-z]+): "((?:[^"\\]|\\.)*)",?$/gm)) art[match[1]] = JSON.parse('"' + match[2] + '"');
  delete art.logo; // the phone's own mark, retired on desktop
  return art;
}

// Runs in the page: turns one stroked SVG into brush fills.
function brushify(svgText, seed) {
  let state = seed;
  const random = () => ((state = (state * 16807) % 2147483647) / 2147483647);
  const host = document.createElement("div");
  host.innerHTML = svgText;
  document.body.append(host);
  const svg = host.querySelector("svg");
  svg.setAttribute("width", "600");
  const out = [];
  const fmt = (n) => (Math.round(n * 10) / 10).toString();
  for (const element of svg.querySelectorAll("path")) {
    const d = element.getAttribute("d");
    const fill = element.getAttribute("fill");
    const strokeAttr = element.getAttribute("stroke") || svg.getAttribute("stroke") || "currentColor";
    const opacity = element.getAttribute("opacity");
    const clay = (fill && fill !== "none" && fill !== "currentColor") || /clay/.test(strokeAttr);
    const paint = clay ? "var(--clay, #D97757)" : "currentColor";
    const op = opacity ? ' opacity="' + opacity + '"' : "";
    if (fill && fill !== "none") { out.push('<path d="' + d + '" fill="' + paint + '"' + op + "/>"); continue; }
    const width = Number(element.getAttribute("stroke-width") || svg.getAttribute("stroke-width") || 2);
    // Split compound paths at every move-to so each stroke tapers by itself.
    const parts = d.match(/[Mm][^Mm]*/g);
    let cursor = { x: 0, y: 0 };
    for (let index = 0; index < parts.length; index++) {
      let part = parts[index];
      if (part[0] === "m" && index > 0) part = "M" + fmt(cursor.x + Number(part.slice(1).trim().split(/[\s,]+|(?=-)/)[0])) + " " + fmt(cursor.y + Number(part.slice(1).trim().split(/[\s,]+|(?=-)/)[1])) + part.slice(1).trim().replace(/^-?[\d.]+[\s,]*-?[\d.]+/, "");
      const probe = document.createElementNS("http://www.w3.org/2000/svg", "path");
      probe.setAttribute("d", part);
      svg.append(probe);
      const length = probe.getTotalLength();
      const end = probe.getPointAtLength(length);
      cursor = { x: end.x, y: end.y };
      const closed = /z\s*$/i.test(part.trim());
      if (length < 2) {
        const p = probe.getPointAtLength(0);
        const r = width * 0.62;
        out.push('<circle cx="' + fmt(p.x + 0.2) + '" cy="' + fmt(p.y) + '" r="' + fmt(r) + '" fill="' + paint + '"' + op + "/>");
        probe.remove();
        continue;
      }
      const steps = Math.max(8, Math.round(length / 2.6));
      const phase = random() * Math.PI * 2, wobble = 0.14 + random() * 0.1;
      const left = [], right = [];
      for (let i = 0; i <= steps; i++) {
        const t = i / steps;
        const p = probe.getPointAtLength(t * length);
        const a = probe.getPointAtLength(Math.max(0, t * length - 0.6));
        const b = probe.getPointAtLength(Math.min(length, t * length + 0.6));
        let dx = b.x - a.x, dy = b.y - a.y;
        const n = Math.hypot(dx, dy) || 1;
        dx /= n; dy /= n;
        // Pressure: full in the middle, lifting at both ends, with a slow wobble.
        // The brush lands heavy and lifts off thin, like the mark's strokes.
        const taper = closed ? 0.62 + 0.38 * Math.sin(Math.PI * t) : Math.pow(Math.sin(Math.PI * Math.min(1, Math.max(0, t * 0.94 + 0.03))), 0.5);
        const pressure = (1 + wobble * Math.sin(phase + t * Math.PI * 2.2)) * (1.18 - 0.36 * t);
        const half = Math.max(0.22, (width * 0.8 * taper * pressure) / 2 + 0.08);
        left.push([p.x - dy * half, p.y + dx * half]);
        right.push([p.x + dy * half, p.y - dx * half]);
      }
      probe.remove();
      const ring = [...left, ...right.reverse()];
      // Catmull-Rom through the outline, written as cubic Béziers.
      let path = "M" + fmt(ring[0][0]) + " " + fmt(ring[0][1]);
      for (let i = 0; i < ring.length; i++) {
        const p0 = ring[(i - 1 + ring.length) % ring.length], p1 = ring[i], p2 = ring[(i + 1) % ring.length], p3 = ring[(i + 2) % ring.length];
        const c1 = [p1[0] + (p2[0] - p0[0]) / 6, p1[1] + (p2[1] - p0[1]) / 6];
        const c2 = [p2[0] - (p3[0] - p1[0]) / 6, p2[1] - (p3[1] - p1[1]) / 6];
        path += "C" + fmt(c1[0]) + " " + fmt(c1[1]) + " " + fmt(c2[0]) + " " + fmt(c2[1]) + " " + fmt(p2[0]) + " " + fmt(p2[1]);
      }
      out.push('<path d="' + path + 'Z" fill="' + paint + '"' + op + "/>");
    }
  }
  host.remove();
  return '<svg aria-hidden="true" focusable="false" viewBox="' + svg.getAttribute("viewBox") + '">' + out.join("") + "</svg>";
}

// Only what the desktop draws; anything else falls back to the phone's line art.
const used = ["sun", "moon", "squiggle", "server", "folder", "terminal", "plug", "key", "tray", "cup"];
// Art that only a lazily loaded page draws is written to its own module, so the first load does not carry it.
// Seeds continue after the first group's, so adding a drawing here leaves every other drawing as it was.
const lazy = { "devices-art.mjs": { export: "deviceArt", used: ["laptop"] } };
const all = { ...(await phoneArt()), ...extra };
const sources = Object.fromEntries(used.map((name) => [name, all[name]]));
const lazySources = Object.fromEntries(Object.entries(lazy).map(([file, group]) => [file, Object.fromEntries(group.used.map((name) => [name, all[name]]))]));
const engines = require(process.env.CAGE_PLAYWRIGHT_MODULE || "playwright");
const browser = await engines.chromium.launch({ headless: true, executablePath: process.env.CAGE_TEST_BROWSER_PATH });
try {
  const page = await browser.newPage();
  await page.setContent("<!doctype html><body></body>");
  const art = {};
  let seed = 7;
  for (const [name, svg] of Object.entries(sources)) art[name] = await page.evaluate(([fn, text, s]) => new Function("return " + fn)()(text, s), [brushify.toString(), svg, seed++ * 7919]);
  const lines = Object.entries(art).map(([name, svg]) => "  " + JSON.stringify(name) + ": " + JSON.stringify(svg) + ",");
  await writeFile(new URL("../src/drawings-art.mjs", import.meta.url),
    "// Generated by scripts/brush-art.mjs from the phone doodles and the desktop\n// drawings defined there. Do not edit by hand.\nexport const brushArt = {\n" + lines.join("\n") + "\n};\n");
  console.log("wrote", Object.keys(art).join(", "));
  for (const [file, group] of Object.entries(lazySources)) {
    const drawn = {};
    for (const [name, svg] of Object.entries(group)) drawn[name] = await page.evaluate(([fn, text, s]) => new Function("return " + fn)()(text, s), [brushify.toString(), svg, seed++ * 7919]);
    const rows = Object.entries(drawn).map(([name, svg]) => "  " + JSON.stringify(name) + ": " + JSON.stringify(svg) + ",");
    await writeFile(new URL("../src/" + file, import.meta.url),
      "// Generated by scripts/brush-art.mjs from the phone doodles and the desktop\n// drawings defined there. Do not edit by hand. Loaded only with the page that draws it.\nexport const " + lazy[file].export + " = {\n" + rows.join("\n") + "\n};\n");
    console.log("wrote", file, Object.keys(drawn).join(", "));
  }
} finally {
  await browser.close();
}
