// Browsing data for the workbench's own dialog, site list, history and reset
// (POST /data). Loaded on the first request, so a browser nobody manages pays
// nothing for it. The regular profile is reached through the built-in
// extension (split-extension/data.js: chrome.browsingData, chrome.history);
// the private session, a CDP browser context, only through CDP. Sign-in,
// audit and clipboard contexts are never addressed. Nothing here logs a URL,
// a cookie or a history entry.
import { createHash, randomUUID } from "node:crypto";
import { lstat, readdir } from "node:fs/promises";
import { join } from "node:path";
import { publicScienceURL, readScienceConfig } from "./browser-science.mjs";

// Loopback origins the workbench itself relies on: the file editor
// (code-server, browser-agent.mjs editor()), Claude Science on its configured
// port, this controller's own page and the web VNC client. Kept by default.
export function builtInOrigins(sciencePort = readScienceConfig()?.port) {
  return [
    { origin: "http://127.0.0.1:18080", label: "文件编辑器" },
    ...(Number.isInteger(sciencePort) ? [
      { origin: `http://localhost:${sciencePort}`, label: "Claude Science" },
      { origin: `http://127.0.0.1:${sciencePort}`, label: "Claude Science" },
    ] : []),
    { origin: "http://127.0.0.1:18500", label: "Super Cage 浏览器组件" },
    { origin: "http://127.0.0.1:16080", label: "Super Cage 远程画面" },
  ];
}

// The site-data types chrome.browsingData accepts in current Chromium
// (browsingData.settings() lists no webSQL or appcache any more).
export const siteTypes = { cookies: true, localStorage: true, indexedDB: true, cacheStorage: true, serviceWorkers: true, fileSystems: true };
const cacheDirectories = ["Cache", "Code Cache", "GPUCache", "DawnGraphiteCache", "DawnWebGPUCache"];
const storageDirectories = ["IndexedDB", "Local Storage", "Session Storage", "Service Worker", "WebStorage", "File System", "Shared Storage"];
const categories = ["history", "site", "cache", "downloads"];
const pageSize = 50;
const memoLifetime = 20_000;

// Cookie keys match browser-cookies.mjs, so a key names the same cookie in
// both tools.
export const cookieKey = cookie => createHash("sha256").update(JSON.stringify([
  cookie.name, cookie.domain, cookie.path, cookie.partitionKey || null, Boolean(cookie.partitionKeyOpaque),
])).digest("hex");

// Registrable domain without a public suffix list in the guest: IPs and
// single labels stand alone; a two-letter country code under a generic
// second level, and the common hosting suffixes, keep three labels. Only the
// grouping of the list depends on this; Chromium applies its own list when
// it removes cookies.
const hostingSuffixes = new Set(["github.io", "gitlab.io", "vercel.app", "netlify.app", "pages.dev", "workers.dev", "herokuapp.com",
  "appspot.com", "web.app", "firebaseapp.com", "blogspot.com", "azurewebsites.net", "cloudfront.net", "onrender.com", "fly.dev", "glitch.me"]);
const secondLevels = new Set(["com", "net", "org", "gov", "edu", "ac", "co", "ne", "or", "go", "mil", "gen", "biz", "info", "nom", "ltd", "plc", "sch"]);
export function siteOf(host) {
  host = String(host || "").toLowerCase().replace(/^\./, "").replace(/\.$/, "");
  if (!host || /^\d+(\.\d+){3}$/.test(host) || host.includes(":") || host.startsWith("[")) return host;
  const labels = host.split(".");
  if (labels.length <= 2) return host;
  const last2 = labels.slice(-2).join(".");
  if (hostingSuffixes.has(last2) || (labels.at(-1).length === 2 && secondLevels.has(labels.at(-2)))) return labels.slice(-3).join(".");
  return last2;
}
export const within = (host, domain) => host === domain || host.endsWith("." + domain);
const cookieHost = cookie => cookie.domain.replace(/^\./, "");
const originHost = origin => { try { return new URL(origin).hostname.replace(/^\[|\]$/g, ""); } catch { return ""; } };
const webOrigin = value => { try { const url = new URL(value); return ["http:", "https:"].includes(url.protocol) ? url.origin : ""; } catch { return ""; } };

