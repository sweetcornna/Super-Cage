// Behaviour smoke for the web on phones: what the device matrix (mobile-web-matrix.mjs) does not walk.
// Real builds behind the portal mock with the phone fixtures, one phone size, Chromium or WebKit
// (CAGE_TEST_BROWSER=webkit; CAGE_PLAYWRIGHT_MODULE as for the other smokes).
//   - which document "/" serves: a phone gets the phone UI; an iPad in Split View is handed over by the
//     desktop page; a narrow desktop window (fine pointer) keeps the desktop workbench;
//   - in the portal: Back walks sheet -> screen -> VM list; "返回门户" goes straight to the list;
//     an expired workspace sign-in (HTTP 401) re-enters the VM on the same screen; links open in a new tab;
//   - "电脑版" and "手机版" round trip in the portal frame, kept across a reload, with the portal's bars
//     coming back for the desktop workbench.
// Build first: npm --prefix ui run build.
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { startMockServer } from "../tenant/mock-server.mjs";

const require = createRequire(import.meta.url);
const engines = require(process.env.CAGE_PLAYWRIGHT_MODULE || "playwright");
const engine = process.env.CAGE_TEST_BROWSER || "chromium";
const workbenchDir = process.env.CAGE_WORKBENCH_DIR || fileURLToPath(new URL("../../internal/workbench/web/", import.meta.url));
const iPhone = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1";
const iPad = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15";
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const assert = (value, message) => { if (!value) throw new Error(message); };

const mock = await startMockServer({ workbenchDir, bootDelayMs: 200, phoneFixture: true, globalLimit: 100000 });
const browser = await engines[engine].launch(engine === "chromium" && process.env.CAGE_TEST_BROWSER_PATH ? { executablePath: process.env.CAGE_TEST_BROWSER_PATH } : {});
const passed = [];

let users = 0;
async function account(page) {
  const post = (path, data) => page.request.post(mock.origin + path, { data, headers: { Origin: mock.origin } });
  await post("/api/tenant/register", { username: `smoke${++users}`, password: "fixture password only" });
  return post;
}
async function workspaceURL(post, name = "开发环境") {
  const { vm } = await (await post("/api/tenant/vms", { name })).json();
  for (let i = 0; i < 300; i++) {
    const open = await post(`/api/tenant/vms/${vm.id}/open`, {});
    if (open.status() === 200) return (await open.json()).workspace_url;
    await sleep(50);
  }
  throw new Error("the VM did not open");
}
function context(options) {
  const agent = options.userAgent || iPhone;
  return browser.newContext({
    viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: engine === "chromium" ? true : undefined,
    // Chromium's mobile emulation claims Sec-CH-UA-Mobile: ?1 whatever the agent; Safari sends no hints.
    extraHTTPHeaders: engine === "chromium" && agent === iPad ? { "sec-ch-ua-mobile": "?0" } : undefined,
    ...options, userAgent: agent,
  });
}
// The system Back (a same-document traversal inside the frame is not a navigation Playwright waits for).
const back = (page) => page.evaluate(() => history.back());
const isPhoneDocument = (target) => target.evaluate(() => !!document.querySelector('meta[name="cage-entry"][content="phone"]'));

async function step(name, run) {
  await run();
  passed.push(name);
}

