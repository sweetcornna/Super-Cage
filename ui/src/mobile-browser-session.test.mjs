import test from "node:test";
import assert from "node:assert/strict";

// browser-session.mjs and transport.mjs touch these globals; a phone page has them.
const live = new Set();
let objectUrls = 0;
globalThis.location = {pathname: "/"};
globalThis.sessionStorage = {getItem: () => null};
globalThis.window = {devicePixelRatio: 2};
globalThis.document = Object.assign(new EventTarget(), {hidden: false});
globalThis.ResizeObserver = class {observe() {} disconnect() {}};
globalThis.Image = class {
  set src(value) { this.value = value; }
  get src() { return this.value; }
  async decode() { if (!live.has(this.value)) throw new Error("decoded a revoked or unknown URL"); }
};
URL.createObjectURL = blob => { assert.ok(blob instanceof Blob && blob.type === "image/jpeg"); const url = "blob:test/" + ++objectUrls; live.add(url); return url; };
URL.revokeObjectURL = url => { live.delete(url); };
const { connectDisplay, frameMeta } = await import("./mobile-cli/browser-session.mjs");

const id = "0f1e2d3c-4b5a-4968-8776-655443322110";
const tagOf = version => version.toString(16).padStart(32, "0");
const jpeg = version => Uint8Array.from([0xff, 0xd8, 0xff, 0xe0, version, ...new Uint8Array(200)]);
const base64url = text => Buffer.from(text).toString("base64url");
const tick = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(check, what, limit = 3000) {
  for (const end = Date.now() + limit; !check(); await tick(5)) if (Date.now() > end) assert.fail("timed out: " + what);
}

// A host that answers like Cage does: the original JSON reads, and, when asked
// for it, JPEG bodies held until the picture changes.
function host({marker = true, negotiated = true, hold = 5000, refuse = 0, instant = false} = {}) {
  const state = {version: 1, seq: 0, requests: [], aborted: 0, waiters: new Set(), reads: [], bytes: []};
  const meta = (version, frameSeq) => ({id, url: "https://example.test/文件", title: "标题", width: 390, height: 640,
    document: "doc", focus: "", password: false, frame_tag: tagOf(version), action_frame: true, ...(frameSeq ? {frame_seq: frameSeq} : {})});
  const json = (value, headers = {}) => new Response(JSON.stringify(value), {headers: {"Content-Type": "application/json", ...(marker ? {"X-Cage-Frame-Stream": "1"} : {}), ...headers}});
  const picture = (version, frameSeq) => new Response(jpeg(version), {headers: {"Content-Type": "image/jpeg", "X-Cage-Frame-Stream": "1", "X-Cage-Frame": base64url(JSON.stringify(meta(version, frameSeq)))}});
  const release = () => { for (const wake of [...state.waiters]) wake(); };
  state.bump = () => { state.version++; release(); };
  state.respondWith = undefined; // optional override for a negotiated read: () => Response
  state.fetch = async (path, init = {}) => {
    const url = new URL(path, "http://phone.test");
    const call = {path: url.pathname, query: url.search, method: init.method || "GET", body: init.body ? JSON.parse(init.body) : undefined, signal: init.signal, at: Date.now()};
    state.requests.push(call);
    const suffix = url.pathname.split("/").pop();
    if (suffix === "open") return json({id, action_frame: true});
    if (suffix === "close") return json({id});
    if (suffix === "action") {
      state.version++;
      release();
      return json({...meta(state.version), image: Buffer.from(jpeg(state.version)).toString("base64")});
    }
    assert.equal(suffix, "frame");
    const after = url.searchParams.get("after"), wait = url.searchParams.get("wait"), fmt = url.searchParams.get("fmt");
    state.reads.push({query: url.search, at: Date.now()});
    if (fmt || wait) {
      assert.ok(negotiated, "a negotiated read reached a host that does not understand it");
      if (refuse) return new Response("画面请求无效", {status: refuse, headers: {"Content-Type": "text/plain"}});
      const forced = state.respondWith?.();
      if (forced) return forced;
      if (after === tagOf(state.version)) {
        if (instant || !wait) return json({id, frame_tag: after, unchanged: true});
        await new Promise((resolve, reject) => {
          const timer = setTimeout(done, hold);
          function done() { clearTimeout(timer); state.waiters.delete(done); init.signal?.removeEventListener("abort", abort); resolve(); }
          function abort() { clearTimeout(timer); state.waiters.delete(done); state.aborted++; reject(new DOMException("aborted", "AbortError")); }
          state.waiters.add(done);
          init.signal?.addEventListener("abort", abort, {once: true});
        });
        if (after === tagOf(state.version)) return json({id, frame_tag: after, unchanged: true});
      }
      const body = picture(state.version, ++state.seq);
      state.bytes.push(jpeg(state.version).length);
      return body;
    }
    if (after) {
      assert.equal(after, tagOf(state.version), "a stale tag");
      return json({id, frame_tag: after, unchanged: true});
    }
    const body = {...meta(state.version), image: Buffer.from(jpeg(state.version)).toString("base64")};
    state.bytes.push(JSON.stringify(body).length);
    return json(body);
  };
  return state;
}