// A bounded, cancellable size of a profile directory. Symbolic links are not
// followed; a directory that disappears while it is walked counts as empty.
export async function directoryBytes(path, budget = { entries: 200_000 }) {
  let total = 0;
  const walk = async directory => {
    let entries;
    try { entries = await readdir(directory, { withFileTypes: true }); } catch { return; }
    for (const entry of entries) {
      if (--budget.entries < 0) return;
      const child = join(directory, entry.name);
      if (entry.isDirectory()) await walk(child);
      else if (entry.isFile()) { try { total += (await lstat(child)).size; } catch {} }
    }
  };
  await walk(path);
  return total;
}

async function pool(items, limit, work) {
  const results = new Array(items.length);
  let next = 0;
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, async () => {
    while (next < items.length) { const index = next++; results[index] = await work(items[index], index); }
  }));
  return results;
}

export class BrowserData {
  constructor({ cdp, control, session, tabs, active = () => "", queue = work => work(), privateContext, downloads, profile,
    protectedOrigins = () => builtInOrigins(), sciencePort = () => readScienceConfig()?.port, now = Date.now }) {
    Object.assign(this, { cdp, control, session, tabs, active, queue, privateContext, downloads, profile, protectedOrigins, sciencePort, now });
    this.job = null;
    this.memo = null;
  }

  async extension(op, input = {}, timeout = 15_000) {
    const call = async repair => this.cdp("Runtime.evaluate", {
      expression: `cageBrowsingData(${JSON.stringify({ op, ...input })})`, awaitPromise: true, returnByValue: true,
    }, await this.control(repair), timeout);
    let response;
    try { response = await call(false); } catch { response = await call(true); }
    const text = response.exceptionDetails?.exception?.description || response.exceptionDetails?.text || "";
    if (/not defined/.test(text)) throw Object.assign(new Error("浏览器组件不支持管理浏览数据，请更新环境组件"), { unsupported: true });
    if (response.exceptionDetails) throw new Error("浏览数据操作未完成，请重试");
    return response.result.value;
  }

  async capabilities() {
    try {
      const probe = await this.extension("probe", {}, 5000);
      return { available: probe.browsingData && probe.history, version: 1, ranges: true, history: probe.history, incognito: true, reset: true };
    } catch (error) {
      return { available: false, reason: error.unsupported ? "update" : "unavailable" };
    }
  }

  // The private session is the one context of user-created private tabs.
  privateTab() {
    const context = this.privateContext();
    return context ? this.tabs().find(tab => tab.browserContextId === context && !tab.login) : null;
  }

  async scope(session) {
    if (session === "private") {
      const tab = this.privateTab();
      if (!tab) return null;
      return { private: true, context: tab.browserContextId, sessionId: await this.session(tab.targetId) };
    }
    return { private: false, sessionId: await this.control(false) };
  }

  async cookies(scope) {
    const { cookies } = await this.cdp("Storage.getCookies", scope.private ? { browserContextId: scope.context } : {});
    return cookies;
  }

  // Every CDP target carries a browserContextId, the default context's too:
  // the regular session is what is neither private nor a sign-in page.
  tabsOf(scope) {
    return this.tabs().filter(tab => !tab.login && (scope.private ? tab.browserContextId === scope.context : !tab.incognito));
  }

  // Pages that lost their session show it: the active page first, then the
  // others one at a time, in the controller's tab queue.
  async reload(ids) {
    const active = this.active();
    const order = [...ids].sort((a, b) => (b === active) - (a === active));
    let reloaded = 0;
    for (const id of order) {
      try { await this.queue(async () => this.cdp("Page.reload", {}, await this.session(id))); reloaded++; } catch {}
    }
    return reloaded;
  }

