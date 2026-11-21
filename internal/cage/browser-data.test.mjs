import test from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync, readdirSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { BrowserData, builtInOrigins, cookieKey, siteOf, within } from "./browser-data.mjs";
import { applyPendingReset, diskCacheBytes, requestReset, seedPreferences } from "./browser-data-profile.mjs";

const cookie = (domain, name, extra = {}) => ({ name, value: name + "-secret", domain, path: "/", size: 10, secure: true, ...extra });

function fixture({ cookies = [], privateCookies = [], history = {}, tabs = [], usage = {}, probe = true } = {}) {
  const calls = [];
  const cdp = async (method, params = {}, sessionId) => {
    calls.push({ method, params, sessionId });
    if (method === "Runtime.evaluate") {
      if (!probe) return { exceptionDetails: { exception: { description: "ReferenceError: cageBrowsingData is not defined" } } };
      const input = JSON.parse(params.expression.slice("cageBrowsingData(".length, -1));
      const value = {
        probe: { browsingData: true, history: true },
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
    downloads: { snapshot: () => [{ state: "saved", updatedAt: new Date(5000).toISOString() }, { state: "downloading", updatedAt: new Date(6000).toISOString() }], forget: since => forgotten.push(since) },
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

test("the disk cache bound follows free space between 32 and 256 MiB", () => {
  const MiB = 1 << 20;
  assert.equal(diskCacheBytes("/", () => ({ bavail: 100 * 1024, bsize: 1024 * 1024 })), 256 * MiB);
  assert.equal(diskCacheBytes("/", () => ({ bavail: 2000, bsize: 1024 * 1024 })), 100 * MiB);
  assert.equal(diskCacheBytes("/", () => ({ bavail: 10, bsize: 1024 * 1024 })), 32 * MiB);
  assert.equal(diskCacheBytes("/", () => { throw new Error("no statfs"); }), 256 * MiB);
});

test("a requested reset starts the next Chromium on a seeded fresh profile", async () => {
  const home = mkdtempSync(join(tmpdir(), "cage-browser-reset-"));
  assert.equal(applyPendingReset(home, "zh-CN"), false);
  mkdirSync(join(home, "profile", "Default"), { recursive: true });
  writeFileSync(join(home, "profile", "Default", "Cookies"), "old");
  requestReset(home);
  const removed = [];
  assert.equal(applyPendingReset(home, "zh-CN", path => removed.push(path)), true);
  assert.equal(existsSync(join(home, "profile", "Default", "Cookies")), false);
  assert.deepEqual(JSON.parse(readFileSync(join(home, "profile", "Default", "Preferences"), "utf8")), seedPreferences("zh-CN"));
  assert.equal(removed.length, 1);
  assert.ok(existsSync(join(removed[0], "Default", "Cookies")), "the old profile moved aside for background removal");
  assert.ok(!readdirSync(home).includes("browser-reset.json"));
  assert.equal(applyPendingReset(home, "zh-CN"), false, "the marker is consumed once");
});
