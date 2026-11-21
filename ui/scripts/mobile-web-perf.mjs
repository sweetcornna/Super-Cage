// Phone path load numbers through the portal mock (ui/tenant/mock-server.mjs serves the real build with
// the workbench's caching and brotli). Per document: requests and bytes to the first usable screen, cold and
// cached, under Lighthouse's slow-4G profile (150 ms RTT, 1.6 Mbit/s down) with 4x CPU slowdown; layout shift;
// long tasks; idle requests and CPU with the session list open; heap on the terminal screen.
// Run after npm --prefix ui run build. Chromium only (CDP). Prints JSON; MOBILE_WEB_PERF_OUT=<file> saves it.
import { createRequire } from "node:module";
import { writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { startMockServer } from "../tenant/mock-server.mjs";

const require = createRequire(import.meta.url);
const { chromium } = require(process.env.CAGE_PLAYWRIGHT_MODULE || "playwright");
const workbenchDir = process.env.CAGE_WORKBENCH_DIR || fileURLToPath(new URL("../../internal/workbench/web/", import.meta.url));
const iPhone = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1";
const idleSeconds = Number(process.env.MOBILE_WEB_PERF_IDLE || 30);
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

const mock = await startMockServer({ workbenchDir, bootDelayMs: 50, phoneFixture: true });
const browser = await chromium.launch({ executablePath: process.env.CAGE_TEST_BROWSER_PATH });
const results = { profile: "slow 4G (150 ms, 1.6 Mbit/s down, 750 kbit/s up), CPU x4, 390x844 @3x", documents: {} };
try {
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true, userAgent: iPhone });
  const page = await context.newPage();
  await page.addInitScript(() => {
    window.__perf = { usable: 0, shift: 0, shiftAfterPaint: 0, longTasks: 0, longTaskMs: 0 };
    // The phone home (greeting and recent sessions) or the desktop home (greeting and sidebar).
    const usable = () => document.querySelector(".mobile-app .recent .session-select, .mobile-app .greeting, .workbench-shell .home-greeting");
    new MutationObserver(() => { if (!window.__perf.usable && usable()) window.__perf.usable = performance.now(); })
      .observe(document, { subtree: true, childList: true });
    try {
      new PerformanceObserver((list) => {
        for (const entry of list.getEntries()) {
          if (entry.hadRecentInput) continue;
          window.__perf.shift += entry.value;
          if (performance.getEntriesByName("first-contentful-paint")[0]?.startTime < entry.startTime) window.__perf.shiftAfterPaint += entry.value;
        }
      }).observe({ type: "layout-shift", buffered: true });
      new PerformanceObserver((list) => { for (const entry of list.getEntries()) { window.__perf.longTasks++; window.__perf.longTaskMs += entry.duration; } })
        .observe({ type: "longtask", buffered: true });
    } catch { /* not supported */ }
  });
  const post = (path, data) => page.request.post(mock.origin + path, { data, headers: { Origin: mock.origin } });
  await post("/api/tenant/register", { username: "perfowner", password: "fixture password only" });
  const { vm } = await (await post("/api/tenant/vms", { name: "开发环境" })).json();
  let open;
  for (let i = 0; i < 200; i++) {
    open = await post(`/api/tenant/vms/${vm.id}/open`, {});
    if (open.status() === 200) break;
    await sleep(50);
  }
  const { workspace_url: entry } = await open.json();
  const origin = new URL(entry).origin;
  await page.goto(entry);
  await page.waitForURL((url) => url.pathname === "/", { timeout: 15000 });
  const cdp = await context.newCDPSession(page);
  await cdp.send("Network.enable");
  await cdp.send("Performance.enable");
  const requests = new Map();
  cdp.on("Network.requestWillBeSent", (event) => { if (!event.request.url.startsWith("data:")) requests.set(event.requestId, { url: event.request.url, bytes: 0, cached: false }); });
  cdp.on("Network.responseReceived", (event) => { const r = requests.get(event.requestId); if (r) r.cached = event.response.fromDiskCache || event.response.fromMemoryCache || event.response.status === 304; });
  cdp.on("Network.loadingFinished", (event) => { const r = requests.get(event.requestId); if (r) r.bytes = event.encodedDataLength; });
  const throttle = async (on) => {
    await cdp.send("Network.emulateNetworkConditions", on
      ? { offline: false, latency: 150, downloadThroughput: 1638.4 * 1024 / 8, uploadThroughput: 750 * 1024 / 8 }
      : { offline: false, latency: 0, downloadThroughput: -1, uploadThroughput: -1 });
    await cdp.send("Emulation.setCPUThrottlingRate", { rate: on ? 4 : 1 });
  };
  const load = async (cold) => {
    if (cold) await cdp.send("Network.clearBrowserCache");
    requests.clear();
    await throttle(true);
    const start = Date.now();
    await page.goto(origin + "/", { waitUntil: "commit" });
    await page.waitForFunction(() => window.__perf?.usable > 0, null, { timeout: 120000, polling: 200 });
    await page.waitForLoadState("networkidle").catch(() => {});
    const perf = await page.evaluate(() => ({ ...window.__perf, fcp: performance.getEntriesByName("first-contentful-paint")[0]?.startTime || 0 }));
    await throttle(false);
    const list = [...requests.values()];
    const assets = list.filter((r) => !r.url.includes("/api/"));
    return {
      wall_ms: Date.now() - start,
      first_contentful_paint_ms: Math.round(perf.fcp),
      first_usable_ms: Math.round(perf.usable),
      layout_shift: Number(perf.shift.toFixed(4)), layout_shift_after_paint: Number(perf.shiftAfterPaint.toFixed(4)),
      long_tasks: perf.longTasks, long_task_ms: Math.round(perf.longTaskMs),
      requests: list.length, asset_requests: assets.length,
      transferred_bytes: list.reduce((sum, r) => sum + r.bytes, 0),
      asset_bytes: assets.reduce((sum, r) => sum + r.bytes, 0),
      revalidated_or_cached: list.filter((r) => r.cached).length,
      desktop_chunks: assets.filter((r) => /\/assets\/(?:desktop|main|remote-hosts)-/.test(r.url)).map((r) => new URL(r.url).pathname),
      files: assets.map((r) => `${new URL(r.url).pathname} ${r.bytes}`),
    };
  };
  for (const [name, layout] of [["desktop document at / (what a phone got before)", "desktop"], ["phone document at /", "phone"]]) {
    await context.addCookies([{ name: "cage-layout", value: layout, url: origin }]);
    results.documents[name] = { cold: await load(true), cached: await load(false) };
  }
  // Idle: the phone home with the session list, unthrottled.
  requests.clear();
  const metric = async () => Object.fromEntries((await cdp.send("Performance.getMetrics")).metrics.map((m) => [m.name, m.value]));
  const before = await metric();
  await sleep(idleSeconds * 1000);
  const after = await metric();
  const idle = [...requests.values()];
  results.idle = {
    seconds: idleSeconds,
    requests_per_minute: Math.round(idle.length * 60 / idleSeconds),
    bytes_per_minute: Math.round(idle.reduce((s, r) => s + r.bytes, 0) * 60 / idleSeconds),
    cpu_ms_per_minute: Math.round((after.TaskDuration - before.TaskDuration) * 1000 * 60 / idleSeconds),
    paths: [...new Set(idle.map((r) => new URL(r.url).pathname))],
    heap_mb: Number((after.JSHeapUsedSize / 1048576).toFixed(1)),
  };
  // Terminal screen heap (xterm is loaded on demand).
  await page.getByRole("button", { name: "展开侧栏", exact: true }).click();
  await page.getByRole("dialog", { name: "工作区侧栏" }).getByRole("button", { name: "终端", exact: true }).click();
  await page.getByRole("button", { name: /^claude/ }).first().click();
  await page.locator(".xterm").first().waitFor({ timeout: 15000 });
  await sleep(1500);
  await cdp.send("HeapProfiler.collectGarbage").catch(() => {});
  const terminal = await metric();
  results.terminal = { heap_mb: Number((terminal.JSHeapUsedSize / 1048576).toFixed(1)), nodes: terminal.Nodes };
} finally {
  await browser.close();
  await mock.close();
}
const text = JSON.stringify(results, null, 2);
if (process.env.MOBILE_WEB_PERF_OUT) await writeFile(process.env.MOBILE_WEB_PERF_OUT, text + "\n");
console.log(text);
