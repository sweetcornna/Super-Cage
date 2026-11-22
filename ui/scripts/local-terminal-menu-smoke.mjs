// "在本机终端中打开" in the desktop window, against the workbench fixture:
// the item in a project's new-terminal menu (the fixture's terminal view has
// no heading menu, and its terminal ids are not real ones), the
// Automation notice before the first open in Ghostty (asked again after a
// cancel, never after continuing, and never for the system Terminal, which
// opens at once), the application looked for at each open (so a Ghostty
// installed meanwhile is the one used, with its notice), the open request,
// the failure toast with 复制命令 (launch failures only), the pending state,
// the per-page viewer identity of terminal streams, and no item (no code
// loaded) outside the desktop app. Needs a Vite dev server (CAGE_UI_URL).
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createWorkbench } from "./workbench-fixture.mjs";

const output = new URL("../../output/local-terminal-menu/", import.meta.url);
await fs.mkdir(output, { recursive: true });
const project = "/Users/shihao/.claude-cage-workspace";
const connection = "g".repeat(64);
const apps = {
  ghostty: { id: "ghostty", name: "Ghostty", automation: true },
  terminal: { id: "terminal", name: "系统终端", automation: false },
};

async function run({ theme, viewport, desktop = true, start = "ghostty" }) {
  // The application the desktop finds now; a test changes it, as installing
  // Ghostty would.
  let found = apps[start];
  let detections = 0;
  const context = { mode: "local", connection_id: connection, desktop_gateway: desktop, client_platform: "darwin", server_platform: "darwin", connection: { state: "connected" } };
  const opens = [];
  const replies = [];
  let release = null;
  const loaded = [];
  const natives = [];
  const fixture = await createWorkbench({
    viewport, clientContext: context,
    // The desktop app writes the clipboard through its native bridge.
    nativeRequest: body => { natives.push(body); return null; },
    beforeNavigate: page => page.emulateMedia({ colorScheme: theme }),
    clientRequest: async (route, path, body) => {
      if (path === "/client/context") return route.fulfill({ json: context });
      if (path === "/client/local-terminal/app") { detections++; return route.fulfill({ json: { app: found } }); }
      if (path === "/client/local-terminal") return route.fulfill({ json: { settings: { image_upload: true, clipboard_write: true }, supported: true } });
      if (path === "/client/local-terminal/open") {
        opens.push({ body, connection: route.request().headers()["x-cage-connection"] });
        const reply = replies.shift() || { status: 200, json: { app: found.name } };
        if (reply.hold) await new Promise(resolve => { release = resolve; });
        return reply.json ? route.fulfill({ status: reply.status, json: reply.json }) : route.fulfill({ status: reply.status, contentType: "text/plain; charset=utf-8", body: reply.body });
      }
      return route.fulfill({ json: {} });
    },
  });
  const { page, browser, errors, state } = fixture;
  page.on("request", request => { if (request.url().includes("local-terminal-open")) loaded.push(request.url()); });
  // Socket URLs as the page opens them (routed sockets raise no event).
  await page.addInitScript(() => {
    const Native = window.WebSocket;
    window.__sockets = [];
    window.WebSocket = class extends Native {
      constructor(url, protocols) { super(url, protocols); window.__sockets.push(String(url)); }
    };
  });
  // A running environment, so its projects can have terminals.
  state.status.vm_running = true;
  state.status.ready = true;
  await page.reload();
  await page.locator('button[data-terminal-location="' + project + '"]:not([disabled])').waitFor();
  const shot = name => page.screenshot({ path: fileURLToPath(new URL(theme + "-" + viewport.width + "x" + viewport.height + "-" + start + "-" + name + ".png", output)) });
  const menu = () => page.getByRole("menu", { name: "新建终端" });
  const item = () => menu().getByRole("menuitem", { name: /在本机终端中打开/ });
  const openProjectMenu = async () => {
    await page.locator('button[data-terminal-location="' + project + '"]').click();
    await menu().waitFor();
  };
  try {
    await openProjectMenu();
    if (!desktop) {
      assert.equal(await item().count(), 0, "only the desktop app offers the item");
      assert.equal(loaded.length, 0, "its code is not loaded outside the desktop app");
      console.log("PASS: no 在本机终端中打开 outside the desktop app");
      return;
    }
    assert.ok(loaded.length > 0, "the opener loads apart from the first chunk");
    await item().waitFor();
    assert.equal(await item().getAttribute("title"), null, "there is no application to name beside the label");
    assert.equal(detections, 0, "the application is looked for when an open starts, not before");
    await shot("menu");
    const toast = page.locator(".toast");
    const click = async () => {
      await openProjectMenu();
      await item().click();
    };
    const opened = async count => {
      for (let i = 0; i < 50 && opens.length < count; i++) await page.waitForTimeout(50);
      assert.equal(opens.length, count);
    };
    // A launch that fails says why, with 复制命令 and no advice to pick another application.
    const failure = async (name, reason, first = false) => {
      replies.push({ status: 502, body: "无法在 " + name + " 中打开：" + reason });
      await click();
      await toast.getByText("没能打开 " + name + "：" + reason + "。").waitFor();
      assert.equal(await page.getByRole("dialog").count(), 0, "the notice is shown once");
      assert.doesNotMatch(await toast.textContent(), /无法在 .* 中打开|换一个|本机终端里/);
      if (first) await shot("failure-toast");
      await toast.getByRole("button", { name: "复制命令" }).click();
      await page.locator(".toast").getByText("已复制命令").waitFor();
      assert.equal(natives.filter(body => body.action === "clipboardWrite").at(-1).text, "cage attach --new shell --project " + project + " current");
    };
    // Ghostty is the application macOS asks about: the notice before it opens.
    const notice = async ({ cancel = false, first = false } = {}) => {
      const dialog = page.getByRole("dialog");
      await dialog.getByText("Super Cage 需要控制 Ghostty 来打开窗口").waitFor();
      assert.match(await dialog.textContent(), /如果不允许，窗口不会打开/);
      assert.doesNotMatch(await dialog.textContent(), /换一个|选择/);
      const sent = opens.length;
      await page.waitForTimeout(400); // the dialog's entrance
      if (first) await shot("automation-notice");
      await dialog.getByRole("button", { name: cancel ? "取消" : "继续" }).click();
      await dialog.waitFor({ state: "detached" });
      if (cancel) assert.equal(opens.length, sent, "nothing is sent when the owner cancels");
    };
    if (start === "ghostty") {
      await item().click();
      assert.equal(opens.length, 0, "nothing is sent before the owner continues");
      await notice({ cancel: true, first: true });
      // Cancelled: the notice comes again at the next open.
      await click();
      await notice();
      await opened(1);
      assert.deepEqual(opens[0], { body: { project, kind: "shell" }, connection });
      assert.equal(detections, 2);
      await failure("Ghostty", "macOS 没有允许 Super Cage 控制这个应用：在“系统设置 › 隐私与安全性 › 自动化”里打开后再试", true);
    } else {
      // The system Terminal needs no permission: the first open goes out at once.
      await item().click();
      await opened(1);
      assert.equal(await page.getByRole("dialog").count(), 0, "the system Terminal needs no notice");
      assert.deepEqual(opens[0], { body: { project, kind: "shell" }, connection });
      await failure("系统终端", "没有这个文件", true);
      // Ghostty gets installed meanwhile: the next open uses it, with its notice.
      found = apps.ghostty;
      await click();
      await notice({ first: true });
      await opened(3);
      assert.equal(detections, 3);
      await shot("after-installing-ghostty");
    }
    // Locked: the server's sentence, no 复制命令 and no advice.
    replies.push({ status: 423, body: "Super Cage 已锁定：解锁后再打开本机终端" });
    await click();
    await page.locator(".toast").getByText("Super Cage 已锁定：解锁后再打开本机终端").waitFor();
    assert.equal(await page.locator(".toast").getByRole("button", { name: "复制命令" }).count(), 0);
    // While an open waits (macOS asking the owner), the item is disabled.
    replies.push({ status: 200, json: { app: found.name }, hold: true });
    await click();
    for (let i = 0; i < 50 && !release; i++) await page.waitForTimeout(50);
    await openProjectMenu();
    assert.ok(await item().isDisabled(), "a second open is not offered while one waits");
    assert.equal(await item().getAttribute("title"), "正在打开…");
    await shot("pending");
    release();
    await page.keyboard.press("Escape");
    await page.waitForFunction(() => true);
    // A terminal: its stream carries this page's viewer identity.
    await openProjectMenu();
    await menu().getByRole("menuitem", { name: "普通终端" }).click();
    await page.getByText("终端 1", { exact: true }).first().click();
    await page.locator(".xterm").first().waitFor();
    // The page names itself to the terminal stream, the same on every reconnect.
    const sockets = () => page.evaluate(() => window.__sockets.filter(url => url.includes("/terminals/")));
    for (let i = 0; i < 50 && !(await sockets()).length; i++) await page.waitForTimeout(50);
    const streams = await sockets();
    assert.ok(streams.length > 0 && streams.every(url => /[?&]viewer=web-[0-9a-f]{24}(&|$)/.test(url)), JSON.stringify(streams));
    assert.deepEqual(errors, []);
    console.log("PASS: " + theme + " " + viewport.width + "x" + viewport.height + " " + start + " menu item, notice, request, failure toast, lock, pending, viewer");
  } finally { await browser.close(); }
}

// Both themes at both sizes, each application once.
await run({ theme: "light", viewport: { width: 1440, height: 900 }, start: "ghostty" });
await run({ theme: "dark", viewport: { width: 1000, height: 700 }, start: "ghostty" });
await run({ theme: "dark", viewport: { width: 1440, height: 900 }, start: "terminal" });
await run({ theme: "light", viewport: { width: 1000, height: 700 }, start: "terminal" });
await run({ theme: "light", viewport: { width: 1000, height: 700 }, desktop: false });
