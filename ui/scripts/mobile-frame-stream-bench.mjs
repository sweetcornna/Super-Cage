// Measures how fast and how heavily the phone page receives a changing remote
// picture: frame interval, delay from change to display and bytes per frame, at a
// given round trip, for a page that changes all the time and for a quiet one. The host is a
// model (no guest, no relay): it needs CAPTURE_MS to look at the picture, and
// delays every request and answer by half the round trip. Run it before and after a change by pointing
// CAGE_FRAME_BASELINE at the mobile-cli directory of the previous source
// (for example from git archive), or compare the page against a host without
// the capability, which is what every older host is.
//
//   node ui/scripts/mobile-frame-stream-bench.mjs
//   CAGE_FRAME_BASELINE=/path/to/old/ui/src/mobile-cli node ui/scripts/mobile-frame-stream-bench.mjs
import {fileURLToPath, pathToFileURL} from "node:url";
import {join} from "node:path";

const RTT_MS = Number(process.env.RTT_MS || 125), CAPTURE_MS = Number(process.env.CAPTURE_MS || 60);
const SCENARIOS = [{name: "busy page, picture changes every 100 ms", changeMs: 100, seconds: Number(process.env.SECONDS || 6)},
  {name: "quiet page, picture changes every 1700 ms", changeMs: 1700, seconds: Number(process.env.QUIET_SECONDS || 10)}];
const JPEG_BYTES = Number(process.env.JPEG_BYTES || 80000);
const id = "0f1e2d3c-4b5a-4968-8776-655443322110";

globalThis.location = {pathname: "/"};
globalThis.sessionStorage = {getItem: () => null};
globalThis.window = {devicePixelRatio: 2};
globalThis.document = Object.assign(new EventTarget(), {hidden: false});
globalThis.ResizeObserver = class {observe() {} disconnect() {}};
// A picture carries its version in two bytes, so the page's output can be matched to the host's.
const versions = new Map();
globalThis.Image = class {set src(value) {this.value = value;} async decode() {await versions.get(this.value);}};
URL.createObjectURL = blob => {
  const url = "blob:bench/" + (versions.size + 1);
  versions.set(url, blob.arrayBuffer().then(buffer => {const a = new Uint8Array(buffer); versions.set(url + "#", a[4] | a[5] << 8);}));
  return url;
};
URL.revokeObjectURL = () => {};
const versionOf = image => image.startsWith("data:") ? (bytes => bytes[4] | bytes[5] << 8)(Buffer.from(image.slice(23, 35), "base64")) : versions.get(image + "#");

const sleep = (ms, signal) => new Promise((resolve, reject) => {
  const timer = setTimeout(resolve, ms);
  signal?.addEventListener("abort", () => {clearTimeout(timer); reject(new DOMException("aborted", "AbortError"));}, {once: true});
});
const tagOf = v => v.toString(16).padStart(32, "0");
const jpeg = v => Buffer.concat([Buffer.from([0xff, 0xd8, 0xff, 0xe0, v & 255, v >> 8]), Buffer.alloc(JPEG_BYTES)]);

