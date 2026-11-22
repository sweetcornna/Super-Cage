// Offline brush renderer for the portal drawings: turns centerline doodles into filled, variable-width
// outlines (thick-thin by stroke direction, tapered ends, slight wobble) to match the brush-drawn mark.
// Usage: node brush.mjs <in.json> <out.json>   in: { name: { box, strokes: [{ d, w, o?, k? }] } }
import fs from "node:fs";
const [input, output] = process.argv.slice(2);
const src = JSON.parse(fs.readFileSync(input, "utf8"));

function tokenize(d) {
  return [...d.matchAll(/[MmLlHhVvCcSsQqZz]|-?(?:\d*\.\d+|\d+)(?:e-?\d+)?/g)].map((m) => m[0]);
}
// Returns subpaths: { pts: [[x,y],...], closed }
function parse(d) {
  const t = tokenize(d);
  let i = 0, cmd = "", x = 0, y = 0, sx = 0, sy = 0, lcx = null, lcy = null;
  const subs = [];
  let cur = null;
  const num = () => parseFloat(t[i++]);
  const bez = (p0, p1, p2, p3) => {
    const len = Math.hypot(p1[0]-p0[0],p1[1]-p0[1]) + Math.hypot(p2[0]-p1[0],p2[1]-p1[1]) + Math.hypot(p3[0]-p2[0],p3[1]-p2[1]);
    const n = Math.max(4, Math.ceil(len / 0.4));
    for (let k = 1; k <= n; k++) {
      const s = k / n, u = 1 - s;
      cur.pts.push([u*u*u*p0[0] + 3*u*u*s*p1[0] + 3*u*s*s*p2[0] + s*s*s*p3[0], u*u*u*p0[1] + 3*u*u*s*p1[1] + 3*u*s*s*p2[1] + s*s*s*p3[1]]);
    }
  };
  const line = (nx, ny) => {
    const len = Math.hypot(nx - x, ny - y);
    const n = Math.max(1, Math.ceil(len / 0.4));
    for (let k = 1; k <= n; k++) cur.pts.push([x + (nx - x) * k / n, y + (ny - y) * k / n]);
    x = nx; y = ny;
  };
  while (i < t.length) {
    if (/[A-Za-z]/.test(t[i])) cmd = t[i++];
    const rel = cmd === cmd.toLowerCase();
    switch (cmd.toUpperCase()) {
      case "M": { const nx = num() + (rel ? x : 0), ny = num() + (rel ? y : 0); x = sx = nx; y = sy = ny; cur = { pts: [[x, y]], closed: false }; subs.push(cur); cmd = rel ? "l" : "L"; lcx = null; break; }
      case "L": { const nx = num() + (rel ? x : 0), ny = num() + (rel ? y : 0); line(nx, ny); lcx = null; break; }
      case "H": { const nx = num() + (rel ? x : 0); line(nx, y); lcx = null; break; }
      case "V": { const ny = num() + (rel ? y : 0); line(x, ny); lcx = null; break; }
      case "C": { const a = [num() + (rel ? x : 0), num() + (rel ? y : 0)], b = [num() + (rel ? x : 0), num() + (rel ? y : 0)], e = [num() + (rel ? x : 0), num() + (rel ? y : 0)]; bez([x, y], a, b, e); x = e[0]; y = e[1]; lcx = b[0]; lcy = b[1]; break; }
      case "S": { const a = lcx === null ? [x, y] : [2*x - lcx, 2*y - lcy]; const b = [num() + (rel ? x : 0), num() + (rel ? y : 0)], e = [num() + (rel ? x : 0), num() + (rel ? y : 0)]; bez([x, y], a, b, e); x = e[0]; y = e[1]; lcx = b[0]; lcy = b[1]; break; }
      case "Q": { const q = [num() + (rel ? x : 0), num() + (rel ? y : 0)], e = [num() + (rel ? x : 0), num() + (rel ? y : 0)]; bez([x, y], [x + 2/3*(q[0]-x), y + 2/3*(q[1]-y)], [e[0] + 2/3*(q[0]-e[0]), e[1] + 2/3*(q[1]-e[1])], e); x = e[0]; y = e[1]; lcx = null; break; }
      case "Z": { cur.closed = true; if (Math.hypot(x - sx, y - sy) > 0.05) line(sx, sy); x = sx; y = sy; lcx = null; break; }
      default: throw new Error("unsupported " + cmd);
    }
  }
  return subs;
}

