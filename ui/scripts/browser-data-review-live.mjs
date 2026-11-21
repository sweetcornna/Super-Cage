// Review 3 cases against the real controller and a local Chromium-family
// browser (each failed before its fix): a kept or cleared IPv4 site no
// longer breaks a clear (finding 2), a kept domain keeps a subdomain's
// localStorage with no tab and no history (finding 6), history replies stay
// bounded with long URLs (finding 12), and a reset names its request and
// reports how it ended after the controller's restart (finding 10; here the
// script restarts the controller, as systemd does in the guest).
// CAGE_TEST_BROWSER_PATH=... node scripts/browser-data-review-live.mjs
import assert from "node:assert/strict";
import { setTimeout as delay } from "node:timers/promises";
import { join } from "node:path";
import { createDataController, startSites } from "./browser-data-fixture.mjs";
import { storageOrigins } from "../../internal/cage/browser-data-storage.mjs";

const results = { checks: [], failures: [] };
const check = (label, value) => { if (value) results.checks.push(label); else results.failures.push(label); };
const { server, port } = await startSites();
const c = await createDataController();
const data = input => c.api("/data", input);
const job = async started => { for (;;) { const { job } = await data({ mode: "job", id: started.job.id }); if (job.state !== "running") return job; await delay(20); } };
const navigate = async (id, url) => {
  await c.api("/navigate", { id, url });
  await c.waitFor(async () => (await c.evaluate(id, "document.readyState === 'complete' && location.href.startsWith(" + JSON.stringify(url) + ") && typeof seed === 'function'")), "page " + url);
};
try {
  const { id: tab } = await c.api("/new", { url: `http://s0.localhost:${port}/` });
  await c.waitFor(async () => c.evaluate(tab, "typeof seed === 'function'"), "first page");

  // Finding 2: an IPv4 kept site and an IPv4 site cleared on its own.
  await navigate(tab, `http://s1.localhost:${port}/`); await c.evaluate(tab, "seed(3000)");
  const keptIP = await job(await data({ mode: "clear", since: 0, types: ["site", "cache"], keep: ["192.168.1.10", "10.0.0.1"] }));
  check("a clear with kept IPv4 sites finishes", keptIP.state === "done");
  const siteIP = await job(await data({ mode: "clear-sites", sites: ["127.0.0.1", "192.168.1.10"] }));
  check("clearing IPv4 sites finishes", siteIP.state === "done");
  const odd = await job(await data({ mode: "clear", since: 0, types: ["site"], keep: ["foo.123", "1.2.3"] }));
  check("a clear with numeric-label kept hosts finishes", odd.state === "done");
  results.f2 = { keptIP: keptIP.state, keptIPError: keptIP.error, siteIP: siteIP.state, siteIPError: siteIP.error, odd: odd.state, skipped: [keptIP.result?.skippedOrigins, siteIP.result?.skippedOrigins, odd.result?.skippedOrigins] };

  // Finding 6: app.keep.localhost keeps a token in localStorage, its cookie
  // sits on .keep.localhost, no tab is open on it and its history is gone.
  await navigate(tab, `http://app.keep.localhost:${port}/`);
  await c.evaluate(tab, "seedLocal(4000)");
  await navigate(tab, `http://s2.localhost:${port}/`);
  // Chromium commits localStorage to disk a few seconds after a write; the
  // case is a sign-in stored earlier, so wait for the commit.
  const committed = performance.now();
  await c.waitFor(async () => (await storageOrigins(join(c.directory, "profile", "Default"))).has(`http://app.keep.localhost:${port}`), "localStorage commit", 20_000);
  results.commitMs = Math.round(performance.now() - committed);
  await data({ mode: "history-delete", host: "app.keep.localhost" });
  const keptSub = await job(await data({ mode: "clear", since: 0, types: ["history", "site", "cache"], keep: ["keep.localhost"] }));
  await navigate(tab, `http://app.keep.localhost:${port}/`);
  const back = await c.evaluate(tab, "readBack()");
  check("a kept domain keeps its subdomain's localStorage without tab or history", keptSub.state === "done" && back.only === 4000);
  results.f6 = { state: keptSub.state, only: back.only };

  // Finding 12: long URLs created with pushState keep the history reply bounded.
  await navigate(tab, `http://s3.localhost:${port}/`);
  await c.evaluate(tab, "for (let i = 0; i < 120; i++) history.pushState(null, '', '/long' + i + '?' + 'q'.repeat(12000)); true");
  await delay(500);
  let page, error = "";
  try { page = await data({ mode: "history", limit: 100 }); } catch (e) { error = e.message; }
  check("history with long URLs loads", Boolean(page));
  const size = page ? JSON.stringify(page).length : 0;
  check("history reply stays under 512 KiB", page && size < 512 * 1024);
  results.f12 = { error, size, longest: page ? Math.max(...page.items.map(item => item.url.length)) : 0 };

  // Finding 10: reset with an id; the controller exits, starts on a fresh
  // profile and reports the outcome of that request.
  const before = (await c.cdp(undefined, "Storage.getCookies", {})).cookies.length;
  const exited = new Promise(resolve => c.agent().once("exit", resolve));
  await data({ mode: "reset", id: "review3-reset" });
  const code = await Promise.race([exited, delay(30_000).then(() => "timeout")]);
  await c.start();
  const status = await data({ mode: "reset-status", id: "review3-reset" }).catch(error => ({ error: error.message }));
  const after = (await c.cdp(undefined, "Storage.getCookies", {})).cookies.length;
  check("a reset restarts on a fresh profile and reports done for its id", code === 1 && status.state === "done" && before > 0 && after === 0);
  check("another id is unknown", (await data({ mode: "reset-status", id: "other" }).catch(() => ({}))).state === "unknown");
  results.f10 = { exitCode: code, status: status.state || status.error, cookiesBefore: before, cookiesAfter: after };
} finally {
  await c.close(); server.close();
}
console.log(JSON.stringify(results, null, 1));
if (results.failures.length) process.exit(1);
