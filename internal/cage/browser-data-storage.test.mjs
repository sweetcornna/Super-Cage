import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, unlinkSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { liveKeys, snappyDecompress, storageKeyOrigin, storageOrigins } from "./browser-data-storage.mjs";
import { BrowserData, builtInOrigins } from "./browser-data.mjs";

// Minimal writers of Chromium's LevelDB files: a log of write batches and a
// table of data blocks (raw or snappy) with an index block and a footer.
const varint = value => { const out = []; while (value >= 0x80) { out.push((value & 0x7f) | 0x80); value = Math.floor(value / 128); } out.push(value); return Buffer.from(out); };
const internalKey = (key, sequence, live) => Buffer.concat([Buffer.from(key, "latin1"), (() => { const b = Buffer.alloc(8); b.writeBigUInt64LE((BigInt(sequence) << 8n) | (live ? 1n : 0n)); return b; })()]);
function blockOf(entries) {
  const parts = [];
  for (const [key, value] of entries) parts.push(varint(0), varint(key.length), varint(value.length), key, value);
  const restarts = Buffer.alloc(8); restarts.writeUInt32LE(0, 0); restarts.writeUInt32LE(1, 4);
  return Buffer.concat([...parts, restarts]);
}
// Snappy with a literal and a two-byte-offset copy when the input repeats.
export function snappyCompress(input) {
  const half = input.length / 2;
  const literal = (bytes) => bytes.length <= 60 ? Buffer.concat([Buffer.from([(bytes.length - 1) << 2]), bytes])
    : bytes.length <= 256 ? Buffer.concat([Buffer.from([60 << 2, bytes.length - 1]), bytes])
    : Buffer.concat([Buffer.from([61 << 2, (bytes.length - 1) & 0xff, (bytes.length - 1) >> 8]), bytes]);
  if (Number.isInteger(half) && half > 0 && half <= 64 && input.subarray(0, half).equals(input.subarray(half)))
    return Buffer.concat([varint(input.length), literal(input.subarray(0, half)), Buffer.from([((half - 1) << 2) | 2, half & 0xff, half >> 8])]);
  return Buffer.concat([varint(input.length), literal(input)]);
}
export function tableFile(blocks) {
  const chunks = [];
  let offset = 0;
  const put = (contents, compressed) => {
    const body = compressed ? snappyCompress(contents) : contents;
    const handle = Buffer.concat([varint(offset), varint(body.length)]);
    chunks.push(body, Buffer.from([compressed ? 1 : 0, 0, 0, 0, 0]));
    offset += body.length + 5;
    return handle;
  };
  const index = [];
  for (const { entries, compressed, damaged } of blocks) {
    const keyed = entries.map(([key, value, sequence = 1, live = true]) => [internalKey(key, sequence, live), Buffer.from(value)]);
    if (damaged) {
      // Unreadable snappy: reading this block would throw.
      const handle = Buffer.concat([varint(offset), varint(3)]);
      chunks.push(Buffer.from([0xff, 0xff, 0xff]), Buffer.from([1, 0, 0, 0, 0]));
      offset += 8;
      index.push([keyed.at(-1)[0], handle]);
      continue;
    }
    index.push([keyed.at(-1)[0], put(blockOf(keyed), compressed)]);
  }
  const meta = put(blockOf([]), false);
  const indexHandle = put(blockOf(index), false);
  const footer = Buffer.alloc(48);
  Buffer.concat([meta, indexHandle]).copy(footer);
  footer.writeUInt32LE(0x8b80fb57, 40); footer.writeUInt32LE(0xdb477524, 44);
  return Buffer.concat([...chunks, footer]);
}
export function logFile(batches) {
  const records = [];
  for (const { sequence, ops } of batches) {
    const head = Buffer.alloc(12); head.writeBigUInt64LE(BigInt(sequence)); head.writeUInt32LE(ops.length, 8);
    const body = [head];
    for (const [key, value] of ops) {
      body.push(Buffer.from([value === null ? 0 : 1]), varint(Buffer.byteLength(key, "latin1")), Buffer.from(key, "latin1"));
      if (value !== null) body.push(varint(value.length), Buffer.from(value));
    }
    const data = Buffer.concat(body);
    const header = Buffer.alloc(7); header.writeUInt16LE(data.length, 4); header[6] = 1;
    records.push(header, data);
  }
  return Buffer.concat(records);
}

