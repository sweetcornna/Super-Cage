import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync, readdirSync, renameSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { BrowserData, builtInOrigins, cookieKey, siteOf, within } from "./browser-data.mjs";
import { applyPendingReset, cancelReset, chromiumCacheBytes, diskCacheBytes, requestReset, resetStatus, seedPreferences } from "./browser-data-profile.mjs";

const cookie = (domain, name, extra = {}) => ({ name, value: name + "-secret", domain, path: "/", size: 10, secure: true, ...extra });

function fixture({ cookies = [], privateCookies = [], history = {}, tabs = [], usage = {}, probe = true, onEvaluate, downloads } = {}) {
  const calls = [];
  const cdp = async (method, params = {}, sessionId) => {
    calls.push({ method, params, sessionId });
    if (method === "Runtime.evaluate") {
      if (!probe) return { exceptionDetails: { exception: { description: "ReferenceError: cageBrowsingData is not defined" } } };
      const input = JSON.parse(params.expression.slice("cageBrowsingData(".length, -1));
      if (onEvaluate) { const answer = await onEvaluate(input, sessionId); if (answer !== undefined) return answer; }
      const value = {
        probe: probe === true ? { browsingData: true, history: true } : probe,
        remove: { ok: true },
        "history-count": { count: 3, more: false },
        "history-origins": { origins: history, complete: true },
        "history-page": { items: input.op === "history-page" ? [
          { url: "http://localhost:47800/?nonce=abcdefghijklmnopqrst", title: "Science", time: 30, visits: 1 },
          { url: "http://127.0.0.1:18080/?folder=%2Fw", title: "Editor", time: 20, visits: 2 },
          { url: "https://example.com/a", title: "A", time: 10, visits: 1 },
        ].slice(0, input.limit) : [], more: false },
        "history-delete-urls": { ok: true, removed: input.urls?.length },
        "history-delete-host": { ok: true, removed: 2 },
        "history-delete-range": { ok: true },
      }[input.op];
      return { result: { value } };
    }
    if (method === "Storage.getCookies") return { cookies: params.browserContextId ? privateCookies : cookies };
    if (method === "Storage.getUsageAndQuota") return { usage: usage[params.origin] || 0 };
    return {};
  };
  const forgotten = [];
  const data = new BrowserData({
    cdp, control: async () => "control", session: async id => "session-" + id, tabs: () => tabs,
    active: () => tabs[0]?.targetId || "", privateContext: () => "private-context",
    downloads: downloads || { snapshot: () => [{ state: "saved", updatedAt: new Date(5000).toISOString() }, { state: "downloading", updatedAt: new Date(6000).toISOString() }], forget: since => { forgotten.push(since); return { removed: 1, kept: 0 }; } },
    profile: mkdtempSync(join(tmpdir(), "cage-browser-data-")), protectedOrigins: () => builtInOrigins(47800), sciencePort: () => 47800,
  });
  const evaluations = () => calls.filter(call => call.method === "Runtime.evaluate").map(call => JSON.parse(call.params.expression.slice("cageBrowsingData(".length, -1)));
  return { data, calls, evaluations, forgotten };
}

async function finish(data, started) {
  for (let i = 0; i < 200; i++) {
    const { job } = await data.request({ mode: "job", id: started.job.id });
    if (job.state !== "running") return job;
    await new Promise(resolve => setTimeout(resolve, 5));
  }
  throw new Error("job did not finish");
}

test("sites group by registrable domain without a suffix list", () => {
  assert.equal(siteOf("mail.google.com"), "google.com");
  assert.equal(siteOf(".github.com"), "github.com");
  assert.equal(siteOf("news.bbc.co.uk"), "bbc.co.uk");
  assert.equal(siteOf("a.b.example.com.cn"), "example.com.cn");
  assert.equal(siteOf("someone.github.io"), "someone.github.io");
  assert.equal(siteOf("127.0.0.1"), "127.0.0.1");
  assert.equal(siteOf("localhost"), "localhost");
  assert.equal(siteOf("sub.c.localhost"), "c.localhost");
  assert.ok(within("gist.github.com", "github.com"));
  assert.ok(!within("notgithub.com", "github.com"));
});

