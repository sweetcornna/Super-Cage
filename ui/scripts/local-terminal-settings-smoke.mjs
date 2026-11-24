// Settings → 本机终端 › 在本机终端中打开 in the real settings page, against
// the isolated workbench fixture: lazy loading, the application looked for
// only while the section is open, the one line that says which application
// opens (Ghostty, or the system Terminal until Ghostty is installed) with no
// list or picker, the two switches, the unsaved-change guard, saving exactly
// the two switches, both parts in one section, and screenshots in both
// themes and both window sizes. Needs a Vite dev server (CAGE_UI_URL).
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createWorkbench } from "./workbench-fixture.mjs";

const output = new URL("../../output/local-terminal-settings/", import.meta.url);
await fs.mkdir(output, { recursive: true });
const apps = {
  herdr: { id: "herdr", name: "Herdr", automation: false },
  ghostty: { id: "ghostty", name: "Ghostty", automation: true },
  terminal: { id: "terminal", name: "系统终端", automation: false },
};
const lines = {
  herdr: "用 Herdr 打开（已自动替代 Ghostty 接管本机终端）",
  ghostty: "用 Ghostty 打开；安装 Herdr 后自动改用 Herdr",
  terminal: "用系统终端打开；安装 Herdr 或 Ghostty 后自动切换",
};
// What the page must never offer or name any more.
const gone = /iTerm2|WezTerm|kitty|Alacritty|Warp|内置终端|选择终端应用|换一个/;