// Chromium's CacheStorageIndex protobuf: field 1 one cache (its name, chosen by
// the page, and its folder), field 2 the origin, field 3 the storage key.
const field = (number, value) => { const bytes = Buffer.isBuffer(value) ? value : Buffer.from(value, "latin1"); return Buffer.concat([varint(number * 8 + 2), varint(bytes.length), bytes]); };
export const cacheIndex = (storageKey, names = ["v1"]) => Buffer.concat([...names.map(name => field(1, Buffer.concat([field(1, name), field(2, "dir-" + name.length)]))), field(2, storageKey), field(3, storageKey)]);

const profile = () => mkdtempSync(join(tmpdir(), "cage-storage-"));

test("snappy: literals and copies", () => {
  const input = Buffer.from("META:https://a.example".repeat(2));
  assert.deepEqual(snappyDecompress(snappyCompress(input)), input);
  const long = Buffer.from("x".repeat(300));
  assert.deepEqual(snappyDecompress(snappyCompress(long)), long);
  assert.throws(() => snappyDecompress(Buffer.from([5, 0x0a, 0x10, 0])), /snappy/);
});

test("live keys: tables then logs, the newest entry of a key decides, data blocks past the prefix are skipped", async () => {
  const dir = join(profile(), "leveldb");
  mkdirSync(dir);
  writeFileSync(join(dir, "000005.ldb"), tableFile([
    { compressed: true, entries: [["META:http://app.keep.localhost:8080", "m"], ["META:https://gone.example", "m", 3]] },
    { compressed: false, entries: [["VERSION", "1"], ["_https://gone.example\x00k", "v"]] },
    { damaged: true, entries: [["_https://gone.example\x00z", "v"]] },
  ]));
  writeFileSync(join(dir, "000007.log"), logFile([
    { sequence: 10, ops: [["META:https://gone.example", null], ["META:https://new.example", "m"]] },
  ]));
  const keys = await liveKeys(dir, ["META:"]);
  assert.deepEqual([...keys].sort(), ["META:http://app.keep.localhost:8080", "META:https://new.example"]);
  assert.deepEqual([...await liveKeys(join(dir, "missing"), ["META:"])], []);
  writeFileSync(join(dir, "000008.ldb"), Buffer.from("not a table"));
  await assert.rejects(liveKeys(dir, ["META:"]), /leveldb/);
});

test("storage origins: Local Storage, service workers, IndexedDB and Cache Storage, partitioned keys by their origin", async () => {
  const root = profile();
  const ls = join(root, "Local Storage", "leveldb");
  mkdirSync(ls, { recursive: true });
  writeFileSync(join(ls, "000003.log"), logFile([{ sequence: 1, ops: [
    ["META:http://app.keep.localhost:8080", "m"], ["META:https://third.example^0https://top.example", "m"], ["META:chrome-extension://abc", "m"],
  ] }]));
  const sw = join(root, "Service Worker", "Database");
  mkdirSync(sw, { recursive: true });
  writeFileSync(join(sw, "000003.log"), logFile([{ sequence: 1, ops: [["INITDATA_UNIQUE_ORIGIN:https://worker.example/", ""], ["REG:https://worker.example/\x001", "r"]] }]));
  mkdirSync(join(root, "IndexedDB", "https_db.example_0.indexeddb.leveldb"), { recursive: true });
  mkdirSync(join(root, "Service Worker", "CacheStorage", "abc123"), { recursive: true });
  writeFileSync(join(root, "Service Worker", "CacheStorage", "abc123", "index.txt"), cacheIndex("https://cached.example:8443/", ["name"]));
  const origins = await storageOrigins(root);
  assert.deepEqual([...origins].sort(), ["http://app.keep.localhost:8080", "https://cached.example:8443", "https://db.example", "https://third.example", "https://worker.example"]);
  assert.equal(storageKeyOrigin("https://a.example/"), "https://a.example");
  assert.equal(storageKeyOrigin("https://www.192.168.1.10"), "");
  assert.equal(storageKeyOrigin("file:///x"), "");
});

