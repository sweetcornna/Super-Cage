// Browser data on servers and guests that do not have it: an old server
// (404), an old guest (available:false, reason "update") keep the earlier
// cookie panel, privacy section and Chromium's clear page; a busy or
// unreachable guest shows its error with a retry instead of switching screens;
// a browser-data chunk that fails to load falls back the same way, without
// the app's error screen, and the next use loads it again.
// CAGE_UI_URL=... CAGE_TEST_BROWSER_PATH=... node scripts/browser-data-fallback-smoke.mjs
import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";
import { createDataController, startSites } from "./browser-data-fixture.mjs";

const results = { checks: [] };
const check = (label, value) => { assert.ok(value, label); results.checks.push(label); };
const { server, port } = await startSites();
const c = await createDataController();
let variant = "404", workbench;
const internal = [];
try {
  await c.api("/new", { url: `http://s1.localhost:${port}/` });
  workbench = await createWorkbench({ viewport: { width: 1280, height: 850 }, browserRequest: async (route, path, body) => {
    if (path === "/browser/resize") return route.fulfill({ json: { ok: true } });
    if (path === "/browser/data" || path === "/browser/data-settings") {
      if (variant === "404") return route.fulfill({ status: 404, body: "404 page not found" });
      if (variant === "update" && body?.mode === "capabilities") return route.fulfill({ json: { available: false, reason: "update" } });
      if (variant === "update") return route.fulfill({ status: 409, contentType: "text/plain; charset=utf-8", body: "浏览器组件不支持管理浏览数据，请更新环境组件" });
      return route.fulfill({ status: 409, contentType: "text/plain; charset=utf-8", body: "环境正在执行其他操作，请稍后再试" });
    }
    if (path === "/browser/internal-page") internal.push(body.mode);
    try { return route.fulfill({ json: await c.api(path.replace("/browser", ""), body ?? undefined) }); }
    catch (error) { return route.fulfill({ status: 400, contentType: "text/plain; charset=utf-8", body: error.message }); }
  } });
  const { page, errors } = workbench;
  page.setDefaultTimeout(15000);
  workbench.state.status.ready = workbench.state.status.vm_running = true;
  const menu = () => page.getByRole("button", { name: "浏览器菜单", exact: true }).first();
  const open = async () => {
    await page.reload();
    await page.getByRole("button", { name: "打开浏览器", exact: true }).click();
    await page.getByRole("tab", { name: "s1.localhost", exact: true }).click();
  };
  for (variant of ["404", "update"]) {
    await open();
    await menu().click();
    await page.getByRole("menuitem", { name: "管理 Cookie", exact: true }).click();
    const panel = page.getByRole("region", { name: "Cookie 管理" });
    await panel.getByText("srv", { exact: true }).waitFor();
    check(`${variant}: 管理 Cookie opens the earlier cookie panel`, true);
    await panel.getByRole("button", { name: "关闭 Cookie 管理" }).click();
    await page.getByRole("button", { name: "网站数据", exact: true }).click();
    await panel.waitFor();
    check(`${variant}: the site button opens the earlier cookie panel`, true);
    await panel.getByRole("button", { name: "关闭 Cookie 管理" }).click();
    const before = internal.length;
    await menu().click();
    await page.getByRole("menuitem", { name: "清除浏览数据…", exact: true }).click();
    for (let i = 0; i < 100 && internal.length === before; i++) await page.waitForTimeout(50);
    check(`${variant}: 清除浏览数据 opens Chromium's own page`, internal.at(-1) === "clear");
    await page.evaluate(() => document.dispatchEvent(new CustomEvent("cage-browser-data", { detail: { view: "privacy" } })));
    const privacy = page.getByRole("region", { name: "隐私与网站数据" });
    await privacy.getByRole("combobox", { name: "Cookie 浏览会话" }).waitFor();
    check(`${variant}: the privacy section keeps the earlier cookie manager`, await privacy.getByRole("region", { name: "Cookie 管理" }).count() === 1);
    if (variant === "update") check("update: the section says an update brings the new screens", await privacy.getByText(/更新环境组件后/).count() === 1);
  }
  variant = "busy";
  await open();
  const before = internal.length;
  await menu().click();
  await page.getByRole("menuitem", { name: "清除浏览数据…", exact: true }).click();
  const unavailable = page.getByRole("dialog", { name: "浏览数据暂时不可用" });
  await unavailable.getByText("环境正在执行其他操作，请稍后再试").waitFor();
  check("busy: the dialog shows the reason instead of switching screens", internal.length === before);
  variant = "404";
  await unavailable.getByRole("button", { name: "重试", exact: true }).click();
  await unavailable.waitFor({ state: "hidden" });
  for (let i = 0; i < 100 && internal.length === before; i++) await page.waitForTimeout(50);
  check("busy: 重试 asks again (here the server turned out old)", internal.at(-1) === "clear");
  variant = "busy";
  await open();
  await page.evaluate(() => document.dispatchEvent(new CustomEvent("cage-browser-data", { detail: { view: "privacy" } })));
  const privacy = page.getByRole("region", { name: "隐私与网站数据" });
  await privacy.getByText("环境正在执行其他操作，请稍后再试").waitFor();
  check("busy: the privacy section shows the reason with 重试", await privacy.getByRole("button", { name: "重试", exact: true }).count() === 1 && await privacy.getByRole("combobox", { name: "Cookie 浏览会话" }).count() === 0);
  check("no page errors", errors.length === 0);
  variant = "404";
  const chunk = /\/(assets\/browser-data-[A-Za-z0-9_-]{8}\.js|src\/browser-data\.jsx)(\?|$)/;
  let aborted = 0;
  await page.route(chunk, route => { aborted++; return route.abort(); });
  await open();
  const crashed = () => page.getByText("工作台显示遇到异常").count();
  const chunkBefore = internal.length;
  await menu().click();
  await page.getByRole("menuitem", { name: "清除浏览数据…", exact: true }).click();
  for (let i = 0; i < 100 && internal.length === chunkBefore; i++) await page.waitForTimeout(50);
  check("chunk: a failed load opens Chromium's clear page instead of the error screen", aborted > 0 && internal.at(-1) === "clear" && await crashed() === 0);
  await menu().click();
  await page.getByRole("menuitem", { name: "管理 Cookie", exact: true }).click();
  const cookiePanel = page.getByRole("region", { name: "Cookie 管理" });
  await cookiePanel.getByText("srv", { exact: true }).waitFor();
  check("chunk: 管理 Cookie falls back to the earlier cookie panel", await crashed() === 0);
  await cookiePanel.getByRole("button", { name: "关闭 Cookie 管理" }).click();
  await page.evaluate(() => document.dispatchEvent(new CustomEvent("cage-browser-data", { detail: { view: "privacy" } })));
  const chunkPrivacy = page.getByRole("region", { name: "隐私与网站数据" });
  await chunkPrivacy.getByRole("button", { name: "清除普通会话浏览数据", exact: true }).waitFor();
  check("chunk: the privacy section keeps Chromium's pages", await chunkPrivacy.getByRole("button", { name: "网站权限", exact: true }).count() === 1 && await crashed() === 0);
  await page.unroute(chunk);
  variant = "busy";
  await page.getByRole("tab", { name: "s1.localhost", exact: true }).click();
  await menu().click();
  await page.getByRole("menuitem", { name: "清除浏览数据…", exact: true }).click();
  await page.getByRole("dialog", { name: "浏览数据暂时不可用" }).waitFor();
  check("chunk: the next use loads the chunk again", true);
  results.passed = true;
  console.log(JSON.stringify(results, null, 1));
} finally {
  await workbench?.browser.close(); await c.close(); server.close();
}
