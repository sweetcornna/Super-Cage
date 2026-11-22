// Writes the master SVGs of this directory from centre lines (gen.mjs draws the brush strokes).
// node brand/final.mjs                      rewrite the masters next to this file
// MASTERS_OUT=<dir> node brand/final.mjs   write them elsewhere (brand/generate.test.mjs compares)
import fs from "node:fs"; import { fileURLToPath } from "node:url"; import { mark, svg, brush, blob } from "./gen.mjs";
const W = [[0, 6], [.3, 10], [.5, 6], [.72, 12.8], [1, 7]];
const ogee = [[68, 213], [[63, 184], [64, 150], [71, 128]], [[78, 104], [98, 90], [113, 80]], [[127, 71], [149, 60], [148, 42]], [[147, 27], [122, 24], [115, 39]], [[108, 55], [124, 69], [141, 79]], [[159, 90], [180, 103], [185, 129]], [[189, 152], [193, 184], [189, 213]]];
const ribs = [[[[108, 92], [[94, 110], [85, 156], [85, 211]]], [[0, 2], [.5, 4.2], [1, 3.2]]], [[[148, 92], [[162, 110], [171, 156], [171, 211]]], [[0, 2.2], [.5, 5], [1, 3.6]]]];
const hoop = [[[69, 193], [[100, 201], [156, 201], [188, 193]]], [[0, 2.4], [.5, 4.6], [1, 2.6]]];
const v = { cage: ogee, cageW: W, extra: [...ribs, hoop], barL: [[124, 88], [[111, 112], [104, 158], [103, 211]]], barR: [[132, 88], [[146, 112], [152, 158], [153, 211]]], light: [128, 168] };
const sCage = [[60, 214], [[54, 180], [56, 148], [64, 124]], [[73, 98], [96, 86], [112, 77]], [[128, 68], [152, 57], [150, 38]], [[148, 20], [120, 18], [113, 36]], [[106, 54], [124, 68], [142, 77]], [[162, 88], [186, 100], [192, 126]], [[198, 150], [202, 182], [196, 214]]];
const small = (ink, clay) => '<path d="' + brush(sCage, [[0, 15], [.3, 18], [.5, 12], [.72, 23], [1, 16]]) + brush([[30, 222], [[92, 228], [164, 228], [226, 221]]], [[0, 10], [.2, 19], [.6, 22], [.9, 17], [1, 10]]) + '" fill="' + ink + '"/>\n<path d="' + blob(128, 160, 29, 31, .06) + '" fill="' + clay + '"/>';
const out = (process.env.MASTERS_OUT || fileURLToPath(new URL("./", import.meta.url))).replace(/\/*$/, "/"); fs.mkdirSync(out, { recursive: true });
const tile = (inner, s, dy) => '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 256 256" fill="none"><rect width="256" height="256" rx="57" fill="#1F1E1D"/><g transform="translate(128 ' + (128 + dy) + ') scale(' + s + ') translate(-128 -128)">' + inner + '</g></svg>\n';
fs.writeFileSync(out + "mark.svg", svg(mark({ v })));
fs.writeFileSync(out + "mark-on-dark.svg", svg(mark({ ink: "#F3EFE5", glow: "#4A3329", v })));
fs.writeFileSync(out + "mark-themed.svg", svg(mark({ ink: "currentColor", clay: "var(--clay, #D97757)", glow: "var(--clay-soft, #F6E5DC)", v })));
fs.writeFileSync(out + "mark-mono.svg", svg(mark({ ink: "#1F1E1D", clay: "#1F1E1D", glow: "", v })));
fs.writeFileSync(out + "mark-small.svg", svg(small("#1F1E1D", "#D97757")));
fs.writeFileSync(out + "mark-small-on-dark.svg", svg(small("#F3EFE5", "#D97757")));
fs.writeFileSync(out + "mark-small-themed.svg", svg(small("currentColor", "var(--clay, #D97757)")));
fs.writeFileSync(out + "tile-ink.svg", tile(mark({ ink: "#F3EFE5", glow: "#4A3329", v }), .74, -4));
fs.writeFileSync(out + "tile-ink-small.svg", tile(small("#F3EFE5", "#D97757"), .72, 0));