test("a clear keeps the built-in origins and kept domains with their subdomains", async () => {
  const tabs = [
    { targetId: "editor", url: "http://127.0.0.1:18080/?folder=/w" },
    { targetId: "kept", url: "https://gist.github.com/x" },
    { targetId: "web", url: "https://example.com/" },
    { targetId: "private", url: "https://example.com/", incognito: true, browserContextId: "private-context" },
    { targetId: "login", url: "https://claude.ai", login: true },
  ];
  const { data, evaluations, forgotten, calls } = fixture({ tabs,
    cookies: [cookie(".github.com", "user"), cookie("example.com", "sid")],
    history: { "https://docs.github.com": { last: 4, visits: 1 }, "https://example.com": { last: 3, visits: 2 } } });
  const job = await finish(data, await data.request({ mode: "clear", since: 1000, types: ["site", "cache", "history", "downloads"], keep: ["github.com"], keepOrigins: ["https://api.github.com", "javascript:alert(1)"] }));
  assert.equal(job.state, "done");
  const removes = evaluations().filter(input => input.op === "remove");
  const site = removes.find(input => input.types.cookies);
  assert.equal(site.since, 1000);
  assert.ok(site.types.cache && site.types.indexedDB && site.types.localStorage && site.types.serviceWorkers && site.types.cacheStorage);
  for (const origin of ["http://127.0.0.1:18080", "http://localhost:47800", "http://127.0.0.1:47800", "https://gist.github.com", "https://docs.github.com", "https://github.com", "https://api.github.com"])
    assert.ok(site.excludeOrigins.includes(origin), origin);
  assert.ok(!site.excludeOrigins.includes("https://example.com"));
  assert.ok(!site.excludeOrigins.includes("javascript:alert(1)"));
  const history = removes.find(input => input.types.history);
  assert.equal(history.excludeOrigins, undefined, "history cannot take an origin filter");
  assert.deepEqual(forgotten, [1000]);
  const reloaded = calls.filter(call => call.method === "Page.reload").map(call => call.sessionId);
  assert.deepEqual(reloaded, ["session-web"], "only regular pages that lost data reload; private and sign-in pages are untouched");
  assert.equal(job.result.reloaded, 1);
  assert.deepEqual(job.result.cleared, ["history", "site", "cache", "downloads"]);
  assert.ok(!calls.some(call => call.params?.browserContextId), "a regular clear never addresses another context");
});

test("unticking 保留 clears the built-in origins too", async () => {
  const { data, evaluations } = fixture();
  await finish(data, await data.request({ mode: "clear", since: 0, types: ["site"], keep: ["github.com"], keepEnabled: false }));
  const site = evaluations().find(input => input.op === "remove");
  assert.equal(site.excludeOrigins, undefined);
});

test("the private session is cleared through CDP only", async () => {
  const tabs = [{ targetId: "p1", url: "https://example.com/", incognito: true, browserContextId: "private-context" }, { targetId: "n1", url: "https://example.com/" }];
  const { data, calls, evaluations } = fixture({ tabs, privateCookies: [cookie(".example.com", "sid"), cookie("other.test", "x")] });
  const job = await finish(data, await data.request({ mode: "clear", session: "private", types: ["site", "cache", "history"] }));
  assert.equal(job.state, "done");
  assert.equal(evaluations().length, 0, "the extension sees the regular profile only");
  const cleared = calls.filter(call => call.method === "Storage.clearDataForOrigin");
  assert.ok(cleared.every(call => call.sessionId === "session-p1"));
  assert.ok(cleared.some(call => call.params.origin === "https://example.com"));
  assert.ok(calls.some(call => call.method === "Storage.clearCookies" && call.params.browserContextId === "private-context"));
  assert.ok(calls.some(call => call.method === "Network.clearBrowserCache" && call.sessionId === "session-p1"));
  assert.deepEqual(calls.filter(call => call.method === "Page.reload").map(call => call.sessionId), ["session-p1"]);
  assert.deepEqual(job.result.cleared, ["site", "cache"]);
});

