// Browser data against the real controller and a local Chromium-family
// browser: what a clear keeps and removes, private and sign-in isolation,
// history, reset, and the timings reported for "fast and light".
// CAGE_TEST_BROWSER_PATH=... BROWSER_DATA_SITES=300 node scripts/browser-data-live.mjs
import assert from "node:assert/strict";
import http from "node:http";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";
import { setTimeout as delay } from "node:timers/promises";
import { createDataController, startSites } from "./browser-data-fixture.mjs";

const count = Number(process.env.BROWSER_DATA_SITES || 60);
const results = { sites: count, checks: [] };
const check = (label, value) => { assert.ok(value, label); results.checks.push(label); };
const { server, port } = await startSites();
const { server: other, port: otherPort } = await startSites();
// The file editor's origin, when this machine has the port free.
const editor = await new Promise(resolve => { const s = http.createServer((req, res) => { res.setHeader("Set-Cookie", "code-server-session=1; Path=/; HttpOnly"); res.setHeader("Content-Type", "text/html"); res.end("<title>editor</title><script>localStorage.setItem('editor','1')</script>"); }); s.once("error", () => resolve(null)); s.listen(18080, "127.0.0.1", () => resolve(s)); });
const c = await createDataController();
const data = input => c.api("/data", input);
const job = async started => { for (;;) { const { job } = await data({ mode: "job", id: started.job.id }); if (job.state !== "running") return job; await delay(20); } };
const open = async (url, incognito = false) => {
  const { id } = await c.api("/new", { url, incognito });
  await c.waitFor(async () => (await c.evaluate(id, "document.readyState === 'complete' && location.href.startsWith(" + JSON.stringify(url) + ")")), "page " + url);
  return id;
};
const navigate = async (id, url) => {
  await c.api("/navigate", { id, url });
  await c.waitFor(async () => (await c.evaluate(id, "document.readyState === 'complete' && location.href.startsWith(" + JSON.stringify(url) + ") && typeof seed === 'function'")), "page " + url);
};
const cookieHosts = async () => new Set((await c.cdp(undefined, "Storage.getCookies", {})).cookies.map(cookie => cookie.domain.replace(/^\./, "") + "/" + cookie.name));
try {
  check("capability reported by the live extension page", (await data({ mode: "capabilities" })).available === true);
  const seedTab = await open(`http://s0.localhost:${port}/`);
  let started = performance.now();
  for (let i = 0; i < count; i++) { await navigate(seedTab, `http://s${i}.localhost:${port}/`); await c.evaluate(seedTab, `seed(${2000 + (i % 10) * 3000})`); }
  results.seedMs = Math.round(performance.now() - started);
  for (const url of [`http://kept.localhost:${port}/`, `http://sub.kept.localhost:${port}/`, `http://kept.localhost:${otherPort}/`, `http://quiet.kept.localhost:${port}/`]) {
    await navigate(seedTab, url); await c.evaluate(seedTab, "seed(4000)");
  }
  await data({ mode: "history-delete", host: "quiet.kept.localhost" });
  if (editor) { await navigate(seedTab, "http://127.0.0.1:18080/").catch(() => {}); await c.waitFor(() => c.evaluate(seedTab, "localStorage.getItem('editor') === '1'"), "editor storage"); }
  await navigate(seedTab, `http://s1.localhost:${port}/`);
  const privateTab = await open(`http://s0.localhost:${port}/`, true);
  await c.waitFor(() => c.evaluate(privateTab, "typeof seed === 'function'"), "private seed");
  await c.evaluate(privateTab, "seed(3000)");

  // What the dialog and the site list cost to open (cold, then the memo).
  started = performance.now(); const summary = await data({ mode: "summary", since: 0 }); results.summaryColdMs = Math.round(performance.now() - started);
  started = performance.now(); const list = await data({ mode: "sites", sort: "size" }); results.sitesWarmMs = Math.round(performance.now() - started);
  await delay(21000);
  started = performance.now(); await data({ mode: "sites", sort: "recent" }); results.sitesColdMs = Math.round(performance.now() - started);
  results.summary = { history: summary.history, sites: summary.sites, cacheBytes: summary.cache.bytes };
  check("site list covers the seeded sites", list.total >= count);
  const kept = (await data({ mode: "sites", query: "kept.localhost" })).sites;
  check("subdomains group under their site", kept.length === 1 && kept[0].site === "kept.localhost");

  // Default clear of everything, keeping kept.localhost.
  started = performance.now();
  const done = await job(await data({ mode: "clear", since: 0, types: ["history", "site", "cache", "downloads"], keep: ["kept.localhost"] }));
  results.clearMs = Math.round(performance.now() - started);
  results.clearResult = { freedBytes: done.result.freedBytes, reloaded: done.result.reloaded, kept: done.result.kept.length };
  check("the clear finished", done.state === "done");
  const hosts = await cookieHosts();
  check("ordinary sites lost their cookies", !hosts.has("s5.localhost/srv") && !hosts.has("s1.localhost/js"));
  check("kept domain kept its cookies", hosts.has("kept.localhost/srv") && hosts.has("kept.localhost/dom"));
  if (editor) check("the file editor stays signed in (127.0.0.1 cookie)", hosts.has("127.0.0.1/code-server-session"));
  const readBack = async url => { await navigate(seedTab, url); return c.evaluate(seedTab, "readBack()"); };
  check("kept origin storage survives", (await readBack(`http://kept.localhost:${port}/`)).local === 4000);
  check("kept subdomain storage survives", (await readBack(`http://sub.kept.localhost:${port}/`)).local === 4000);
  check("kept host on another port survives", (await readBack(`http://kept.localhost:${otherPort}/`)).local === 4000);
  results.limit = { originWithoutHistoryKept: (await readBack(`http://quiet.kept.localhost:${port}/`)).local === 4000 };
  check("ordinary site storage is gone", (await readBack(`http://s3.localhost:${port}/`)).local === 0);
  if (editor) { await navigate(seedTab, "http://127.0.0.1:18080/").catch(() => {}); check("editor storage survives", await c.evaluate(seedTab, "localStorage.getItem('editor') === '1'")); }
  check("the private session was not touched", (await c.evaluate(privateTab, "readBack()")).local === 3000);
  const remaining = (await data({ mode: "history" })).items.map(item => new URL(item.url).hostname);
  check("history cleared (only pages visited since remain)", !remaining.includes("s5.localhost") && !remaining.includes("s0.localhost"));

  // Per-site clear on the kept site, then the private session.
  await navigate(seedTab, `http://kept.localhost:${port}/`); await c.evaluate(seedTab, "seed(4000)");
  started = performance.now();
  const site = await job(await data({ mode: "clear-sites", sites: ["kept.localhost"] }));
  results.clearSiteMs = Math.round(performance.now() - started);
  // The reloaded page sets its response cookies again; seed() wrote js and storage.
  check("clear this site removes its cookies and storage", site.state === "done" && !(await cookieHosts()).has("kept.localhost/js") && (await readBack(`http://kept.localhost:${port}/`)).local === 0);
  check("the page of the cleared site reloaded", site.result.reloaded >= 1);
  const privateDone = await job(await data({ mode: "clear", session: "private", types: ["site", "cache"] }));
  await c.waitFor(() => c.evaluate(privateTab, "document.readyState === 'complete' && typeof readBack === 'function'"), "private page reloaded");
  check("private clear removes private data only", privateDone.state === "done" && privateDone.result.reloaded === 1 && (await c.evaluate(privateTab, "readBack()")).local === 0);

  // History view: entries, search, delete one and a range.
  for (const i of [7, 8, 9]) await navigate(seedTab, `http://s${i}.localhost:${port}/`);
  const page = await data({ mode: "history", limit: 2 });
  check("history pages with a cursor", page.items.length === 2 && page.next > 0);
  await data({ mode: "history-delete", urls: [page.items[0].url] });
  check("an entry can be deleted", !(await data({ mode: "history" })).items.some(item => item.url === page.items[0].url));
  await data({ mode: "history-delete", start: 0, end: Date.now() + 1000 });
  check("a range can be deleted", (await data({ mode: "history" })).items.length === 0);

  // Reset: Chromium closes, the controller exits non-zero (systemd restarts it).
  const before = c.agent();
  await data({ mode: "reset" });
  await new Promise(resolve => before.exitCode !== null ? resolve() : before.once("exit", resolve));
  check("controller exits non-zero for the restart", before.exitCode !== 0);
  started = performance.now(); await c.start(); results.resetRestartMs = Math.round(performance.now() - started);
  const preferences = JSON.parse(readFileSync(join(c.directory, "profile", "Default", "Preferences"), "utf8"));
  check("fresh profile keeps the seeded language", preferences.intl?.accept_languages === "zh-CN" && preferences.intl?.selected_languages === "zh-CN");
  check("fresh profile has no cookies", (await c.cdp(undefined, "Storage.getCookies", {})).cookies.length === 0);
  check("built-in extension loads on the fresh profile", (await data({ mode: "capabilities" })).available === true);
  await c.waitFor(() => !readdirSync(c.directory).some(name => name.startsWith("profile.reset-")), "old profile removed", 30000);
  check("old profile removed in the background", true);
} finally {
  await c.close(); server.close(); other.close(); editor?.close();
}
console.log(JSON.stringify(results, null, 1));
