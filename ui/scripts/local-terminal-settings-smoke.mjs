// Settings → 本机终端 › 用哪个终端应用打开 in the real settings page, against
// the isolated workbench fixture: lazy loading, detection only while the
// section is open, the chooser order, the macOS question said once, the
// unsaved-change guard, saving, both parts in one section, and screenshots
// in both themes and both window sizes. Needs a Vite dev server
// (CAGE_UI_URL).
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createWorkbench } from "./workbench-fixture.mjs";

const output = new URL("../../output/local-terminal-settings/", import.meta.url);
await fs.mkdir(output, { recursive: true });
const table = [
  ["ghostty", "Ghostty", true, true], ["iterm2", "iTerm2", true, true], ["terminal", "终端", false, true],
  ["warp", "Warp", false, true], ["wezterm", "WezTerm", false, true], ["kitty", "kitty", false, true],
  ["alacritty", "Alacritty", false, true], ["herdr", "Herdr", false, false],
];
const installed = new Set(["ghostty", "iterm2", "terminal", "kitty", "herdr"]);

async function run({ theme, viewport, desktop = true }) {
  const context = { mode: "local", connection_id: "g".repeat(64), desktop_gateway: desktop, client_platform: "darwin", server_platform: "darwin", connection: { state: "connected" } };
  let settings = { app: "ghostty", image_upload: true, clipboard_write: true };
  const puts = [];
  let detections = 0;
  const fixture = await createWorkbench({
    viewport, clientContext: context,
    beforeNavigate: page => page.emulateMedia({ colorScheme: theme }),
    clientRequest: async (route, path, body) => {
      if (path === "/client/context") return route.fulfill({ json: context });
      if (path === "/client/local-terminal/apps") {
        detections++;
        return route.fulfill({ json: { apps: table.map(([id, name, automation, hosts]) => ({ id, name, automation, hosts, installed: installed.has(id), ...(hosts ? {} : { note: "Herdr 通过本机终端的 SSH 入口连接，稍后的版本提供" }) })) } });
      }
      if (path === "/client/local-terminal") {
        if (route.request().method() === "PUT") { puts.push(body); settings = body; }
        return route.fulfill({ json: { settings, supported: true } });
      }
      return route.fulfill({ json: {} });
    },
  });
  const { page, browser, errors } = fixture;
  const chunk = [];
  page.on("request", request => { if (request.url().includes("local-terminal-settings")) chunk.push(request.url()); });
  try {
    await page.getByRole("button", { name: "设置", exact: true }).click();
    const nav = page.getByRole("navigation", { name: "设置分组" });
    await nav.getByRole("button", { name: "通用", exact: true }).waitFor();
    if (!desktop) {
      // Outside the desktop app the section holds the ssh part only.
      await nav.getByRole("button", { name: "本机终端", exact: true }).click();
      await page.getByRole("region", { name: "从你自己的终端或 Herdr 连进来", exact: true }).waitFor();
      assert.equal(await page.getByRole("region", { name: "用哪个终端应用打开", exact: true }).count(), 0, "only the desktop app chooses the application");
      assert.equal(detections, 0);
      assert.deepEqual(errors, []);
      console.log("PASS: outside the desktop app 本机终端 has no application chooser");
      return;
    }
    assert.equal(chunk.length, 0, "the section's code must load only when it opens");
    assert.equal(detections, 0, "detection must not run before the section opens");
    assert.equal(await nav.getByRole("button", { name: "本机终端应用", exact: true }).count(), 0, "one 本机终端 section, not two");
    await nav.getByRole("button", { name: "本机终端", exact: true }).click();
    const section = page.getByRole("region", { name: "用哪个终端应用打开", exact: true });
    const group = section.getByRole("radiogroup", { name: "本机终端应用" });
    await group.waitFor();
    await page.getByRole("region", { name: "从你自己的终端或 Herdr 连进来", exact: true }).waitFor();
    // The macOS question is said once, not on each application.
    assert.equal(await page.getByText(/macOS 会询问/).count(), 1);
    await section.getByText("第一次用 Ghostty、iTerm2 打开时，macOS 会询问是否允许 Super Cage 控制它。", { exact: true }).waitFor();
    assert.ok(chunk.length > 0);
    assert.equal(detections, 1);
    const names = await group.locator(".local-terminal-app strong").allTextContents();
    assert.deepEqual(names, ["Ghostty", "iTerm2", "kitty", "终端", "Super Cage 内置终端", "Herdr", "Alacritty", "Warp", "WezTerm"]);
    assert.ok(await group.getByRole("radio", { name: /Ghostty/ }).isChecked());
    assert.ok(await group.getByRole("radio", { name: /WezTerm/ }).isDisabled());
    await page.screenshot({ path: fileURLToPath(new URL(theme + "-" + viewport.width + "x" + viewport.height + ".png", output)) });
    // Change, then try to leave: the page asks first.
    await group.getByText("kitty", { exact: true }).click();
    await section.getByRole("switch", { name: /允许环境写入剪贴板/ }).click();
    await section.getByText("有尚未保存的修改").waitFor();
    // The save bar stays in view at the bottom of the page while the list is scrolled to the top.
    await page.evaluate(() => document.querySelector(".settings-content").scrollTo(0, 0));
    await page.waitForTimeout(400); // the bar's entrance animation
    const bar = await section.locator(".local-terminal-savebar").boundingBox();
    const scroller = await page.locator(".settings-content").boundingBox();
    assert.ok(bar && Math.abs(bar.y + bar.height - (scroller.y + scroller.height)) <= 1, "the save bar sits on the bottom edge: " + JSON.stringify({ bar, scroller }));
    await page.screenshot({ path: fileURLToPath(new URL(theme + "-" + viewport.width + "x" + viewport.height + "-dirty.png", output)) });
    await nav.getByRole("button", { name: "外观", exact: true }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByText("放弃尚未保存的本机终端设置？").waitFor();
    await dialog.getByRole("button").filter({ hasNotText: "放弃修改" }).first().click();
    await dialog.waitFor({ state: "detached" });
    assert.ok(await group.getByRole("radio", { name: /kitty/ }).isChecked(), "cancelling keeps the draft");
    await section.getByRole("button", { name: "保存", exact: true }).click();
    await section.getByText("已保存", { exact: true }).waitFor();
    assert.deepEqual(puts, [{ app: "kitty", image_upload: true, clipboard_write: false }]);
    assert.equal(await section.getByText("有尚未保存的修改").count(), 0);
    // Saved: leaving asks nothing.
    await nav.getByRole("button", { name: "外观", exact: true }).click();
    assert.equal(await page.getByRole("dialog").count(), 0);
    assert.equal(await page.evaluate(() => document.querySelector(".settings-content").scrollWidth > document.querySelector(".settings-content").clientWidth), false);
    assert.deepEqual(errors, []);
    console.log("PASS: " + theme + " " + viewport.width + "x" + viewport.height + " lazy section, detection on open, order, one macOS note, both parts, guard, save");
  } finally { await browser.close(); }
}

for (const theme of ["light", "dark"]) {
  for (const viewport of [{ width: 1440, height: 900 }, { width: 1000, height: 700 }]) await run({ theme, viewport });
}
await run({ theme: "light", viewport: { width: 1000, height: 700 }, desktop: false });