test("site list: grouped, sized, sorted, searched and paged", async () => {
  const history = {};
  for (let i = 0; i < 120; i++) history[`https://s${i}.example${i}.com`] = { last: i + 1, visits: 1 };
  history["http://127.0.0.1:18080"] = { last: 999, visits: 4 };
  const usage = { "https://s5.example5.com": 5000, "https://s7.example7.com": 9000 };
  const { data } = fixture({ history, usage, cookies: [cookie(".example7.com", "a"), cookie("s7.example7.com", "b")] });
  const first = await data.request({ mode: "sites", sort: "size" });
  assert.equal(first.total, 121);
  assert.equal(first.sites.length, 50);
  assert.equal(first.sites[0].site, "example7.com");
  assert.equal(first.sites[0].cookies, 2);
  assert.equal(first.sites[0].bytes, 9020);
  const recent = await data.request({ mode: "sites", sort: "recent" });
  assert.equal(recent.sites[0].site, "127.0.0.1");
  assert.equal(recent.sites[0].protected, true);
  const last = await data.request({ mode: "sites", sort: "name", page: 9 });
  assert.equal(last.page, 2);
  assert.equal(last.sites.length, 21);
  const search = await data.request({ mode: "sites", query: "example11" });
  assert.deepEqual(search.sites.map(site => site.site), ["example11.com", "example110.com", "example111.com", "example112.com", "example113.com", "example114.com", "example115.com", "example116.com", "example117.com", "example118.com", "example119.com"]);
});

test("site detail lists cookies without values; values and deletes by key", async () => {
  const partitioned = cookie("cdn.example.com", "p", { partitionKey: { topLevelSite: "https://news.test", hasCrossSiteAncestor: true } });
  const opaque = cookie("example.com", "o", { partitionKeyOpaque: true });
  const { data, calls } = fixture({ cookies: [cookie(".example.com", "sid"), partitioned, opaque, cookie("other.test", "x")] });
  const detail = await data.request({ mode: "site", site: "example.com" });
  assert.equal(detail.total, 3);
  assert.ok(detail.cookies.every(item => !("value" in item)));
  assert.equal((await data.request({ mode: "cookie", site: "example.com", key: cookieKey(partitioned), action: "value" })).value, "p-secret");
  await data.request({ mode: "cookie", site: "example.com", key: cookieKey(partitioned), action: "delete" });
  const deletion = calls.find(call => call.method === "Network.deleteCookies");
  assert.deepEqual(deletion.params.partitionKey, partitioned.partitionKey);
  assert.equal(deletion.sessionId, "control");
  await assert.rejects(data.request({ mode: "cookie", site: "example.com", key: cookieKey(opaque), action: "delete" }), /分区/);
  await assert.rejects(data.request({ mode: "cookie", site: "example.com", key: cookieKey(cookie("other.test", "x")), action: "value" }), /已变化/);
});

test("history strips sign-in secrets and names the protected pages", async () => {
  const { data } = fixture();
  const page = await data.request({ mode: "history", limit: 2 });
  assert.equal(page.items.length, 2);
  assert.equal(page.items[0].url, "http://localhost:47800/");
  assert.equal(page.items[0].label, "Claude Science");
  assert.equal(page.items[1].label, "文件编辑器");
  assert.equal(page.next, 20);
});

test("summary counts finished downloads in range and sizes from the profile", async () => {
  const { data } = fixture();
  mkdirSync(join(data.profile, "Default", "Cache", "Cache_Data"), { recursive: true });
  writeFileSync(join(data.profile, "Default", "Cache", "Cache_Data", "entry"), Buffer.alloc(4096));
  mkdirSync(join(data.profile, "Default", "IndexedDB", "https_example.com_0.indexeddb.leveldb"), { recursive: true });
  writeFileSync(join(data.profile, "Default", "IndexedDB", "https_example.com_0.indexeddb.leveldb", "000003.log"), Buffer.alloc(1000));
  const summary = await data.request({ mode: "summary", since: 0 });
  assert.equal(summary.cache.bytes, 4096);
  assert.equal(summary.sites.bytes, 1000);
  assert.equal(summary.downloads.count, 1);
  assert.equal(summary.history.count, 3);
  assert.ok(summary.protected.some(item => item.label === "文件编辑器"));
});

test("an old extension page reports the capability as missing", async () => {
  const { data } = fixture({ probe: false });
  assert.deepEqual(await data.request({ mode: "capabilities" }), { available: false, reason: "update" });
  assert.equal((await fixture().data.request({ mode: "capabilities" })).available, true);
});

test("one clear at a time; unknown modes are refused", async () => {
  const { data } = fixture();
  const started = await data.request({ mode: "clear", types: ["cache"] });
  await assert.rejects(async () => data.request({ mode: "clear", types: ["cache"] }), /正在清除/);
  await finish(data, started);
  await assert.rejects(data.request({ mode: "reset-everything" }), /无效/);
  await assert.rejects(async () => data.request({ mode: "clear", types: [] }), /请选择/);
});