function open(t, h, {tool = "browser"} = {}) {
  globalThis.fetch = h.fetch;
  const updates = [];
  const session = connectDisplay({clientWidth: 390, clientHeight: 640}, state => updates.push(state), id, true, tool);
  // A session left running by a failed test must not read from the next test's host.
  t.after(() => { session.close(); document.hidden = false; });
  return {session, updates, frames: () => updates.filter(u => u.frame && !u.busy).map(u => u.frame),
    shown: () => updates.filter(u => u.frame).at(-1)?.frame};
}

test("frame metadata round-trips UTF-8 and rejects anything that is not base64url JSON", () => {
  assert.deepEqual(frameMeta(base64url(JSON.stringify({title: "标题：画面", n: 1}))), {title: "标题：画面", n: 1});
  for (const bad of ["", "a b", "!!", base64url("[1]"), base64url("not json"), base64url("null"), base64url(Buffer.from([0xff, 0xfe]).toString("latin1"))]) {
    assert.throws(() => frameMeta(bad), /画面无效/, JSON.stringify(bad));
  }
});

test("a host that does not announce held reads keeps the original polling, now with object URLs", async t => {
  const h = host({marker: false, negotiated: false});
  const {session, frames} = open(t, h);
  await until(() => h.reads.length >= 3, "three polls");
  session.close();
  assert.equal(h.reads[0].query, "", "the first read is always the original one");
  assert.ok(h.reads.slice(1).every(r => r.query === "?after=" + tagOf(1)), "later reads are the conditional ones");
  assert.ok(frames()[0].image.startsWith("blob:test/"), "the picture is no longer a data: URL");
  assert.equal(frames()[0].title, "标题");
  assert.equal(live.size, 0, "closing releases every picture");
});

test("an announced host gets negotiated reads that are held and answered as JPEG bodies", async t => {
  const h = host();
  const {session, frames, shown} = open(t, h);
  await until(() => h.reads.length >= 2, "the negotiated read");
  assert.equal(h.reads[0].query, "");
  assert.equal(h.reads[1].query, "?after=" + tagOf(1) + "&wait=12000&fmt=jpeg");
  const first = shown();
  assert.equal(first.title, "标题");
  assert.equal(first.url, "https://example.test/文件");
  await tick(150);
  assert.equal(h.reads.length, 2, "a held read is not asked again while it is held");
  const changedAt = Date.now();
  h.bump();
  await until(() => shown().frame_tag === tagOf(2), "the new picture");
  assert.ok(Date.now() - changedAt < 400, "a new picture is shown as soon as the host answers");
  assert.ok(shown().image.startsWith("blob:test/") && shown().image !== first.image);
  await until(() => h.reads.length >= 3, "the next held read");
  assert.equal(h.reads[2].query, "?after=" + tagOf(2) + "&wait=12000&fmt=jpeg", "the next read is asked at once");
  assert.ok(live.size <= 2, "only the picture on screen and the one before it stay alive: " + live.size);
  session.close();
  assert.equal(live.size, 0);
  assert.ok(frames().length >= 2);
});