  // Every origin this session is known to have touched: open tabs, cookie
  // hosts, recent history and the profile's IndexedDB directories.
  async origins(scope) {
    const origins = new Map();
    const add = (origin, last = 0, visits = 0) => {
      if (!origin) return;
      const entry = origins.get(origin) || { last: 0, visits: 0 };
      entry.last = Math.max(entry.last, last); entry.visits += visits;
      origins.set(origin, entry);
    };
    for (const tab of this.tabsOf(scope)) add(webOrigin(tab.url));
    const cookies = await this.cookies(scope);
    // A cookie names a host, not a scheme or a port: both schemes are probed.
    for (const host of new Set(cookies.map(cookieHost))) { add("https://" + host); add("http://" + host); }
    let historyComplete = true;
    if (!scope.private) {
      const history = await this.extension("history-origins");
      historyComplete = history.complete;
      for (const [origin, entry] of Object.entries(history.origins)) add(origin, entry.last, entry.visits);
      try {
        for (const name of await readdir(join(this.profile, "Default", "IndexedDB"))) {
          const match = /^(https?)_(.+)_(\d+)\.indexeddb\.leveldb$/.exec(name);
          if (match) add(`${match[1]}://${match[2]}${(match[1] === "https" && match[3] === "443") || (match[1] === "http" && match[3] === "80") ? "" : ":" + match[3]}`);
        }
      } catch {}
    }
    return { origins, cookies, historyComplete };
  }

  async usage(scope, origins) {
    return pool(origins, 8, async origin => {
      try { return (await this.cdp("Storage.getUsageAndQuota", { origin }, scope.sessionId)).usage || 0; } catch { return 0; }
    });
  }

  async siteIndex(session) {
    if (this.memo?.session === session && this.now() - this.memo.at < memoLifetime) return this.memo;
    const scope = await this.scope(session);
    if (!scope) return { session, sites: [], at: this.now() };
    const { origins, cookies, historyComplete } = await this.origins(scope);
    const list = [...origins.keys()];
    const bytes = await this.usage(scope, list);
    const sites = new Map();
    const entry = site => {
      if (!sites.has(site)) sites.set(site, { site, cookies: 0, cookieBytes: 0, bytes: 0, last: 0, origins: [] });
      return sites.get(site);
    };
    list.forEach((origin, index) => {
      const item = entry(siteOf(originHost(origin)));
      item.bytes += bytes[index];
      item.last = Math.max(item.last, origins.get(origin).last);
      item.origins.push(origin);
    });
    for (const cookie of cookies) {
      const item = entry(siteOf(cookieHost(cookie)));
      item.cookies++; item.cookieBytes += cookie.size || 0;
    }
    const result = { session, at: this.now(), historyComplete, sites: [...sites.values()].filter(site => site.site && (site.cookies || site.bytes || site.last)) };
    this.memo = result;
    setTimeout(() => { if (this.memo === result) this.memo = null; }, memoLifetime).unref?.();
    return result;
  }

  async sites({ session = "normal", query = "", sort = "size", page = 0 }) {
    const index = await this.siteIndex(session);
    const search = query.trim().toLowerCase();
    const protectedSites = new Set(this.protectedOrigins().map(item => siteOf(originHost(item.origin))));
    let list = index.sites.filter(site => !search || site.site.includes(search) || site.origins.some(origin => origin.includes(search)));
    const order = { size: (a, b) => (b.bytes + b.cookieBytes) - (a.bytes + a.cookieBytes), recent: (a, b) => b.last - a.last, name: () => 0 };
    list = list.sort((a, b) => order[sort](a, b) || a.site.localeCompare(b.site));
    const current = Math.min(page, Math.max(0, Math.ceil(list.length / pageSize) - 1));
    return {
      total: list.length, page: current, pageSize, historyComplete: index.historyComplete,
      bytes: list.reduce((sum, site) => sum + site.bytes + site.cookieBytes, 0),
      sites: list.slice(current * pageSize, (current + 1) * pageSize).map(site => ({
        site: site.site, cookies: site.cookies, bytes: site.bytes + site.cookieBytes, last: site.last,
        origins: site.origins.length, protected: protectedSites.has(site.site),
      })),
    };
  }