test("the disk cache bound never exceeds Chromium's own size and stays under 256 MiB", () => {
  const MiB = 1 << 20, free = mib => () => ({ bavail: mib, bsize: MiB });
  for (const mib of [10, 90, 500, 1500, 2000, 4096, 10 * 1024, 30 * 1024, 100 * 1024]) {
    const bytes = diskCacheBytes("/", free(mib));
    assert.ok(bytes <= chromiumCacheBytes(mib * MiB) && bytes <= 256 * MiB && bytes >= MiB, `${mib} MiB free: ${bytes}`);
  }
  assert.equal(chromiumCacheBytes(30 * 1024 * MiB), 307 * MiB + Math.floor(0.2 * MiB));
  assert.equal(diskCacheBytes("/", free(100 * 1024)), 256 * MiB);  // Chromium: 1 GiB
  assert.equal(diskCacheBytes("/", free(10 * 1024)), 200 * MiB);   // Chromium: 200 MiB, unchanged
  assert.equal(diskCacheBytes("/", free(2000)), 100 * MiB);        // Chromium: 200 MiB
  assert.equal(diskCacheBytes("/", free(10)), MiB);                // Chromium: 8 MiB
  assert.equal(diskCacheBytes("/", () => { throw new Error("no statfs"); }), 256 * MiB);
});

test("a requested reset starts the next Chromium on a seeded fresh profile", async () => {
  const home = mkdtempSync(join(tmpdir(), "cage-browser-reset-"));
  assert.equal(applyPendingReset(home, "zh-CN"), false);
  mkdirSync(join(home, "profile", "Default"), { recursive: true });
  writeFileSync(join(home, "profile", "Default", "Cookies"), "old");
  requestReset(home);
  const removed = [];
  assert.equal(applyPendingReset(home, "zh-CN", { removeLater: path => removed.push(path) }), true);
  assert.equal(existsSync(join(home, "profile", "Default", "Cookies")), false);
  assert.deepEqual(JSON.parse(readFileSync(join(home, "profile", "Default", "Preferences"), "utf8")), seedPreferences("zh-CN"));
  assert.equal(removed.length, 1);
  assert.ok(existsSync(join(removed[0], "Default", "Cookies")), "the old profile moved aside for background removal");
  assert.ok(!readdirSync(home).includes("browser-reset.json"));
  assert.equal(applyPendingReset(home, "zh-CN"), false, "the marker is consumed once");
});

// Review 3.
const validOrigin = origin => { try { return new URL(origin).origin === origin; } catch { return false; } };

test("finding 7: a page without chrome.browsingData or chrome.history asks for an update, never a retry", async () => {
  for (const probe of [{ browsingData: false, history: true }, { browsingData: true, history: false }])
    assert.deepEqual(await fixture({ probe }).data.request({ mode: "capabilities" }), { available: false, reason: "update" });
});

test("finding 2: IP and numeric hosts never get a www. origin; every origin handed to Chromium parses", async () => {
  const { data, evaluations } = fixture({ cookies: [cookie("192.168.1.10", "sid")] });
  const kept = await finish(data, await data.request({ mode: "clear", since: 0, types: ["site", "cache"], keep: ["192.168.1.10", "127.0.0.1", "10.0.0.1", "1.2.3", "foo.123", "github.com"] }));
  assert.equal(kept.state, "done");
  const remove = evaluations().find(input => input.op === "remove");
  assert.deepEqual(remove.excludeOrigins.filter(origin => !validOrigin(origin)), []);
  assert.ok(remove.excludeOrigins.includes("https://www.github.com") && remove.excludeOrigins.includes("http://192.168.1.10") && remove.excludeOrigins.includes("http://10.0.0.1"));
  assert.ok(kept.result.skippedOrigins > 0, "invalid kept hosts are counted, not sent");
  const sites = await finish(data, await data.request({ mode: "clear-sites", sites: ["127.0.0.1", "192.168.1.10", "foo.123"] }));
  assert.equal(sites.state, "done");
  const siteRemove = evaluations().filter(input => input.op === "remove").at(-1);
  assert.deepEqual(siteRemove.origins.filter(origin => !validOrigin(origin)), []);
  assert.ok(siteRemove.origins.includes("http://127.0.0.1") && siteRemove.origins.includes("https://192.168.1.10"));
  await finish(data, await data.request({ mode: "clear-sites", sites: ["::1"] }));
  assert.ok(evaluations().filter(input => input.op === "remove").at(-1).origins.includes("http://[::1]"), "an IPv6 site is cleared by its bracketed origin");
  const before = evaluations().length;
  const onlyInvalid = await finish(data, await data.request({ mode: "clear-sites", sites: ["foo.123"] }));
  assert.equal(onlyInvalid.state, "done");
  assert.equal(evaluations().slice(before).filter(input => input.op === "remove").length, 0, "an empty origin list never reaches browsingData.remove, which would clear every site");
  assert.ok(onlyInvalid.result.skippedOrigins > 0);
});