function resample(pts, step) {
  const out = [pts[0]];
  let acc = 0;
  for (let k = 1; k < pts.length; k++) {
    let [ax, ay] = pts[k - 1];
    const [bx, by] = pts[k];
    let seg = Math.hypot(bx - ax, by - ay);
    while (acc + seg >= step) {
      const r = (step - acc) / seg;
      ax = ax + (bx - ax) * r; ay = ay + (by - ay) * r;
      out.push([ax, ay]);
      seg = Math.hypot(bx - ax, by - ay);
      acc = 0;
    }
    acc += seg;
  }
  const last = pts[pts.length - 1];
  if (Math.hypot(last[0] - out[out.length-1][0], last[1] - out[out.length-1][1]) > step * 0.3) out.push(last);
  return out;
}

const smooth = (e0, e1, v) => { const s = Math.min(1, Math.max(0, (v - e0) / (e1 - e0))); return s * s * (3 - 2 * s); };
const NIB = (38 * Math.PI) / 180;

function outline(sub, w, seed) {
  const closed = sub.closed;
  let pts = resample(sub.pts, 0.6);
  if (closed && pts.length > 2 && Math.hypot(pts[0][0]-pts.at(-1)[0], pts[0][1]-pts.at(-1)[1]) < 0.4) pts.pop();
  const n = pts.length;
  let total = 0;
  const cum = [0];
  for (let k = 1; k < n; k++) { total += Math.hypot(pts[k][0]-pts[k-1][0], pts[k][1]-pts[k-1][1]); cum.push(total); }
  if (total < 1.2) return { dot: true, cx: (pts[0][0] + pts.at(-1)[0]) / 2, cy: (pts[0][1] + pts.at(-1)[1]) / 2, r: w * 0.62 };
  const left = [], right = [], widths = [];
  for (let k = 0; k < n; k++) {
    const prev = pts[closed ? (k - 1 + n) % n : Math.max(0, k - 1)];
    const next = pts[closed ? (k + 1) % n : Math.min(n - 1, k + 1)];
    let tx = next[0] - prev[0], ty = next[1] - prev[1];
    const tl = Math.hypot(tx, ty) || 1; tx /= tl; ty /= tl;
    const theta = Math.atan2(ty, tx);
    const s = cum[k];
    let f = 0.5 + 0.5 * Math.abs(Math.sin(theta - NIB));
    f *= 1 + 0.05 * Math.sin(s * 0.23 + seed) + 0.03 * Math.sin(s * 0.61 + seed * 2.1);
    if (!closed) {
      const ls = Math.min(4, total * 0.22), le = Math.min(8, total * 0.32);
      f *= (0.5 + 0.5 * smooth(0, ls, s)) * (0.28 + 0.72 * smooth(0, le, total - s));
    }
    const half = (w * 1.28 * f) / 2;
    widths.push(half);
    left.push([pts[k][0] - ty * half, pts[k][1] + tx * half]);
    right.push([pts[k][0] + ty * half, pts[k][1] - tx * half]);
  }
  if (closed) return { rings: [left, right.reverse()] };
  // Round caps
  const cap = (c, from, to, r) => {
    const out = [];
    let a0 = Math.atan2(from[1] - c[1], from[0] - c[0]);
    let a1 = Math.atan2(to[1] - c[1], to[0] - c[0]);
    let da = a1 - a0;
    while (da > 0) da -= 2 * Math.PI;
    const steps = 6;
    for (let k = 1; k < steps; k++) { const a = a0 + (da * k) / steps; out.push([c[0] + Math.cos(a) * r, c[1] + Math.sin(a) * r]); }
    return out;
  };
  const ring = [...left, ...cap(pts[n-1], left[n-1], right[n-1], widths[n-1]), ...right.slice().reverse(), ...cap(pts[0], right[0], left[0], widths[0])];
  return { rings: [ring] };
}