function makeHost({capable, desktop, changeMs}) {
  const t0 = Date.now();
  const version = () => Math.floor((Date.now() - t0) / changeMs) + 1;
  const stats = {bytes: [], reads: 0, held: 0};
  const delivered = () => {};
  const meta = v => ({id, url: "https://example.test/", title: "Bench", width: 390, height: 640, document: "doc", focus: "",
    password: false, frame_tag: tagOf(v), window: desktop ? "desktop" : undefined, ...(desktop ? {} : {action_frame: true})});
  const marker = capable ? {"X-Cage-Frame-Stream": "1"} : {};
  const json = (body, count) => {
    const text = JSON.stringify(body);
    if (count) stats.bytes.push(Buffer.byteLength(text));
    return new Response(text, {headers: {"Content-Type": "application/json", ...marker}});
  };
  const full = v => ({...meta(v), image: jpeg(v).toString("base64")});
  const fetchHost = async (path, init = {}) => {
    const {signal} = init, url = new URL(path, "http://bench.test");
    await sleep(RTT_MS / 2, signal);
    const suffix = url.pathname.split("/").pop();
    let response;
    if (suffix === "open") response = json({id, ...(desktop ? {} : {action_frame: true})});
    else if (suffix === "close") response = json({id});
    else if (suffix === "action") response = json(desktop ? {id} : full(version()));
    else {
      stats.reads++;
      const after = url.searchParams.get("after"), wait = Number(url.searchParams.get("wait") || 0), binary = url.searchParams.get("fmt") === "jpeg";
      if (!capable && (binary || wait)) response = new Response("画面请求无效", {status: 400});
      else {
        const deadline = Date.now() + wait;
        let pause = 100;
        for (;;) {
          await sleep(CAPTURE_MS, signal);
          const v = version();
          const tag = desktop && !binary ? undefined : tagOf(v);
          // The original conditional read exists for the browser only.
          if (after && tag === after && (!desktop || binary)) {
            if (wait && Date.now() + pause < deadline) {stats.held++; await sleep(pause, signal); pause = Math.min(pause * 1.5, 1000); continue;}
            response = json({id, frame_tag: after, unchanged: true});
            break;
          }
          if (binary) {
            delivered(v);
            const header = Buffer.from(JSON.stringify(meta(v))).toString("base64url");
            stats.bytes.push(JPEG_BYTES + 6 + header.length);
            response = new Response(jpeg(v), {headers: {"Content-Type": "image/jpeg", "X-Cage-Frame": header, ...marker}});
          } else { delivered(v); response = json(desktop ? (({frame_tag, ...rest}) => rest)(full(v)) : full(v), true); }
          break;
        }
      }
    }
    await sleep(RTT_MS / 2, signal);
    return response;
  };
  return {fetch: fetchHost, stats, t0, version};
}

async function run(label, directory, {capable, desktop}, {changeMs, seconds}) {
  const {connectDisplay} = await import(pathToFileURL(join(directory, "browser-session.mjs")).href + "?" + label);
  const host = makeHost({capable, desktop, changeMs});
  globalThis.fetch = host.fetch;
  const shown = [];
  const session = connectDisplay({clientWidth: 390, clientHeight: 640}, state => {
    const image = state.frame && !state.busy ? state.frame.image : null;
    if (image && image !== shown.at(-1)?.image) shown.push({image, at: Date.now()});
  }, id, true, desktop ? "desktop" : "browser");
  await sleep(seconds * 1000);
  session.close();
  await sleep(RTT_MS + 50);
  const gaps = shown.slice(1).map((f, i) => f.at - shown[i].at).sort((a, b) => a - b);
  const pick = q => gaps.length ? gaps[Math.min(gaps.length - 1, Math.floor(q * gaps.length))] : null;
  // From the moment the host's picture became version v to the moment the page showed it.
  const seen = shown.map(f => ({...f, version: versionOf(f.image)}));
  const lags = seen.filter(f => f.version > 1).map(f => f.at - (host.t0 + (f.version - 1) * changeMs)).sort((a, b) => a - b);
  const repeats = seen.filter((f, i) => i && f.version === seen[i - 1].version).length;
  const bytes = host.stats.bytes;
  return {label, frames: shown.length, repaintsOfTheSamePicture: repeats, fps: +(shown.length / seconds).toFixed(2), intervalMsP50: pick(.5), intervalMsP95: pick(.95),
    bytesPerFrame: bytes.length ? Math.round(bytes.reduce((a, b) => a + b, 0) / bytes.length) : null,
    readsToHost: host.stats.reads, hostLooks: host.stats.reads + host.stats.held,
    changeToDisplayMsP50: lags.length ? Math.round(lags[Math.floor(lags.length / 2)]) : null, changeToDisplayMsMax: lags.length ? Math.round(lags.at(-1)) : null};
}

const here = fileURLToPath(new URL("../src/mobile-cli/", import.meta.url));
const baseline = process.env.CAGE_FRAME_BASELINE;
const scenarios = [];
for (const scenario of SCENARIOS) {
  const rows = [];
  for (const desktop of [false, true]) {
    const surface = desktop ? "desktop" : "browser";
    if (baseline) rows.push(await run("before: previous page, " + surface, baseline, {capable: false, desktop}, scenario));
    rows.push(await run("older host: this page, " + surface, here, {capable: false, desktop}, scenario));
    rows.push(await run("after: this page, capable host, " + surface, here, {capable: true, desktop}, scenario));
  }
  scenarios.push({scenario: scenario.name, rows});
}
console.log(JSON.stringify({rttMs: RTT_MS, captureMs: CAPTURE_MS, jpegBytes: JPEG_BYTES, scenarios}, null, 2));