test("finding 1: clearing download records keeps unsaved downloads and says so", async () => {
  const at = new Date(5000).toISOString();
  const snapshot = () => [{ state: "saved", updatedAt: at }, { state: "failed", retryable: true, updatedAt: at }, { state: "failed", retryable: false, updatedAt: at }, { state: "canceled", updatedAt: at }];
  const { data } = fixture({ downloads: { snapshot, forget: () => ({ removed: 3, kept: 1 }) } });
  const summary = await data.request({ mode: "summary", since: 0 });
  assert.deepEqual(summary.downloads, { count: 3, unsaved: 1 });
  const job = await finish(data, await data.request({ mode: "clear", since: 0, types: ["downloads"] }));
  assert.equal(job.result.keptDownloads, 1);
});

test("finding 8: a clear asked not to reload leaves the pages alone", async () => {
  const tabs = [{ targetId: "web", url: "https://example.com/" }];
  const { data, calls } = fixture({ tabs });
  const job = await finish(data, await data.request({ mode: "clear", since: 0, types: ["site"], reload: false }));
  assert.equal(job.state, "done");
  assert.equal(calls.filter(call => call.method === "Page.reload").length, 0);
  assert.equal(job.result.reloaded, 0);
});

test("finding 12: a destructive call that timed out is never sent twice; reads retry once", async () => {
  let removes = 0, pages = 0;
  const { data } = fixture({ onEvaluate: input => {
    if (input.op === "remove" && ++removes === 1) throw new Error("Browser command timed out: Runtime.evaluate");
    if (input.op === "history-page" && ++pages === 1) throw new Error("Browser command timed out: Runtime.evaluate");
  } });
  const job = await finish(data, await data.request({ mode: "clear", since: 0, types: ["history"] }));
  assert.equal(job.state, "failed");
  assert.equal(removes, 1);
  const page = await data.request({ mode: "history" });
  assert.equal(pages, 2);
  assert.ok(page.items.length > 0);
  let deletes = 0;
  const second = fixture({ onEvaluate: input => { if (input.op === "history-delete-host" && ++deletes === 1) throw new Error("Browser command timed out: Runtime.evaluate"); } });
  await assert.rejects(second.data.request({ mode: "history-delete", host: "example.com" }));
  assert.equal(deletes, 1);
});

test("finding 12: history entries are bounded and a long address is fetched whole only to open it", async () => {
  const long = "https://example.com/?" + "q".repeat(5000);
  const { data } = fixture({ onEvaluate: input => {
    if (input.op === "history-page") return { result: { value: { items: [{ url: long.slice(0, 2048), truncated: true, id: "7", title: "t", time: 5, visits: 1 }], more: false } } };
    if (input.op === "history-url") return { result: { value: { url: input.id === "7" ? long : "" } } };
  } });
  const page = await data.request({ mode: "history" });
  assert.equal(page.items[0].truncated, true);
  assert.ok(page.items[0].url.length <= 2048);
  assert.equal((await data.request({ mode: "history-url", id: "7", time: 5 })).url, long);
  await assert.rejects(data.request({ mode: "history-url", id: "8", time: 5 }), /记录/);
});

test("finding 12: a private cache-only clear reports nothing freed it cannot measure", async () => {
  const tabs = [{ targetId: "p1", url: "https://example.com/", incognito: true, browserContextId: "private-context" }];
  const { data } = fixture({ tabs, usage: { "https://example.com": 5000 } });
  const job = await finish(data, await data.request({ mode: "clear", session: "private", types: ["cache"] }));
  assert.equal(job.result.freedBytes, 0);
});