  async site({ session = "normal", site, page = 0, query = "" }) {
    const scope = await this.scope(session);
    if (!scope) return { site, origins: [], cookies: [], total: 0, page: 0 };
    const index = await this.siteIndex(session);
    const origins = index.sites.find(item => item.site === site)?.origins || [];
    const bytes = await this.usage(scope, origins);
    const search = String(query).trim().toLowerCase();
    const cookies = (await this.cookies(scope)).filter(cookie => siteOf(cookieHost(cookie)) === site &&
      (!search || `${cookie.domain}\n${cookie.name}`.toLowerCase().includes(search)))
      .sort((a, b) => a.domain.localeCompare(b.domain) || a.name.localeCompare(b.name) || a.path.localeCompare(b.path));
    const current = Math.min(page, Math.max(0, Math.ceil(cookies.length / pageSize) - 1));
    return {
      site, total: cookies.length, page: current, pageSize, incognito: scope.private,
      // Origins guessed from a cookie's host show only when they hold data.
      origins: origins.map((origin, i) => ({ origin, bytes: bytes[i] })).filter(item => item.bytes > 0).sort((a, b) => b.bytes - a.bytes),
      cookies: cookies.slice(current * pageSize, (current + 1) * pageSize).map(cookie => ({
        key: cookieKey(cookie), name: cookie.name, domain: cookie.domain, path: cookie.path,
        expires: cookie.expires, session: cookie.session, secure: cookie.secure, httpOnly: cookie.httpOnly,
        sameSite: cookie.sameSite || "", partitionKey: cookie.partitionKey, partitionKeyOpaque: Boolean(cookie.partitionKeyOpaque), size: cookie.size,
      })),
    };
  }

  async cookie({ session = "normal", site, key, action }) {
    const scope = await this.scope(session);
    const cookie = scope && (await this.cookies(scope)).find(item => cookieKey(item) === key && siteOf(cookieHost(item)) === site);
    if (!cookie) throw new Error("Cookie 已变化，请刷新列表");
    if (action === "value") return { value: cookie.value };
    // Opaque partition keys cannot be addressed exactly by Network.deleteCookies.
    if (cookie.partitionKeyOpaque) throw new Error("此 Cookie 的分区不可单独定位，请清除此网站数据");
    await this.cdp("Network.deleteCookies", { name: cookie.name, domain: cookie.domain, path: cookie.path,
      ...(cookie.partitionKey ? { partitionKey: cookie.partitionKey } : {}) }, scope.sessionId);
    this.memo = null;
    return { ok: true };
  }

  async disk(names) {
    const sizes = await Promise.all(names.map(name => directoryBytes(join(this.profile, "Default", name))));
    return sizes.reduce((sum, size) => sum + size, 0);
  }

  downloadRecords(since) {
    return this.downloads.snapshot().filter(item => ["saved", "failed", "canceled"].includes(item.state) && Date.parse(item.updatedAt || 0) >= since);
  }

  async summary({ session = "normal", since = 0 }) {
    const scope = await this.scope(session);
    if (!scope) return { session, empty: true };
    if (scope.private) {
      const index = await this.siteIndex("private");
      return { session, sites: { count: index.sites.length, bytes: index.sites.reduce((sum, site) => sum + site.bytes + site.cookieBytes, 0) } };
    }
    const [history, index, storage, cache] = await Promise.all([
      this.extension("history-count", { since, limit: 10_000 }), this.siteIndex("normal"),
      this.disk(storageDirectories), this.disk(cacheDirectories),
    ]);
    return {
      session, since,
      history: { count: history.count, more: history.more },
      sites: { count: index.sites.length, bytes: storage },
      cache: { bytes: cache },
      downloads: { count: this.downloadRecords(since).length },
      protected: this.protectedOrigins(),
    };
  }

