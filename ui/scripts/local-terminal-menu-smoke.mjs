// "在本机终端中打开" in the desktop window, against the workbench fixture:
// the item in a project's new-terminal menu (the fixture's terminal view has
// no heading menu, and its terminal ids are not real ones), the
// Automation notice before the first open of iTerm2 (and only then), the
// open request, the failure toast with 复制命令 (launch failures only), the
// pending state, the per-page viewer identity of terminal streams, and no
// item (no code loaded) outside the desktop app. Needs a Vite dev server
// (CAGE_UI_URL).
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createWorkbench } from "./workbench-fixture.mjs";

const output = new URL("../../output/local-terminal-menu/", import.meta.url);
await fs.mkdir(output, { recursive: true });
const project = "/Users/shihao/.claude-cage-workspace";
const connection = "g".repeat(64);
const apps = [{ id: "iterm2", name: "iTerm2", automation: true, hosts: true, installed: true }, { id: "kitty", name: "kitty", automation: false, hosts: true, installed: true }];

async function run({ theme, viewport, desktop = true }) {
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
      if (path === "/client/local-terminal/apps") return route.fulfill({ json: { apps } });
      if (path === "/client/local-terminal") return route.fulfill({ json: { settings: { app: "iterm2", image_upload: true, clipboard_write: true }, supported: true } });
      if (path === "/client/local-terminal/open") {
        opens.push({ body, connection: route.request().headers()["x-cage-connection"] });
        const reply = replies.shift() || { status: 200, json: { app: "iTerm2", automation: true } };
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
  const shot = name => page.screenshot({ path: fileURLToPath(new URL(theme + "-" + viewport.width + "x" + viewport.height + "-" + name + ".png", output)) });
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
    assert.equal(await item().getAttribute("title"), "iTerm2");
    await shot("menu");
    // First open of an application macOS asks about: the notice first.
    await item().click();
    const dialog = page.getByRole("dialog");
    await dialog.getByText("Super Cage 需要控制 iTerm2 来打开窗口").waitFor();
    assert.match(await dialog.textContent(), /如果不允许，窗口不会打开/);
    assert.equal(opens.length, 0, "nothing is sent before the owner continues");
    await page.waitForTimeout(400); // the dialog's entrance
    await shot("automation-notice");
    await dialog.getByRole("button", { name: "继续" }).click();
    await page.waitForFunction(() => true);
    for (let i = 0; i < 50 && opens.length < 1; i++) await page.waitForTimeout(50);
    assert.deepEqual(opens[0], { body: { project, kind: "shell" }, connection });
    // Later opens: no notice. The application fails: the toast says why,
    // with 复制命令.
    replies.push({ status: 502, body: "无法在 iTerm2 中打开：macOS 没有允许 Super Cage 控制这个应用：在“系统设置 › 隐私与安全性 › 自动化”里打开后再试" });
    await openProjectMenu();
    await item().click();
    const toast = page.locator(".toast");
    await toast.getByText(/没能打开 iTerm2：macOS 没有允许/).waitFor();
    assert.equal(await page.getByRole("dialog").count(), 0, "the notice is shown once");
    assert.doesNotMatch(await toast.textContent(), /无法在 iTerm2 中打开/);
    await shot("failure-toast");
    await toast.getByRole("button", { name: "复制命令" }).click();
    await page.locator(".toast").getByText("已复制命令").waitFor();
    assert.deepEqual(natives.filter(body => body.action === "clipboardWrite").map(body => body.text), ["cage attach --new shell --project " + project + " current"]);
    // Locked: the server's sentence, no 复制命令 and no advice to switch apps.
    replies.push({ status: 423, body: "Super Cage 已锁定：解锁后再打开本机终端" });
    await openProjectMenu();
    await item().click();
    await page.locator(".toast").getByText("Super Cage 已锁定：解锁后再打开本机终端").waitFor();
    assert.equal(await page.locator(".toast").getByRole("button", { name: "复制命令" }).count(), 0);
    // While an open waits (macOS asking the owner), the item is disabled.
    replies.push({ status: 200, json: { app: "iTerm2", automation: true }, hold: true });
    await openProjectMenu();
    await item().click();
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
    const opened = () => page.evaluate(() => window.__sockets.filter(url => url.includes("/terminals/")));
    for (let i = 0; i < 50 && !(await opened()).length; i++) await page.waitForTimeout(50);
    const streams = await opened();
    assert.ok(streams.length > 0 && streams.every(url => /[?&]viewer=web-[0-9a-f]{24}(&|$)/.test(url)), JSON.stringify(streams));
    assert.deepEqual(errors, []);
    console.log("PASS: " + theme + " " + viewport.width + "x" + viewport.height + " menu item, notice once, request, failure toast, lock, pending, viewer");
  } finally { await browser.close(); }
}

await run({ theme: "light", viewport: { width: 1440, height: 900 } });
await run({ theme: "dark", viewport: { width: 1000, height: 700 } });
await run({ theme: "light", viewport: { width: 1000, height: 700 }, desktop: false });