test("finding 10: a reset marker carries its id and expires; the outcome is written for the host", () => {
  const home = mkdtempSync(join(tmpdir(), "cage-browser-reset-"));
  mkdirSync(join(home, "profile", "Default"), { recursive: true });
  writeFileSync(join(home, "profile", "Default", "Cookies"), "old");
  // A marker older than ten minutes is dropped, the profile untouched.
  requestReset(home, "r-old", Date.now() - 11 * 60_000);
  assert.equal(applyPendingReset(home, "zh-CN", { removeLater: () => {} }), false);
  assert.ok(existsSync(join(home, "profile", "Default", "Cookies")));
  assert.ok(!existsSync(join(home, "browser-reset.json")));
  assert.deepEqual(resetStatus(home, "r-old"), { state: "expired" });
  // The host gave up: the marker goes before the next start.
  requestReset(home, "r-cancel");
  cancelReset(home, "r-cancel");
  assert.equal(applyPendingReset(home, "zh-CN", { removeLater: () => {} }), false);
  // A rename that fails keeps the old profile and says so.
  requestReset(home, "r-rename");
  assert.equal(applyPendingReset(home, "zh-CN", { removeLater: () => {}, rename: () => { throw Object.assign(new Error("busy"), { code: "EBUSY" }); } }), false);
  assert.ok(existsSync(join(home, "profile", "Default", "Cookies")));
  assert.equal(resetStatus(home, "r-rename").state, "failed");
  // Seeding that fails after the rename moves the old profile back.
  requestReset(home, "r-seed");
  assert.equal(applyPendingReset(home, "zh-CN", { removeLater: () => {}, seed: () => { throw Object.assign(new Error("full"), { code: "ENOSPC" }); } }), false);
  assert.ok(existsSync(join(home, "profile", "Default", "Cookies")), "the old profile is back in place");
  assert.equal(resetStatus(home, "r-seed").state, "failed");
  assert.deepEqual(readdirSync(home).filter(name => name.startsWith("profile.reset-")), []);
  // Success, and the id of another request is unknown.
  requestReset(home, "r-ok");
  const removed = [];
  assert.equal(applyPendingReset(home, "zh-CN", { removeLater: path => removed.push(path) }), true);
  assert.deepEqual(resetStatus(home, "r-ok"), { state: "done" });
  assert.deepEqual(resetStatus(home, "r-other"), { state: "unknown" });
});

test("finding 10: a leftover old profile is removed at the next start", () => {
  const home = mkdtempSync(join(tmpdir(), "cage-browser-reset-"));
  mkdirSync(join(home, "profile.reset-1700000000000", "Default"), { recursive: true });
  mkdirSync(join(home, "profile", "Default"), { recursive: true });
  const removed = [];
  assert.equal(applyPendingReset(home, "zh-CN", { removeLater: path => removed.push(path) }), false);
  assert.deepEqual(removed, [join(home, "profile.reset-1700000000000")]);
});

test("review round 2, N6: a reset whose seeding fails and whose old profile cannot be put back is never done, and the old profile stays", () => {
  const home = mkdtempSync(join(tmpdir(), "cage-browser-reset-"));
  mkdirSync(join(home, "profile", "Default"), { recursive: true });
  writeFileSync(join(home, "profile", "Default", "Cookies"), "old");
  requestReset(home, "r-stuck");
  let renames = 0;
  const removed = [];
  const rename = (from, to) => { if (renames++ === 0) return renameSync(from, to); throw Object.assign(new Error("busy"), { code: "EBUSY" }); };
  assert.equal(applyPendingReset(home, "zh-CN", { removeLater: path => removed.push(path), rename, seed: () => { throw Object.assign(new Error("full"), { code: "ENOSPC" }); } }), false);
  assert.equal(resetStatus(home, "r-stuck").state, "incomplete");
  const aside = readdirSync(home).filter(name => name.startsWith("profile.reset-"));
  assert.equal(aside.length, 1);
  assert.ok(existsSync(join(home, aside[0], "Default", "Cookies")));
  assert.deepEqual(removed, [], "the old profile is not deleted");
  // The next start's sweep of leftover old profiles leaves this one too.
  assert.equal(applyPendingReset(home, "zh-CN", { removeLater: path => removed.push(path) }), false);
  assert.deepEqual(removed, []);
});
