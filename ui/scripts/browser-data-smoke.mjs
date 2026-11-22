// Browser data management in the workbench UI against the real controller and
// a local Chromium-family browser: the clear dialog (menu and shortcut), the
// privacy section (kept sites, automatic cleaning, sites, a site's cookies,
// history, reset), the address bar's site menu, both themes and two window
// sizes. The host side of /api/browser/data (range to since, kept sites from
// the settings file) is mirrored here; internal/workbench tests cover it.
// CAGE_UI_URL=... CAGE_TEST_BROWSER_PATH=... node scripts/browser-data-smoke.mjs
import assert from "node:assert/strict";
import { mkdir, writeFile } from "node:fs/promises";
import { setTimeout as delay } from "node:timers/promises";
import { execFileSync } from "node:child_process";
import { createWorkbench } from "./workbench-fixture.mjs";
import { createDataController, startSites } from "./browser-data-fixture.mjs";

const count = Number(process.env.BROWSER_DATA_SITES || 24);
const output = process.env.BROWSER_DATA_SHOTS || new URL("../../docs/test-reports/browser-data/", import.meta.url).pathname;
await mkdir(output, { recursive: true });
const results = { sites: count, checks: [], timings: {} };
const check = (label, value) => { assert.ok(value, label); results.checks.push(label); };
const began = Date.now(); results.sections = {};
const mark = label => { results.sections[label] = Date.now() - began; };
const { server, port } = await startSites();
const c = await createDataController();
const windows = { hour: 3_600_000, day: 86_400_000, week: 7 * 86_400_000, month: 28 * 86_400_000, all: 0 };
const settings = { defaults: { range: "day", types: ["history", "site", "cache"] }, keep: [], auto: { enabled: false, trigger: "stop", days: 7, types: ["history", "site", "cache"], last: 0, result: "" } };
const requests = [];
// What internal/workbench/browser_data.go does before the guest sees a request,
// including its refusal of fields it does not know.
const fields = ["mode", "session", "range", "since", "types", "protect", "keep", "sites", "site", "key", "action", "query", "sort", "page", "before", "limit", "urls", "entries", "time", "host", "start", "end", "id", "remember"];
// Downloads whose save failed, as the guest counts them (browser-downloads.mjs
// keeps them; its own tests cover that): set to show them in the dialog.
let unsavedDownloads = 0;
// Set to answer a clear as the host does while a different clear runs.
let refuseClear = false;
const opened = [], openedTabs = [];
const settingsFields = ["action", "domain", "range", "types", "enabled", "trigger", "days"];
const strict = (body, known) => { const extra = Object.keys(body || {}).filter(name => !known.includes(name)); if (extra.length) throw new Error("请求格式无效: " + extra.join(",")); };
const dataRoute = async body => {
  strict(body, fields);
  const input = { ...body };
  if (input.range) { input.since = windows[input.range] ? Date.now() - windows[input.range] : 0; delete input.range; }
  if (input.mode === "clear" && refuseClear) throw new Error("另一项清除正在进行，完成后再试");
  if (input.mode === "clear") {
    input.keep = settings.keep.map(entry => entry.domain); input.keepEnabled = body.protect !== "none"; delete input.protect;
    if (body.remember) settings.defaults = { range: body.range, types: body.types };
    delete input.remember;
  }
  const result = await c.api("/data", input);
  if (input.mode === "summary" && unsavedDownloads && result.downloads) result.downloads = { ...result.downloads, unsaved: unsavedDownloads };
  return result;
};
const settingsRoute = body => {
  strict(body, settingsFields);
  if (body?.action === "keep-add" && !settings.keep.some(entry => entry.domain === body.domain)) settings.keep.push({ domain: body.domain, added: Date.now() });
  if (body?.action === "keep-remove") settings.keep = settings.keep.filter(entry => entry.domain !== body.domain);
  if (body?.action === "auto") Object.assign(settings.auto, { enabled: body.enabled, trigger: body.trigger, days: body.days, types: body.types });
  return settings;
};
// Controlled inputs change once the settings request answers.
const setChecked = async (locator, value) => { await locator.click(); for (let i = 0; i < 100 && await locator.isChecked() !== value; i++) await delay(50); assert.equal(await locator.isChecked(), value); };
// CPU seconds used so far by a process and everything it started (the
// controller and its Chromium).
const treeCPU = root => {
  const rows = execFileSync("ps", ["-A", "-o", "pid=,ppid=,time="], { encoding: "utf8" }).trim().split("\n").map(line => line.trim().split(/\s+/));
  const seconds = time => time.split(":").reduce((total, part) => total * 60 + Number(part.replace(/-.*/, "")), 0);
  const tree = new Set([root]);
  for (let grew = true; grew;) { grew = false; for (const [pid, ppid] of rows) if (tree.has(Number(ppid)) && !tree.has(Number(pid))) { tree.add(Number(pid)); grew = true; } }
  return rows.filter(([pid]) => tree.has(Number(pid))).reduce((total, [, , time]) => total + seconds(time), 0);
};
const cookieNames = async () => new Set((await c.cdp(undefined, "Storage.getCookies", {})).cookies.map(cookie => cookie.domain.replace(/^\./, "") + "/" + cookie.name));
let workbench;
try {
  // Seed: N sites with cookies and storage, one kept site, history for each.
  const { id: seedTab } = await c.api("/new", { url: `http://s0.localhost:${port}/` });
  const go = async url => {
    await c.api("/navigate", { id: seedTab, url });
    await c.waitFor(async () => c.evaluate(seedTab, `document.readyState === "complete" && location.href.startsWith(${JSON.stringify(url)}) && typeof seed === "function"`), url);
  };
  for (let i = 0; i < count; i++) { await go(`http://s${i}.localhost:${port}/`); await c.evaluate(seedTab, `seed(${2000 + (i % 7) * 4000})`); }
  for (const url of [`http://kept.localhost:${port}/`, `http://app.kept.localhost:${port}/`]) { await go(url); await c.evaluate(seedTab, "seed(6000)"); }
  await go(`http://s1.localhost:${port}/`);
  mark("seeded");

  workbench = await createWorkbench({ viewport: { width: 1440, height: 900 }, browserRequest: async (route, path, body) => {
    if (path === "/browser/resize") return route.fulfill({ json: { ok: true } });
    try {
      if (path === "/browser/data") { requests.push({ at: Date.now(), mode: body.mode }); return route.fulfill({ json: await dataRoute(body) }); }
      if (path === "/browser/data-settings") return route.fulfill({ json: settingsRoute(body) });
      if (path === "/browser/new") { opened.push(body?.url || ""); const reply = await c.api("/new", body ?? undefined); openedTabs.push(reply.id); return route.fulfill({ json: reply }); }
      return route.fulfill({ json: await c.api(path.replace("/browser", ""), body ?? undefined) });
    } catch (error) { return route.fulfill({ status: /请求格式无效/.test(error.message) ? 400 : 409, contentType: "text/plain; charset=utf-8", body: error.message }); }
  } });
  const { page, errors } = workbench;
  page.setDefaultTimeout(15000);
  await page.emulateMedia({ colorScheme: "light" });
  workbench.state.status.ready = workbench.state.status.vm_running = true;
  await page.reload();
  await page.getByRole("button", { name: "打开浏览器", exact: true }).click();
  await page.getByRole("tab", { name: "s1.localhost", exact: true }).waitFor();
  // Screenshots wait for the dialogs' entrance animations.
  const shot = async name => { await delay(350); await page.screenshot({ path: output + name + ".png" }); };
  const menu = () => page.getByRole("button", { name: "浏览器菜单", exact: true }).first();
  const dialog = page.getByRole("dialog", { name: "清除浏览数据" });
  const openPrivacy = section => page.evaluate(view => document.dispatchEvent(new CustomEvent("cage-browser-data", { detail: { view: "privacy", section: view } })), section);
  const loaded = () => page.waitForFunction(() => { const metas = [...document.querySelectorAll(".bd-meta")]; return metas.length > 0 && metas.every(node => node.textContent && !node.textContent.includes("正在统计")); });

  // 1. The clear dialog from the menu: sizes, defaults, both sessions. The
  // workbench loads its own lazy panes right after the first paint; measure
  // once that has settled (up to 10 s), as a user opening the dialog would.
  let started = Date.now();
  for (let last = -1, count = 0; Date.now() - started < 10000 && count !== last; await delay(500)) { last = count; count = await page.evaluate(() => performance.getEntriesByType("resource").length); }
  results.timings.settleMs = Date.now() - started;
  started = Date.now();
  await menu().click();
  await page.getByRole("menuitem", { name: "清除浏览数据…", exact: true }).click();
  await dialog.waitFor(); await loaded();
  results.timings.dialogFirstOpenMs = Date.now() - started;
  const metas = await dialog.locator(".bd-meta").allTextContents();
  check("the dialog says what each category holds", metas.length === 4 && /条记录/.test(metas[0]) && /个网站/.test(metas[1]) && /释放/.test(metas[2]));
  check("defaults: past 24 hours, history, site data and cache", (await dialog.getByRole("combobox", { name: "时间范围" }).textContent()).includes("过去 24 小时")
    && await dialog.getByRole("checkbox", { name: /浏览记录/ }).isChecked() && !(await dialog.getByRole("checkbox", { name: /下载记录/ }).isChecked()));
  check("passwords point to the vault and permissions to Chromium", await dialog.getByText("已保存的密码属于 Super Cage 密码保险箱。").count() === 1 && await dialog.getByRole("button", { name: "查看网站权限" }).count() === 1);
  await shot("clear-dialog-1440-light");
  await dialog.getByRole("button", { name: "取消", exact: true }).click();
  await dialog.waitFor({ state: "hidden" });
  started = Date.now();
  await menu().click();
  await page.getByRole("menuitem", { name: "清除浏览数据…", exact: true }).click();
  await dialog.waitFor(); await loaded();
  results.timings.dialogOpenMs = Date.now() - started;
  await page.keyboard.press("Escape");
  await dialog.waitFor({ state: "hidden" });

  mark("dialog");
  // 2. The privacy section: kept sites, automatic cleaning.
  await page.evaluate(() => document.dispatchEvent(new CustomEvent("cage-browser-data", { detail: { view: "privacy" } })));
  const privacy = page.getByRole("region", { name: "隐私与网站数据" });
  await privacy.getByRole("button", { name: /^清除浏览数据/ }).waitFor();
  await privacy.getByRole("textbox", { name: "要保留的网站" }).fill(`https://www.kept.localhost:${port}/login`);
  await privacy.getByRole("button", { name: "添加", exact: true }).click();
  await privacy.getByText("kept.localhost", { exact: true }).waitFor();
  check("typing a URL keeps its domain", settings.keep.length === 1 && settings.keep[0].domain === "kept.localhost");
  check("open pages are offered as one-click additions", await privacy.locator(".bd-suggest button").count() >= 1);
  await setChecked(privacy.getByRole("switch", { name: "自动清除浏览数据" }), true);
  await setChecked(privacy.getByRole("radio", { name: /定期清除/ }), true);
  await privacy.getByRole("combobox", { name: "清理间隔" }).waitFor();
  check("automatic cleaning saves its choices", settings.auto.enabled && settings.auto.trigger === "days");
  await shot("privacy-1440-light");
  await setChecked(privacy.getByRole("switch", { name: "自动清除浏览数据" }), false);
  check("automatic cleaning turns off", settings.auto.enabled === false);

  mark("privacy");
  // 3. Sites: list, search, sort, multi-select clear.
  await privacy.getByRole("button", { name: /^网站数据/ }).click();
  const sites = page.getByRole("region", { name: "网站数据" });
  started = Date.now();
  await sites.locator(".bd-site-row").first().waitFor();
  results.timings.siteListMs = Date.now() - started;
  check("the site list shows the seeded sites", Number((await sites.locator(".bd-view-title p").textContent()).match(/^[\d,]+/)[0].replace(/,/g, "")) >= Math.min(count, 50));
  const search = sites.getByRole("textbox", { name: "搜索网站" });
  await search.fill("kept.localhost");
  await page.waitForFunction(() => document.querySelectorAll(".bd-site-row").length === 1);
  check("the kept site is marked", await sites.locator(".bd-site-row", { hasText: "kept.localhost" }).getByText("保持登录").count() === 1);
  await search.fill("");
  await page.waitForFunction(() => document.querySelectorAll(".bd-site-row").length > 1);
  await shot("sites-1440-light");
  await sites.getByRole("textbox", { name: "搜索网站" }).fill("s2.localhost");
  await sites.locator(".bd-site-row").filter({ hasText: "s2.localhost" }).first().waitFor();
  await page.waitForFunction(() => document.querySelectorAll(".bd-site-row").length === 1);
  await sites.getByRole("textbox", { name: "搜索网站" }).fill("");
  await sites.getByRole("combobox", { name: "网站排序" }).click();
  await page.getByRole("option", { name: "按名称", exact: true }).click();
  await page.waitForFunction(() => document.querySelector(".bd-site-row .list-row-title")?.textContent.startsWith("kept.localhost"));
  // A selection survives searching, so sites far down a long list can be picked.
  for (const site of ["s3.localhost", "s4.localhost"]) {
    await search.fill(site);
    await sites.getByRole("checkbox", { name: `选择 ${site}`, exact: true }).check();
  }
  await search.fill("");
  await sites.getByRole("button", { name: "清除所选网站数据" }).click();
  await sites.getByRole("button", { name: "确认清除", exact: true }).click();
  await sites.getByText(/已清除 2 个网站的数据/).waitFor();
  const afterSites = await cookieNames();
  check("selected sites lose their cookies, others keep theirs", !afterSites.has("s3.localhost/js") && !afterSites.has("s4.localhost/js") && afterSites.has("s5.localhost/js"));

  // 4. A site's cookies (the former cookie manager).
  await search.fill("s5.localhost");
  await sites.getByRole("button", { name: "s5.localhost 详情", exact: true }).click();
  const detail = page.getByRole("region", { name: "s5.localhost 的网站数据" });
  await detail.getByRole("list", { name: "Cookie 列表" }).waitFor();
  await detail.locator(".bd-cookie", { hasText: "js" }).getByRole("button", { name: "查看值" }).click();
  await detail.locator(".bd-cookie-value").waitFor();
  check("a cookie's value shows on request", (await detail.locator(".bd-cookie-value").textContent()) === "1");
  await shot("site-detail-1440-light");
  await detail.getByRole("button", { name: "删除 Cookie js", exact: true }).click();
  await page.waitForFunction(() => ![...document.querySelectorAll(".bd-cookie-head strong")].some(node => node.textContent === "js"));
  check("one cookie can be deleted", !(await cookieNames()).has("s5.localhost/js") && (await cookieNames()).has("s5.localhost/srv"));
  await detail.getByRole("button", { name: "返回网站列表" }).click();
  await sites.getByRole("button", { name: "返回隐私与网站数据" }).click();

  mark("sites");
  // 5. History: by day, search, delete one entry.
  await privacy.getByRole("button", { name: /^浏览记录/ }).click();
  const history = page.getByRole("region", { name: "浏览记录" });
  await history.locator(".bd-day").first().waitFor();
  check("history is grouped by day", (await history.locator(".bd-day-head h4").first().textContent()).startsWith("今天"));
  await shot("history-1440-light");
  await history.getByRole("textbox", { name: "搜索浏览记录" }).fill("s6.localhost");
  await page.waitForFunction(() => { const rows = [...document.querySelectorAll(".bd-history-row")]; return rows.length >= 1 && rows.every(row => row.textContent.includes("s6.localhost")); });
  await history.locator(".bd-history-row").first().getByRole("button", { name: /的记录$/ }).click();
  await history.getByText("没有匹配的记录").waitFor();
  check("an entry can be deleted", !(await c.api("/data", { mode: "history", query: "s6.localhost" })).items.length);
  await history.getByRole("textbox", { name: "搜索浏览记录" }).fill("");
  await history.getByRole("button", { name: "返回隐私与网站数据" }).click();

  // 6. Reset is guarded and says what stays.
  await privacy.getByRole("button", { name: "重置浏览器…" }).click();
  const reset = page.getByRole("dialog", { name: "重置浏览器？" });
  check("reset needs an acknowledgement", await reset.getByRole("button", { name: "重置浏览器", exact: true }).isDisabled());
  check("reset lists what is kept", await reset.getByText("收藏夹和密码保险箱").count() === 1);
  await shot("reset-1440-light");
  await reset.getByRole("button", { name: "取消", exact: true }).click();

  mark("history+reset");
  // 7. Idle with the settings page open: no requests, little main-thread work.
  const session = await page.context().newCDPSession(page);
  await session.send("Performance.enable");
  const before = (await session.send("Performance.getMetrics")).metrics.find(metric => metric.name === "TaskDuration").value;
  const quietFrom = Date.now(), controllerBefore = treeCPU(c.agent().pid);
  await delay(10_000);
  results.timings.idleControllerCPUmsPer10s = Math.round((treeCPU(c.agent().pid) - controllerBefore) * 1000);
  const after = (await session.send("Performance.getMetrics")).metrics.find(metric => metric.name === "TaskDuration").value;
  results.timings.idleTaskMsPer10s = Math.round((after - before) * 1000);
  check("nothing polls while the settings page is open", !requests.some(request => request.at >= quietFrom));

  mark("idle");
  // 8. The address bar's site menu clears one site.
  await page.getByRole("tab", { name: "s1.localhost", exact: true }).click();
  await page.getByRole("button", { name: "网站数据", exact: true }).click();
  const popover = page.getByRole("dialog", { name: "s1.localhost" });
  await popover.getByText(/个 Cookie/).waitFor();
  const [anchorBox, popoverBox] = [await page.getByRole("button", { name: "网站数据", exact: true }).boundingBox(), await popover.boundingBox()];
  check("the site menu opens under its button", Math.abs(popoverBox.x - anchorBox.x) < 2 && popoverBox.y > anchorBox.y);
  await shot("site-menu-1440-light");
  await popover.getByRole("button", { name: "清除此网站数据" }).click();
  await popover.getByRole("button", { name: "确认清除", exact: true }).click();
  await popover.getByText(/已清除 1 个网站的数据/).waitFor();
  check("the site menu clears this site", !(await cookieNames()).has("s1.localhost/js"));
  await popover.getByRole("button", { name: "完成", exact: true }).click();

  // 9. The shortcut opens the dialog; clear everything, keeping kept.localhost.
  await page.locator(".pane-body").click({ position: { x: 300, y: 300 } }).catch(() => {});
  started = Date.now();
  await page.keyboard.press(process.platform === "darwin" ? "Meta+Shift+Backspace" : "Control+Shift+Delete");
  await dialog.waitFor(); await loaded();
  results.timings.shortcutOpenMs = Date.now() - started;
  await dialog.getByRole("combobox", { name: "时间范围" }).click();
  await page.getByRole("option", { name: "全部时间", exact: true }).click();
  await dialog.getByRole("checkbox", { name: /下载记录/ }).check();
  // Review round 2, N2: a different clear running in another window is
  // refused in plain words, and this one can be started again afterwards.
  refuseClear = true;
  await dialog.getByRole("button", { name: "清除数据", exact: true }).click();
  await dialog.getByText("另一项清除正在进行，完成后再试").waitFor();
  check("a different clear running elsewhere is refused in the dialog", await dialog.getByRole("button", { name: "清除数据", exact: true }).isEnabled());
  refuseClear = false;
  started = Date.now();
  await dialog.getByRole("button", { name: "清除数据", exact: true }).click();
  await dialog.getByText(/^已清除浏览记录/).waitFor();
  results.timings.clearMs = Date.now() - started;
  results.resultLine = await dialog.locator(".bd-result p").textContent();
  await shot("clear-result-1440-light");
  const afterClear = await cookieNames();
  check("everything else is cleared", !afterClear.has("s5.localhost/srv") && !afterClear.has("s0.localhost/js"));
  check("the kept site and its subdomains stay signed in", afterClear.has("kept.localhost/js") && afterClear.has("app.kept.localhost/js"));
  check("the result says what was kept", /保持登录的网站和内置页面未受影响/.test(results.resultLine));
  check("defaults are remembered", settings.defaults.range === "all" && settings.defaults.types.includes("downloads"));
  await dialog.getByRole("button", { name: "完成", exact: true }).click();

  // 10. 管理 Cookie opens the site's data.
  await menu().click();
  await page.getByRole("menuitem", { name: "管理 Cookie", exact: true }).click();
  const cookies = page.getByRole("dialog", { name: "Cookie 和网站数据" });
  await cookies.getByRole("region", { name: "s1.localhost 的网站数据" }).waitFor();
  await cookies.getByRole("list", { name: "Cookie 列表" }).waitFor();
  check("管理 Cookie opens the site's data and cookies", true);
  await shot("site-dialog-1440-light");
  await page.keyboard.press("Escape");

  mark("menu+shortcut");
  // 11. Review 3: unsaved downloads in the dialog; a long address in history
  // is shown cut, opened whole and deleted by its id.
  unsavedDownloads = 2;
  await menu().click();
  await page.getByRole("menuitem", { name: "清除浏览数据…", exact: true }).click();
  await dialog.waitFor(); await loaded();
  check("the dialog says unsaved downloads stay", /2 个未保存成功的会保留/.test(await dialog.getByRole("checkbox", { name: /下载记录/ }).locator("xpath=..").textContent()));
  await page.keyboard.press("Escape");
  unsavedDownloads = 0;
  await go(`http://s8.localhost:${port}/`);
  await c.evaluate(seedTab, "history.pushState(null, '', '/long?' + 'q'.repeat(6000)); true");
  await openPrivacy("history");
  await history.getByRole("textbox", { name: "搜索浏览记录" }).fill("s8.localhost/long");
  const longRow = history.locator(".bd-history-row").first();
  await longRow.waitFor();
  const shownTitle = await longRow.locator(".bd-history-open").getAttribute("title");
  check("a long address arrives cut", shownTitle.length <= 2049);
  await longRow.locator(".bd-history-open").click();
  for (let i = 0; i < 100 && !opened.some(url => url.length > 6000); i++) await delay(50);
  check("opening it fetches the whole address", opened.some(url => url.length > 6000 && url.includes("/long?")));
  // The opened page adds a visit of its own when it commits; wait for it, or
  // a slow machine shows that new visit after the deletion below.
  await c.waitFor(async () => openedTabs.length && await c.evaluate(openedTabs.at(-1), "document.readyState === 'complete'").catch(() => false), "the opened page");
  // Opening showed the page; back to the history to delete the entry.
  await openPrivacy("history");
  await history.getByRole("textbox", { name: "搜索浏览记录" }).fill("s8.localhost/long");
  await longRow.waitFor();
  await longRow.getByRole("button", { name: /的记录$/ }).click();
  await history.getByText("没有匹配的记录").waitFor();
  check("deleting it removes the whole address", !(await c.api("/data", { mode: "history", query: "s8.localhost/long" })).items.length);
  await history.getByRole("textbox", { name: "搜索浏览记录" }).fill("");
  mark("review3");
  // The screenshot matrix: every view in the other theme and size combinations
  // (light at 1440×900 was taken along the way).
  for (const url of [`http://s7.localhost:${port}/`, `http://kept.localhost:${port}/`, `http://s1.localhost:${port}/`]) await go(url);
  const views = [
    ["clear-dialog", async () => { await menu().click(); await page.getByRole("menuitem", { name: "清除浏览数据…", exact: true }).click(); await dialog.waitFor(); await loaded(); }, () => page.keyboard.press("Escape")],
    ["privacy", async () => { await openPrivacy(""); await privacy.getByRole("button", { name: /^网站数据/ }).waitFor(); }],
    ["sites", async () => { await openPrivacy("sites"); await sites.locator(".bd-site-row").first().waitFor(); }],
    ["site-detail", async () => { await sites.getByRole("button", { name: "kept.localhost 详情" }).click(); await page.getByRole("region", { name: "kept.localhost 的网站数据" }).getByRole("list", { name: "Cookie 列表" }).waitFor(); }],
    ["history", async () => { await openPrivacy("history"); await history.locator(".bd-day").first().waitFor(); }],
    ["reset", async () => { await openPrivacy(""); await privacy.getByRole("button", { name: "重置浏览器…" }).click(); await reset.waitFor(); }, () => reset.getByRole("button", { name: "取消", exact: true }).click()],
    ["site-menu", async () => { await page.getByRole("tab", { name: "s1.localhost", exact: true }).click(); await page.getByRole("button", { name: "网站数据", exact: true }).click(); await popover.getByText(/个 Cookie/).waitFor(); }, () => page.keyboard.press("Escape")],
    ["site-dialog", async () => { await menu().click(); await page.getByRole("menuitem", { name: "管理 Cookie", exact: true }).click(); await cookies.getByRole("list", { name: "Cookie 列表" }).waitFor(); }, () => page.keyboard.press("Escape")],
  ];
  for (const [scheme, width, height] of [["dark", 1440, 900], ["light", 1000, 700], ["dark", 1000, 700]]) {
    await page.emulateMedia({ colorScheme: scheme });
    await page.setViewportSize({ width, height });
    for (const [name, open, close] of views) { await open(); await shot(`${name}-${width}-${scheme}`); await close?.(); }
    check(`no horizontal overflow at ${width}×${height} ${scheme}`, !(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1)));
  }
  mark("matrix");
  // The dialog's permissions link opens Chromium's page in a tab and shows it.
  await page.setViewportSize({ width: 1440, height: 900 });
  await menu().click();
  await page.getByRole("menuitem", { name: "清除浏览数据…", exact: true }).click();
  await dialog.waitFor(); await loaded();
  await dialog.getByRole("button", { name: "查看网站权限", exact: true }).click();
  await dialog.waitFor({ state: "hidden" });
  await page.waitForFunction(() => document.body.innerText.includes("chrome://settings/content") || [...document.querySelectorAll("input")].some(input => input.value.includes("chrome://settings/content")), null, { timeout: 15000 });
  check("查看网站权限 opens Chromium's page from the menu's dialog", true);
  check("no page errors", errors.length === 0);
  results.passed = true;
  await writeFile(output + "result.json", JSON.stringify(results, null, 2) + "\n");
  console.log(JSON.stringify(results, null, 1));
} catch (error) {
  await workbench?.page.screenshot({ path: output + "failure.png" }).catch(() => {});
  console.error(workbench?.errors);
  throw error;
} finally {
  await workbench?.browser.close(); await c.close(); server.close();
}