function data(root, evaluations, value = {}, methods = []) {
  const cdp = async (method, params = {}) => {
    methods.push(method);
    if (method === "Runtime.evaluate") {
      const input = JSON.parse(params.expression.slice("cageBrowsingData(".length, -1));
      evaluations.push(input);
      return { result: { value: value[input.op] || { probe: { browsingData: true, history: true }, remove: { ok: true }, "history-origins": { origins: {}, complete: true } }[input.op] } };
    }
    if (method === "Storage.getCookies") return { cookies: [{ name: "sid", value: "x", domain: ".keep.localhost", path: "/", size: 5 }] };
    if (method === "Storage.getUsageAndQuota") return { usage: 100 };
    return {};
  };
  return new BrowserData({ cdp, control: async () => "control", session: async id => "session-" + id, tabs: () => [],
    downloads: { snapshot: () => [], forget: () => ({ removed: 0, kept: 0 }) }, profile: root, protectedOrigins: () => builtInOrigins(47800), sciencePort: () => 47800 });
}
async function finish(data, started) {
  for (let i = 0; i < 200; i++) {
    const { job } = await data.request({ mode: "job", id: started.job.id });
    if (job.state !== "running") return job;
    await new Promise(resolve => setTimeout(resolve, 5));
  }
  throw new Error("job did not finish");
}

test("a kept domain keeps a subdomain's localStorage with no tab and no history", async () => {
  const root = profile();
  const ls = join(root, "Default", "Local Storage", "leveldb");
  mkdirSync(ls, { recursive: true });
  writeFileSync(join(ls, "000003.log"), logFile([{ sequence: 1, ops: [["META:http://app.keep.localhost:8080", "m"], ["META:https://other.example", "m"]] }]));
  const evaluations = [];
  const browser = data(root, evaluations);
  const job = await finish(browser, await browser.request({ mode: "clear", since: 0, types: ["history", "site", "cache"], keep: ["keep.localhost"] }));
  assert.equal(job.state, "done");
  const site = evaluations.find(input => input.op === "remove" && input.types.localStorage);
  assert.ok(site.excludeOrigins.includes("http://app.keep.localhost:8080"));
  assert.ok(!site.excludeOrigins.includes("https://other.example"));
  const list = await browser.request({ mode: "sites", sort: "name" });
  assert.ok(list.sites.some(item => item.site === "other.example" && item.origins === 1), "an origin with only localStorage is listed");
});

test("when the profile's storage cannot be read, a clear with kept sites deletes nothing", async () => {
  const root = profile();
  const ls = join(root, "Default", "Local Storage", "leveldb");
  mkdirSync(ls, { recursive: true });
  writeFileSync(join(ls, "000009.ldb"), Buffer.from("damaged"));
  const evaluations = [];
  const browser = data(root, evaluations);
  const job = await finish(browser, await browser.request({ mode: "clear", since: 0, types: ["history", "site"], keep: ["keep.localhost"] }));
  assert.equal(job.state, "failed");
  assert.match(job.error, /保持登录/);
  assert.equal(evaluations.filter(input => input.op === "remove").length, 0);
  const cacheOnly = await finish(browser, await browser.request({ mode: "clear", since: 0, types: ["cache"], keep: ["keep.localhost"] }));
  assert.equal(cacheOnly.state, "done", "the HTTP cache holds no sign-in: a cache clear still runs");
  const unkept = await finish(browser, await browser.request({ mode: "clear", since: 0, types: ["site"], keep: [] }));
  assert.equal(unkept.state, "done", "nothing kept, nothing to protect");
  unlinkSync(join(ls, "000009.ldb"));
});