  // Origins kept by the exceptions: every known origin of a kept domain or its
  // subdomains (both schemes), the origins remembered for it by the host, and
  // the built-in protected origins. Cookies are kept by Chromium for the whole
  // registrable domain of each kept origin.
  keptOrigins(known, keep = [], remembered = [], builtIn = true) {
    const kept = new Set(builtIn ? this.protectedOrigins().map(item => item.origin) : []);
    for (const domain of keep) {
      for (const origin of known) if (within(originHost(origin), domain)) kept.add(origin);
      for (const prefix of ["https://", "http://", "https://www."]) kept.add(prefix + domain);
    }
    for (const origin of remembered) if (webOrigin(origin) === origin) kept.add(origin);
    return [...kept];
  }

  start(work, steps) {
    if (this.job?.state === "running") throw new Error("正在清除浏览数据，请等待完成");
    const job = { id: randomUUID(), state: "running", step: 0, steps, started: this.now() };
    this.job = job;
    work(step => { job.step = step; }).then(result => { job.state = "done"; job.result = result; },
      error => { job.state = "failed"; job.error = error.message || "浏览数据未能清除"; })
      .finally(() => { job.finished = this.now(); this.memo = null; });
    return { job: this.status(job.id) };
  }

  status(id) {
    if (!this.job || this.job.id !== id) throw new Error("清除任务已结束，请重新打开");
    const { started, finished, ...job } = this.job;
    return { ...job, elapsed: (finished || this.now()) - started };
  }

  clear({ session = "normal", since = 0, types = [], keep = [], keepOrigins = [], keepEnabled = true }) {
    const wanted = categories.filter(type => types.includes(type));
    if (!wanted.length) throw new Error("请选择要清除的内容");
    return this.start(async step => {
      const scope = await this.scope(session);
      if (!scope) return { cleared: [], freedBytes: 0, reloaded: 0, kept: [] };
      if (scope.private) return this.clearPrivate(scope, wanted, step);
      step(1);
      const measured = [...(wanted.includes("site") ? storageDirectories : []), ...(wanted.includes("cache") ? cacheDirectories : [])];
      const before = await this.disk(measured);
      let kept = [], keptHere = [];
      if (wanted.includes("site") || wanted.includes("cache")) {
        const { origins } = keepEnabled ? await this.origins(scope) : { origins: new Map() };
        kept = keepEnabled ? this.keptOrigins([...origins.keys()], keep, keepOrigins) : [];
        keptHere = kept.filter(origin => origins.has(origin));
        step(2);
        const remove = { ...(wanted.includes("site") ? siteTypes : {}), ...(wanted.includes("cache") ? { cache: true } : {}) };
        await this.extension("remove", { since, ...(kept.length ? { excludeOrigins: kept } : {}), types: remove }, 60_000);
      }
      step(3);
      if (wanted.includes("history")) await this.extension("remove", { since, types: { history: true } }, 60_000);
      if (wanted.includes("downloads")) {
        await this.extension("remove", { since, types: { downloads: true } }, 30_000);
        this.downloads.forget?.(since);
      }
      step(4);
      const after = await this.disk(measured);
      let reloaded = 0;
      if (wanted.includes("site")) {
        const keptSet = new Set(kept);
        const ids = this.tabsOf(scope).filter(tab => webOrigin(tab.url) && !keptSet.has(webOrigin(tab.url))).map(tab => tab.targetId);
        reloaded = await this.reload(ids);
      }
      return { cleared: wanted, freedBytes: Math.max(0, before - after), reloaded, kept: keptHere };
    }, 4);
  }

  async clearPrivate(scope, wanted, step) {
    step(1);
    const { origins } = await this.origins(scope);
    const before = (await this.usage(scope, [...origins.keys()])).reduce((sum, bytes) => sum + bytes, 0);
    step(2);
    if (wanted.includes("site")) {
      await pool([...origins.keys()], 4, origin => this.cdp("Storage.clearDataForOrigin", { origin, storageTypes: "all" }, scope.sessionId).catch(() => {}));
      await this.cdp("Storage.clearCookies", { browserContextId: scope.context });
    }
    step(3);
    if (wanted.includes("cache")) await this.cdp("Network.clearBrowserCache", {}, scope.sessionId).catch(() => {});
    step(4);
    const reloaded = wanted.includes("site") ? await this.reload(this.tabsOf(scope).filter(tab => webOrigin(tab.url)).map(tab => tab.targetId)) : 0;
    return { cleared: wanted.filter(type => type === "site" || type === "cache"), freedBytes: before, reloaded, kept: [] };
  }