test("an input is sent at once while a read is held and the held read is given up", async t => {
  const h = host();
  const {session, shown} = open(t, h);
  await until(() => h.reads.length >= 2, "the held read");
  const held = h.requests.at(-1);
  assert.equal(held.signal.aborted, false);
  const before = Date.now();
  assert.equal(await session.send({type: "tap", x: 4, y: 5}, shown()), true);
  assert.ok(Date.now() - before < 300, "a tap waited behind the held read");
  assert.equal(held.signal.aborted, true, "the held read was released for the input");
  assert.equal(h.requests.filter(r => r.path.endsWith("/action")).length, 1);
  await until(() => h.reads.length >= 3, "a new held read after the input");
  assert.match(h.reads.at(-1).query, /wait=12000&fmt=jpeg$/);
  session.close();
});

test("a frame the host captured earlier than the one shown is dropped without stopping the reads", async t => {
  const h = host({hold: 60});
  const {session, frames, shown} = open(t, h);
  await until(() => h.reads.length >= 2, "the first held read");
  h.bump();
  await until(() => shown().frame_seq === 1, "frame 1");
  let late = true;
  h.respondWith = () => {
    if (!late) return undefined;
    late = false;
    // Captured earlier (seq 0 < 1) and a different picture.
    return new Response(jpeg(9), {headers: {"Content-Type": "image/jpeg", "X-Cage-Frame-Stream": "1",
      "X-Cage-Frame": base64url(JSON.stringify({id, width: 390, height: 640, document: "doc", frame_tag: tagOf(9), frame_seq: 0}))}});
  };
  h.bump();
  await until(() => h.reads.length >= 5, "reads after the dropped frame");
  assert.ok(frames().every(f => f.frame_tag !== tagOf(9)), "the late frame was shown");
  session.close();
});

test("a link that refuses the negotiated read sends the session back to the original polling without an error", async t => {
  const h = host({refuse: 404});
  const {session, updates} = open(t, h);
  await until(() => h.reads.length >= 4, "reads after the refusal");
  session.close();
  assert.equal(h.reads.filter(r => /fmt=/.test(r.query)).length, 1, "the refused form is tried once");
  assert.ok(h.reads.slice(2).every(r => !/fmt=|wait=/.test(r.query)));
  assert.ok(updates.every(u => !u.error), "a refusal by an older host is not a failure");
});

test("a host that answers a held read at once is not treated as holding it", async t => {
  const h = host({instant: true});
  const {session} = open(t, h);
  await until(() => h.reads.length >= 7, "reads without a hold", 6000);
  session.close();
  const withWait = h.reads.filter(r => /wait=/.test(r.query)).length;
  assert.ok(withWait <= 2, "kept asking for holds: " + withWait);
  assert.ok(h.reads.at(-1).query.includes("fmt=jpeg") && !h.reads.at(-1).query.includes("wait="));
});

test("a hidden page holds no read open and starts none, and a shown page starts over", async t => {
  const h = host();
  const {session, shown} = open(t, h);
  await until(() => h.reads.length >= 2, "the held read");
  const held = h.requests.at(-1);
  document.hidden = true;
  document.dispatchEvent(new Event("visibilitychange"));
  assert.equal(held.signal.aborted, true);
  assert.equal(live.size, 0, "a hidden page keeps no picture");
  const count = h.requests.length;
  h.bump();
  await tick(300);
  assert.equal(h.requests.length, count, "a hidden page made a request");
  document.hidden = false;
  document.dispatchEvent(new Event("visibilitychange"));
  await until(() => h.reads.length >= 3 && shown(), "reads after the page is shown again");
  // The picture was dropped with the hidden page, so there is no tag to wait after.
  assert.equal(h.reads.at(-1).query, "?fmt=jpeg");
  await until(() => h.reads.at(-1).query.includes("after=") , "a held read after the first picture");
  session.close();
});

test("closing a session aborts its held read and releases its pictures", async t => {
  const h = host();
  const {session} = open(t, h);
  await until(() => h.reads.length >= 2, "the held read");
  const held = h.requests.at(-1);
  session.close();
  assert.equal(held.signal.aborted, true);
  assert.equal(live.size, 0);
});
