// Draws the Super Cage mark as brush strokes: variable-width outlines built from centre lines.
import fs from "node:fs";
const cubic = (p0, p1, p2, p3, t) => { const u = 1 - t; return [u*u*u*p0[0] + 3*u*u*t*p1[0] + 3*u*t*t*p2[0] + t*t*t*p3[0], u*u*u*p0[1] + 3*u*u*t*p1[1] + 3*u*t*t*p2[1] + t*t*t*p3[1]]; };
// path: [start, [c1, c2, end], ...] -> evenly spaced points
function sample(path, step = 1.5) {
  const dense = []; let cur = path[0];
  for (const [c1, c2, end] of path.slice(1)) { for (let i = dense.length ? 1 : 0; i <= 80; i++) dense.push(cubic(cur, c1, c2, end, i / 80)); cur = end; }
  const len = [0]; for (let i = 1; i < dense.length; i++) len.push(len[i-1] + Math.hypot(dense[i][0]-dense[i-1][0], dense[i][1]-dense[i-1][1]));
  const total = len.at(-1), n = Math.max(8, Math.round(total / step)), out = []; let j = 0;
  for (let k = 0; k <= n; k++) { const d = total * k / n; while (j < len.length - 2 && len[j+1] < d) j++; const f = (d - len[j]) / (len[j+1] - len[j] || 1); out.push([dense[j][0] + (dense[j+1][0]-dense[j][0])*f, dense[j][1] + (dense[j+1][1]-dense[j][1])*f]); }
  return out;
}
const widthAt = (stops, s) => { let i = 0; while (i < stops.length - 2 && stops[i+1][0] < s) i++; const [s0, w0] = stops[i], [s1, w1] = stops[i+1]; const f = Math.min(1, Math.max(0, (s - s0) / (s1 - s0 || 1))); const e = f*f*(3 - 2*f); return w0 + (w1 - w0) * e; };
const r2 = (v) => Math.round(v * 10) / 10;
// smooth closed path through points (Catmull-Rom -> cubic Bezier)
function smooth(pts) { const n = pts.length; let d = "M" + r2(pts[0][0]) + " " + r2(pts[0][1]); for (let i = 0; i < n; i++) { const p0 = pts[(i-1+n)%n], p1 = pts[i], p2 = pts[(i+1)%n], p3 = pts[(i+2)%n]; d += "C" + [p1[0]+(p2[0]-p0[0])/6, p1[1]+(p2[1]-p0[1])/6, p2[0]-(p3[0]-p1[0])/6, p2[1]-(p3[1]-p1[1])/6, p2[0], p2[1]].map(r2).join(" "); } return d + "Z"; }
export function brush(path, stops, { step = Number(process.env.BRUSH_STEP || 5) } = {}) {
  const pts = sample(path, step), n = pts.length, L = [], R = [];
  for (let i = 0; i < n; i++) { const a = pts[Math.max(0, i-1)], b = pts[Math.min(n-1, i+1)]; let tx = b[0]-a[0], ty = b[1]-a[1]; const m = Math.hypot(tx, ty) || 1; tx /= m; ty /= m; const w = widthAt(stops, i/(n-1)) / 2; L.push([pts[i][0] - ty*w, pts[i][1] + tx*w]); R.push([pts[i][0] + ty*w, pts[i][1] - tx*w]); }
  // round caps: a half circle of points from one side to the other
  const cap = (i, from, to) => { const c = pts[i], a0 = Math.atan2(from[1]-c[1], from[0]-c[0]); let a1 = Math.atan2(to[1]-c[1], to[0]-c[0]); const w = Math.hypot(from[0]-c[0], from[1]-c[1]); let d = a1 - a0; while (d <= 0) d += Math.PI*2; if (Math.abs(d - Math.PI) > .5) d = Math.PI; const out = []; for (let k = 1; k < 6; k++) { const a = a0 + (i === 0 ? -1 : 1) * 0 + d * k / 6 * capDir; out.push([c[0] + Math.cos(a)*w, c[1] + Math.sin(a)*w]); } return out; };
  let capDir = -1; const endCap = cap(n-1, L[n-1], R[n-1]); capDir = -1; const startCap = cap(0, R[0], L[0]);
  return smooth([...L, ...endCap, ...R.slice().reverse(), ...startCap]);
}
// an egg: rx/ry ellipse, slightly fuller at the bottom
export function blob(cx, cy, rx, ry = rx, low = 0) { const pts = []; for (let i = 0; i < 24; i++) { const a = i / 24 * Math.PI * 2; const s = Math.sin(a); pts.push([cx + Math.cos(a) * rx * (1 + low * Math.max(0, s) * .5), cy + s * ry]); } return smooth(pts); }
export const svg = (body, vb = "0 0 256 256") => '<svg xmlns="http://www.w3.org/2000/svg" viewBox="' + vb + '" fill="none">\n<title>Super Cage</title>\n' + body + '\n</svg>\n';
export function mark({ ink = "#1F1E1D", clay = "#D97757", glow = "#F6E5DC", v = {} } = {}) {
  const P = { ...defaults, ...v };
  const extra = (P.extra || []).map(([p, w]) => brush(p, w)).join("");
  const lightPath = P.flame ? P.flame : blob(P.light[0], P.light[1], P.lightR, P.lightR * 1.06, .06);
  const cage = brush(P.cage, P.cageW), barL = brush(P.barL, P.barLW), barR = brush(P.barR, P.barRW), base = brush(P.base, P.baseW), foot = P.foot ? brush(P.foot, P.footW) : "";
  return (glow ? '<path d="' + blob(P.light[0], P.light[1], P.glowR) + '" fill="' + glow + '"/>\n' : '') +
    '<path d="' + cage + barL + barR + extra + base + foot + '" fill="' + ink + '"/>\n<path d="' + blob(P.light[0], P.light[1], P.lightR, P.lightR * 1.06, .06) + '" fill="' + clay + '"/>';
}
export const defaults = {
  cage: [[68, 213], [[63, 184], [64, 150], [70, 126]], [[77, 96], [98, 77], [121, 70]], [[141, 64], [152, 53], [148, 39]], [[144, 25], [121, 23], [114, 38]], [[108, 53], [119, 65], [138, 71]], [[162, 78], [181, 97], [186, 127]], [[190, 152], [193, 184], [189, 213]]],
  cageW: [[0, 6], [.06, 8.5], [.28, 10], [.41, 8], [.5, 6], [.59, 8.5], [.7, 13.5], [.86, 13.5], [.95, 10.5], [1, 7]],
  barL: [[124.5, 78], [[111, 104], [104, 156], [103, 211]]], barLW: [[0, 2.4], [.55, 5], [1, 3.8]],
  barR: [[131.5, 78], [[146, 104], [152, 156], [153, 211]]], barRW: [[0, 2.6], [.55, 6], [1, 4.4]],
  base: [[46, 218], [[98, 221], [160, 221], [212, 217]]], baseW: [[0, 4.5], [.15, 9.5], [.55, 12.5], [.88, 9], [1, 4.5]],
  foot: [[84, 234], [[112, 236.5], [146, 236.5], [176, 233]]], footW: [[0, 2.6], [.4, 5.6], [.8, 4.8], [1, 2.6]],
  light: [128, 168], lightR: 17.5, glowR: 39,
};
if (process.argv[2]) fs.writeFileSync(process.argv[2], svg(mark()));