try {
  // ---- which document "/" serves ----------------------------------------------------------------------
  await step("a phone opening a workspace gets the phone UI", async () => {
    const ctx = await context({});
    const page = await ctx.newPage();
    const post = await account(page);
    await page.goto(await workspaceURL(post));
    await page.waitForURL((url) => url.pathname === "/");
    await page.locator(".mobile-app .greeting").waitFor();
    assert(await isPhoneDocument(page), "expected the phone document");
    await ctx.close();
  });
  await step("an iPad in Split View is handed to the phone UI once, and keeps it", async () => {
    const ctx = await context({ userAgent: iPad, viewport: { width: 507, height: 1024 } });
    const page = await ctx.newPage();
    const post = await account(page);
    await page.goto(await workspaceURL(post));
    await page.waitForURL((url) => url.pathname === "/");
    await page.locator(".mobile-app .greeting").waitFor();
    const cookies = await ctx.cookies();
    assert(cookies.some((c) => c.name === "cage-layout" && c.value === "phone-auto" && c.expires === -1), "expected a session cookie phone-auto");
    // Full width again (Split View closed): the phone page drops the automatic choice; the next load is desktop.
    await page.setViewportSize({ width: 1024, height: 1366 });
    await page.reload();
    await page.locator(".mobile-app, .workbench-shell, .client-loading").first().waitFor();
    await page.reload();
    await page.waitForLoadState();
    await sleep(800);
    assert(!(await isPhoneDocument(page)), "a full-width iPad should get the desktop workbench again");
    await ctx.close();
  });
  await step("a narrow desktop window keeps the desktop workbench", async () => {
    const ctx = await browser.newContext({ viewport: { width: 480, height: 900 }, userAgent: iPad, hasTouch: false });
    const page = await ctx.newPage();
    const post = await account(page);
    await page.goto(await workspaceURL(post));
    await page.waitForURL((url) => url.pathname === "/");
    await page.waitForLoadState();
    await sleep(1500);
    assert(!(await isPhoneDocument(page)), "a fine pointer must not be sent to the phone UI");
    assert(!(await page.locator(".layout-switch").isVisible()), "no 手机版 switch with a fine pointer");
    await ctx.close();
  });

  // ---- the portal --------------------------------------------------------------------------------------
  // WebKit keeps no cookie for the VM's *.localhost name inside the portal's frame, so the framed workspace
  // answers 401 there (the mock cannot serve one registrable domain over HTTPS as production does). The
  // portal steps run on Chromium; on iOS they are on the real-device checklist.
  if (engine === "webkit") {
    console.log("skip portal steps on WebKit (no frame cookie on *.localhost)");
    throw Object.assign(new Error("done"), { done: true });
  }
  const ctx = await context({});
  await ctx.route("https://example.com/**", (route) => route.fulfill({ status: 200, contentType: "text/html", body: "<title>外部页面</title>ok" }));
  const page = await ctx.newPage();
  page.setDefaultTimeout(15000);
  const post = await account(page);
  await post("/api/tenant/vms", { name: "开发环境" });
  const phoneUI = () => page.waitForFunction(() => document.querySelector(".shell")?.dataset.phoneUi === "true");
  const frame = async () => {
    await page.waitForFunction(() => document.querySelector("iframe.workspace-frame"));
    for (let i = 0; i < 100; i++) {
      const found = page.frames().find((f) => f !== page.mainFrame() && f.url().startsWith("http://vm-"));
      if (found) return found;
      await sleep(100);
    }
    throw new Error("no workspace frame");
  };
  const list = async () => {
    await page.locator(".vm-card").first().waitFor();
    await page.waitForFunction(() => !document.querySelector("iframe.workspace-frame") && !document.querySelector(".shell")?.dataset.phoneUi);
  };
  const enter = async () => {
    await page.locator(".vm-card").first().click();
    await phoneUI();
    const inner = await frame();
    await inner.locator(".mobile-app").waitFor();
    return inner;
  };
  const tool = async (inner, name) => {
    await inner.getByRole("button", { name: "展开侧栏", exact: true }).click();
    await inner.getByRole("dialog", { name: "工作区侧栏" }).getByRole("button", { name, exact: true }).click();
  };
  await page.goto(mock.origin);

  await step("portal: Back closes the sheet, then the screen, then returns to the VM list", async () => {
    const inner = await enter();
    await tool(inner, "文件");
    await inner.getByRole("button", { name: /^README\.md/ }).click();
    await inner.getByText("手机网页版测试项目").waitFor();
    await back(page);
    await inner.getByText("手机网页版测试项目").waitFor({ state: "hidden" });
    assert(await inner.getByRole("button", { name: /^README\.md/ }).isVisible(), "Back left the files screen");
    await back(page);
    await inner.locator(".greeting").waitFor();
    await back(page);
    await list();
  });
  await step("portal: 返回门户 goes straight to the VM list and leaves no extra Back step", async () => {
    const inner = await enter();
    await tool(inner, "文件");
    await inner.getByRole("button", { name: "展开侧栏", exact: true }).click();
    await inner.getByRole("button", { name: "返回门户", exact: true }).click();
    await list();
    await sleep(300);
    assert(!(await page.evaluate(() => history.state?.cagePortal)), "the portal's history entry is still there");
  });
  await step("portal: links in a reply open in a new tab without an opener", async () => {
    const inner = await enter();
    await inner.getByRole("button", { name: "展开侧栏", exact: true }).click();
    await inner.getByRole("dialog", { name: "工作区侧栏" }).getByRole("button", { name: "整理发布说明" }).first().click();
    const [popup] = await Promise.all([ctx.waitForEvent("page"), inner.getByRole("link", { name: "发布说明" }).click()]);
    await popup.waitForURL("https://example.com/**");
    assert(await popup.evaluate(() => window.opener === null), "the new tab has an opener");
    await popup.close();
  });
  await step("portal: an expired workspace sign-in re-enters the VM on the same screen", async () => {
    const inner = await frame();
    await tool(inner, "文件");
    await inner.getByRole("button", { name: /^README\.md/ }).waitFor();
    let refused = 0;
    await ctx.route("**/api/mobile/files/list", (route) => (refused++ ? route.continue() : route.fulfill({ status: 401, body: "unauthenticated" })));
    await inner.getByRole("button", { name: /^docs/ }).click();
    await page.waitForFunction(() => document.querySelector("iframe.workspace-frame"));
    for (let i = 0; i < 100 && refused < 2; i++) await sleep(100);
    await ctx.unroute("**/api/mobile/files/list");
    assert(refused >= 2, "the workspace was not entered again after the 401");
    const again = await frame();
    await again.locator(".mobile-app").waitFor();
    await again.getByText("文件", { exact: true }).first().waitFor();
    await phoneUI();
  });
  await step("portal: 电脑版 and 手机版 round trip, kept across a reload", async () => {
    let inner = await frame();
    await inner.getByRole("button", { name: "展开侧栏", exact: true }).click();
    await inner.getByRole("button", { name: "电脑版", exact: true }).click();
    await page.waitForFunction(() => !document.querySelector(".shell")?.dataset.phoneUi);
    assert(await page.locator(".topbar").isVisible(), "the portal bars did not come back for the desktop workbench");
    inner = await frame();
    await inner.locator(".layout-switch").waitFor();
    assert(!(await isPhoneDocument(inner)), "expected the desktop document in the frame");
    await page.reload();
    await page.waitForLoadState();
    inner = await frame();
    await inner.locator(".layout-switch").waitFor();
    await inner.locator(".layout-switch").click();
    await phoneUI();
    inner = await frame();
    await inner.locator(".mobile-app").waitFor();
  });
  await ctx.close();
} catch (error) {
  if (!error.done) throw error;
} finally {
  await browser.close();
  await mock.close();
}
for (const name of passed) console.log("ok  " + name);
console.log(`${engine}: ${passed.length} mobile web behaviours passed`);