function rdp(points, eps) {
  if (points.length < 3) return points;
  const [a, b] = [points[0], points[points.length - 1]];
  let idx = -1, dmax = 0;
  const dx = b[0] - a[0], dy = b[1] - a[1], len = Math.hypot(dx, dy) || 1;
  for (let k = 1; k < points.length - 1; k++) {
    const d = Math.abs(dy * points[k][0] - dx * points[k][1] + b[0] * a[1] - b[1] * a[0]) / len;
    if (d > dmax) { dmax = d; idx = k; }
  }
  if (dmax <= eps) return [a, b];
  return [...rdp(points.slice(0, idx + 1), eps).slice(0, -1), ...rdp(points.slice(idx), eps)];
}

const f1 = (v) => { const r = Math.round(v * 10) / 10; return (Object.is(r, -0) ? 0 : r).toString().replace(/^0\./, ".").replace(/^-0\./, "-."); };
function ringPath(ring) {
  // Split the closed ring at its farthest point so the simplifier has a real chord on each half.
  let far = 0, fd = 0;
  for (let k = 1; k < ring.length; k++) { const d = Math.hypot(ring[k][0] - ring[0][0], ring[k][1] - ring[0][1]); if (d > fd) { fd = d; far = k; } }
  const simple = [...rdp(ring.slice(0, far + 1), 0.09).slice(0, -1), ...rdp([...ring.slice(far), ring[0]], 0.09).slice(0, -1)];
  let out = "M" + f1(simple[0][0]) + " " + f1(simple[0][1]) + "l";
  let [px, py] = [Math.round(simple[0][0]*10)/10, Math.round(simple[0][1]*10)/10];
  const parts = [];
  for (let k = 1; k < simple.length; k++) {
    const x = Math.round(simple[k][0]*10)/10, y = Math.round(simple[k][1]*10)/10;
    parts.push(f1(x - px) + (x - px < 0 || (y - py) >= 0 ? (y - py < 0 ? "" : " ") : " ") + f1(y - py));
    px = x; py = y;
  }
  // join with minimal separators
  out += parts.map((p, k) => (k && !p.startsWith("-") ? " " : "") + p).join("");
  return out + "z";
}
const circlePath = (cx, cy, r) => "M" + f1(cx - r) + " " + f1(cy) + "a" + f1(r) + " " + f1(r) + " 0 1 0 " + f1(2 * r) + " 0a" + f1(r) + " " + f1(r) + " 0 1 0 " + f1(-2 * r) + " 0z";

const result = {};
let seed = 1;
for (const [name, spec] of Object.entries(src)) {
  const shapes = [];
  for (const stroke of spec.strokes) {
    seed += 1.7;
    if (stroke.k === "fill") { shapes.push({ d: stroke.d, k: "clay", keep: true }); continue; }
    let d = "";
    for (const sub of parse(stroke.d)) {
      const o = outline(sub, stroke.w, seed += 0.9);
      if (o.dot) d += circlePath(o.cx, o.cy, o.r);
      else for (const ring of o.rings) d += ringPath(ring);
    }
    const shape = { d };
    if (stroke.o) shape.o = stroke.o;
    if (stroke.k === "stroke") shape.k = "clay";
    shapes.push(shape);
  }
  result[name] = { box: spec.box, parts: spec.parts, shapes };
}
fs.writeFileSync(output, JSON.stringify(result));
for (const [name, v] of Object.entries(result)) console.log(name, JSON.stringify(v).length);