async function run({ theme, viewport, desktop = true, app = "ghostty", supported = true, line = lines[app] }) {
  const context = { mode: "local", connection_id: "g".repeat(64), desktop_gateway: desktop, client_platform: "darwin", server_platform: "darwin", connection: { state: "connected" } };
  // A route that still sends the application an earlier build kept (the page
  // must not show it, nor send it back).
  let settings = { app: "kitty", image_upload: true, clipboard_write: true };
  const puts = [];
  let detections = 0;
  const fixture = await createWorkbench({
    viewport, clientContext: context,
    beforeNavigate: page => page.emulateMedia({ colorScheme: theme }),
    clientRequest: async (route, path, body) => {
      if (path === "/client/context") return route.fulfill({ json: context });
      if (path === "/client/local-terminal/app") {
        detections++;
        return route.fulfill({ json: { app: app ? apps[app] : null } });
      }
      if (path === "/client/local-terminal/install-herdr" && route.request().method() === "POST") {
        return route.fulfill({ json: { app: apps.herdr, installed: true } });
      }
      if (path === "/client/local-terminal") {
        if (route.request().method() === "PUT") { puts.push(body); settings = body; }
        return route.fulfill({ json: { settings, supported } });
      }
      return route.fulfill({ json: {} });
    },
  });
  const { page, browser, errors } = fixture;
  const chunk = [];
  page.on("request", request => { if (request.url().includes("local-terminal-settings")) chunk.push(request.url()); });
  const name = viewport.width + "x" + viewport.height;
  const shot = suffix => page.screenshot({ path: fileURLToPath(new URL(theme + "-" + name + "-" + suffix + ".png", output)) });
  try {
    await page.getByRole("button", { name: "设置", exact: true }).click();
    const nav = page.getByRole("navigation", { name: "设置分组" });
    await nav.getByRole("button", { name: "通用", exact: true }).waitFor();
    if (!desktop) {
      // Outside the desktop app the section holds the ssh part only.
      await nav.getByRole("button", { name: "本机终端", exact: true }).click();
      await page.getByRole("region", { name: "从你自己的终端连进来", exact: true }).waitFor();
      assert.equal(await page.getByRole("region", { name: "在本机终端中打开", exact: true }).count(), 0, "only the desktop app opens windows");
      assert.equal(detections, 0);
      assert.equal(await page.locator(".settings-content").getByText(gone).count(), 0);
      assert.deepEqual(errors, []);
      console.log("PASS: outside the desktop app 本机终端 has the ssh part only");
      return;
    }
    assert.equal(chunk.length, 0, "the section's code must load only when it opens");
    assert.equal(detections, 0, "the application must not be looked for before the section opens");
    assert.equal(await nav.getByRole("button", { name: "本机终端应用", exact: true }).count(), 0, "one 本机终端 section, not two");
    await nav.getByRole("button", { name: "本机终端", exact: true }).click();
    const section = page.getByRole("region", { name: "在本机终端中打开", exact: true });
    await section.getByText(line, { exact: true }).waitFor();
    await page.getByRole("region", { name: "从你自己的终端连进来", exact: true }).waitFor();
    assert.ok(chunk.length > 0);
    assert.equal(detections, 1);
    // One line, no list, no picker, nothing to choose from.
    assert.equal(await section.getByRole("radio").count(), 0);
    assert.equal(await section.getByRole("radiogroup").count(), 0);
    assert.equal(await section.locator(".choice-list, .local-terminal-chip").count(), 0);
    assert.equal(await page.locator(".settings-content").getByText(gone).count(), 0, "no other application or chooser is named");
    assert.equal(await section.locator(".local-terminal-opener").count(), 1);
    const installBtn = section.getByRole("button", { name: "安装 Herdr 桌面端（安装后自动改用 Herdr）", exact: true });
    assert.equal(await installBtn.count(), supported && app !== "herdr" ? 1 : 0, "optional Herdr install button visibility");
    assert.equal(await section.getByText(/macOS 会询问/).count(), 0, "the macOS question is the notice's, not the page's");
    const switches = section.getByRole("switch");
    assert.equal(await switches.count(), 2);
    assert.ok(await section.getByRole("switch", { name: /上传拖入的图片/ }).isChecked());
    assert.ok(await section.getByRole("switch", { name: /允许环境写入剪贴板/ }).isChecked());
    await shot(!supported ? "unsupported" : app ? app : "none");
    if (!supported || !app) {
      // Nothing can open: the line says so, and the switches (which ssh sessions follow too) stay.
      assert.deepEqual(errors, []);
      console.log("PASS: " + theme + " " + name + " " + (supported ? "no application" : "unsupported system") + " line");
      return;
    }
    // Change, then try to leave: the page asks first.
    await section.getByRole("switch", { name: /允许环境写入剪贴板/ }).click();
    await section.getByText("有尚未保存的修改").waitFor();
    // The save bar is in view with the page scrolled to the top: on the
    // window's bottom edge when the section reaches past it, else at the
    // section's end.
    await page.evaluate(() => document.querySelector(".settings-content").scrollTo(0, 0));
    await page.waitForTimeout(400); // the bar's entrance animation
    const bar = await section.locator(".local-terminal-savebar").boundingBox();
    const scroller = await page.locator(".settings-content").boundingBox();
    const box = await section.boundingBox();
    assert.ok(bar && bar.y >= scroller.y && bar.y + bar.height <= scroller.y + scroller.height + 1, "the save bar is in view: " + JSON.stringify({ bar, scroller }));
    if (box.y + box.height > scroller.y + scroller.height + 1) {
      assert.ok(Math.abs(bar.y + bar.height - (scroller.y + scroller.height)) <= 1, "the save bar sits on the bottom edge: " + JSON.stringify({ bar, scroller }));
    } else {
      assert.ok(bar.y + bar.height <= box.y + box.height + 1, "the save bar ends with its section: " + JSON.stringify({ bar, box }));
    }
    await shot(app + "-dirty");
    await nav.getByRole("button", { name: "外观", exact: true }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByText("放弃尚未保存的本机终端设置？").waitFor();
    await dialog.getByRole("button").filter({ hasNotText: "放弃修改" }).first().click();
    await dialog.waitFor({ state: "detached" });
    assert.equal(await section.getByRole("switch", { name: /允许环境写入剪贴板/ }).isChecked(), false, "cancelling keeps the draft");
    await section.getByRole("button", { name: "保存", exact: true }).click();
    await section.getByText("已保存", { exact: true }).waitFor();
    // Exactly the two switches: a stored application name is never sent back.
    assert.deepEqual(puts, [{ image_upload: true, clipboard_write: false }]);
    assert.equal(await section.getByText("有尚未保存的修改").count(), 0);
    // Saved: leaving asks nothing, and nothing looks for the application again.
    await nav.getByRole("button", { name: "外观", exact: true }).click();
    assert.equal(await page.getByRole("dialog").count(), 0);
    assert.equal(detections, 1);
    assert.equal(await page.evaluate(() => document.querySelector(".settings-content").scrollWidth > document.querySelector(".settings-content").clientWidth), false);
    assert.deepEqual(errors, []);
    console.log("PASS: " + theme + " " + name + " " + app + " lazy section, looked for on open, one line, two switches, both parts, guard, save");
  } finally { await browser.close(); }
}

for (const theme of ["light", "dark"]) {
  for (const viewport of [{ width: 1440, height: 900 }, { width: 1000, height: 700 }]) {
    await run({ theme, viewport, app: "herdr" });
    await run({ theme, viewport, app: "ghostty" });
    await run({ theme, viewport, app: "terminal" });
  }
}
await run({ theme: "light", viewport: { width: 1000, height: 700 }, app: null, line: "这台电脑上找不到 Herdr、Ghostty 或系统终端，暂时不能在本机终端中打开" });
await run({ theme: "light", viewport: { width: 1000, height: 700 }, supported: false, line: "这台电脑的系统暂不支持在本机终端中打开，终端会在 Super Cage 的标签页中打开" });
await run({ theme: "light", viewport: { width: 1000, height: 700 }, desktop: false });