  clearSites({ session = "normal", sites = [] }) {
    if (!sites.length) throw new Error("请选择网站");
    return this.start(async step => {
      const scope = await this.scope(session);
      if (!scope) return { cleared: [], freedBytes: 0, reloaded: 0 };
      step(1);
      const index = await this.siteIndex(session);
      const wanted = new Set(sites);
      const origins = new Set();
      for (const site of sites) for (const prefix of ["https://", "http://", "https://www."]) origins.add(prefix + site);
      for (const item of index.sites) if (wanted.has(item.site)) item.origins.forEach(origin => origins.add(origin));
      const list = [...origins];
      const before = (await this.usage(scope, list)).reduce((sum, bytes) => sum + bytes, 0);
      const cookies = (await this.cookies(scope)).filter(cookie => wanted.has(siteOf(cookieHost(cookie))));
      const cookieBytes = cookies.reduce((sum, cookie) => sum + (cookie.size || 0), 0);
      step(2);
      if (scope.private) {
        await pool(list, 4, origin => this.cdp("Storage.clearDataForOrigin", { origin, storageTypes: "all" }, scope.sessionId).catch(() => {}));
        for (const cookie of cookies) if (!cookie.partitionKeyOpaque)
          await this.cdp("Network.deleteCookies", { name: cookie.name, domain: cookie.domain, path: cookie.path,
            ...(cookie.partitionKey ? { partitionKey: cookie.partitionKey } : {}) }, scope.sessionId).catch(() => {});
      } else {
        await this.extension("remove", { origins: list, types: { ...siteTypes, cache: true } }, 60_000);
      }
      step(3);
      const ids = this.tabsOf(scope).filter(tab => wanted.has(siteOf(originHost(webOrigin(tab.url))))).map(tab => tab.targetId);
      const reloaded = await this.reload(ids);
      return { cleared: ["site"], sites: sites.length, freedBytes: before + cookieBytes, reloaded };
    }, 3);
  }

  async history({ query = "", before = 0, limit = 100 }) {
    const end = before || this.now() + 60_000;
    limit = Math.max(1, Math.min(100, limit));
    const page = await this.extension("history-page", { query, end, limit: limit + 1 });
    // Sign-in links and callbacks of Claude Science carry single-use secrets.
    const port = this.sciencePort(), labels = new Map(this.protectedOrigins().map(item => [item.origin, item.label]));
    const items = page.items.slice(0, limit).map(item => ({ ...item, url: publicScienceURL(item.url, port),
      ...(labels.has(webOrigin(item.url)) ? { label: labels.get(webOrigin(item.url)) } : {}) }));
    return { items, next: page.items.length > limit ? items.at(-1).time : 0 };
  }

  async historyDelete({ urls, host, start, end }) {
    let result;
    if (urls) result = await this.extension("history-delete-urls", { urls });
    else if (host) result = await this.extension("history-delete-host", { host }, 60_000);
    else result = await this.extension("history-delete-range", { start, end }, 60_000);
    this.memo = null;
    return result;
  }

  async request(input) {
    const mode = input.mode;
    if (mode === "capabilities") return this.capabilities();
    if (mode === "summary") return this.summary(input);
    if (mode === "clear") return this.clear(input);
    if (mode === "clear-sites") return this.clearSites(input);
    if (mode === "job") return { job: this.status(input.id) };
    if (mode === "sites") return this.sites(input);
    if (mode === "site") return this.site(input);
    if (mode === "cookie") return this.cookie(input);
    if (mode === "history") return this.history(input);
    if (mode === "history-delete") return this.historyDelete(input);
    throw new Error("浏览数据操作无效");
  }
}