test("review round 2, N1: cache names a page chooses never become origins; a damaged index is unreadable", async () => {
  const root = profile();
  const caches = join(root, "Service Worker", "CacheStorage");
  mkdirSync(join(caches, "evil"), { recursive: true });
  writeFileSync(join(caches, "evil", "index.txt"), cacheIndex("http://evil.localhost:8080/", ["https://bank.example", "https://app.kept.localhost", "http://victim.localhost:1234"]));
  mkdirSync(join(caches, "framed"), { recursive: true });
  writeFileSync(join(caches, "framed", "index.txt"), cacheIndex("https://framed.example/^0https://top.example", ["https://bank.example"]));
  assert.deepEqual([...await storageOrigins(root)].sort(), ["http://evil.localhost:8080", "https://framed.example"]);
  mkdirSync(join(caches, "broken"), { recursive: true });
  writeFileSync(join(caches, "broken", "index.txt"), Buffer.from([0x12, 0x40, 0x68, 0x74]));
  await assert.rejects(storageOrigins(root), /cache storage/);
});

test("review round 2, N1: one read returns a bounded number of origins; a clear's read fails closed past it", async () => {
  const root = profile();
  const ls = join(root, "Local Storage", "leveldb");
  mkdirSync(ls, { recursive: true });
  const ops = [];
  for (let i = 0; i < 40; i++) ops.push(["META:https://s" + i + ".page.example", "m"]);
  for (let i = 0; i < 3; i++) ops.push(["META:https://a" + i + ".keep.example", "m"]);
  writeFileSync(join(ls, "000003.log"), logFile([{ sequence: 1, ops }]));
  assert.equal((await storageOrigins(root, { limit: 10 })).size, 10, "a listing stops at the limit");
  await assert.rejects(storageOrigins(root, { limit: 10, strict: true }), /origins/);
  const kept = await storageOrigins(root, { limit: 10, strict: true, accept: origin => origin.endsWith(".keep.example") });
  assert.deepEqual([...kept].sort(), ["https://a0.keep.example", "https://a1.keep.example", "https://a2.keep.example"], "only origins under a kept domain count");
});

test("review round 2, N1: a site list measures at most 500 origins, the ones holding storage first", async () => {
  const root = profile();
  const ls = join(root, "Default", "Local Storage", "leveldb");
  mkdirSync(ls, { recursive: true });
  const ops = [];
  for (let i = 0; i < 700; i++) ops.push(["META:https://s" + i + ".many.example", "m"]);
  writeFileSync(join(ls, "000003.log"), logFile([{ sequence: 1, ops }]));
  const methods = [];
  const browser = data(root, [], {}, methods);
  const list = await browser.request({ mode: "sites", sort: "name" });
  assert.ok(list.sites.some(item => item.site === "many.example"));
  const measured = methods.filter(method => method === "Storage.getUsageAndQuota").length;
  assert.ok(measured > 0 && measured <= 500, "usage calls: " + measured);
});

test("review round 2, N3: lengths in snappy and table headers are checked against the input before allocating", async () => {
  const realAlloc = Buffer.alloc, asked = [];
  Buffer.alloc = (size, ...rest) => { asked.push(size); if (size > 64 * 1024 * 1024) throw new RangeError("test refused " + size); return realAlloc(size, ...rest); };
  try {
    assert.throws(() => snappyDecompress(Buffer.from([0xff, 0xff, 0xff, 0xff, 0x07, 0x00, 0x41])), /snappy/);
    const dir = join(profile(), "leveldb");
    mkdirSync(dir);
    const footer = realAlloc(48);
    footer[0] = 0; footer[1] = 0; footer[2] = 0; footer[3] = 7;
    footer.writeUInt32LE(0x8b80fb57, 40); footer.writeUInt32LE(0xdb477524, 44);
    writeFileSync(join(dir, "000007.ldb"), Buffer.concat([Buffer.from([0xff, 0xff, 0xff, 0xff, 0x07, 0x00, 0x41, 0x01, 0, 0, 0, 0]), footer]));
    await assert.rejects(liveKeys(dir, ["META:"]), /snappy|leveldb/);
  } finally { Buffer.alloc = realAlloc; }
  assert.ok(Math.max(0, ...asked) <= 1 << 20, "largest allocation asked for: " + Math.max(...asked));
});
