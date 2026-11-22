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
// The desktop lists Herdr only where it can open a terminal of the
// environment shown; here it can.
const herdrNote = "在项目上新建终端时用 Herdr 打开；已有的终端仍在 Super Cage 中打开";
const installed = new Set(["ghostty", "iterm2", "terminal", "kitty", "herdr"]);

// The entry on, with Herdr added and connected: the entry's part comes
// first, and Herdr is in one place, its row among the applications (已连接),
// not a second time as a command to set it up.
const entryOn = {
  enabled: true, supported: true, environment: "default", name: "默认环境", state: "in_use", host_alias: "cage-default",
  connections: { total: 2, shell: 1, exec: 0, sftp: 0, herdr: 1 },
  commands: { ssh: "ssh cage-default", cage_ssh: "cage ssh default", attach: "cage attach", cage_on_path: true },
  include: { line: "Include ~/.ssh/super-cage/config", present: true, display_path: "~/.ssh/config" },
  herdr: { state: "connected", command: "hc machine add cage-default", lines: 1 },
  devices: [{ id: "a".repeat(32), name: "MacBook Pro", fingerprint: "SHA256:o/+qFR975llb1cjqdgUG0z2vIkIUeG0n2P6A+7yb6/A", created: new Date().toISOString(), current: true, revoked: false }],
  kept: [], kept_limit: 4,
};

async function run({ theme, viewport, desktop = true, entry = null }) {
  const context = { mode: "local", connection_id: "g".repeat(64), desktop_gateway: desktop, client_platform: "darwin", server_platform: "darwin", connection: { state: "connected" } };
  let settings = { app: "ghostty", image_upload: true, clipboard_write: true };
  const puts = [];
  const named = [];
  let detections = 0;
  const fixture = await createWorkbench({
    viewport, clientContext: context,
    beforeNavigate: async page => {
      await page.emulateMedia({ colorScheme: theme });
      if (entry) await page.route("**/api/entry", route => route.fulfill({ json: entry }));
    },
    clientRequest: async (route, path, body) => {
      if (path === "/client/context") return route.fulfill({ json: context });
      if (path === "/client/local-terminal/apps") {
        detections++;
        named.push(route.request().headers()["x-cage-connection"]);
        return route.fulfill({ json: { apps: table.map(([id, name, automation, hosts]) => ({ id, name, automation, hosts, installed: installed.has(id), ...(hosts ? {} : { machine: true, note: herdrNote }) })) } });
      }
      if (path === "/client/local-terminal") {
        if (route.request().method() === "PUT") { puts.push(body); settings = body; named.push(route.request().headers()["x-cage-connection"]); }
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
    if (entry) {
      await nav.getByRole("button", { name: "本机终端", exact: true }).click();
      const group = page.getByRole("radiogroup", { name: "本机终端应用" });
      await group.getByRole("radio", { name: /Herdr/ }).waitFor();
      const part = page.getByRole("region", { name: "从你自己的终端或 Herdr 连进来", exact: true });
      await part.getByText("已开启 · 2 个连接（ssh 1，Herdr 1）").waitFor();
      const order = await page.locator(".local-terminal-part").evaluateAll(parts => parts.map(part => part.getAttribute("aria-label")));
      assert.deepEqual(order, ["从你自己的终端或 Herdr 连进来", "用哪个终端应用打开"], "with the entry on, connecting in comes first");
      assert.equal(await part.locator('.local-entry-commands li[data-row="herdr"]').count(), 0, "Herdr is not set up a second time");
      await part.getByText("cage attach", { exact: true }).waitFor();
      const herdr = group.locator(".local-terminal-app").filter({ hasText: "Herdr" });
      await herdr.getByText("已连接", { exact: true }).waitFor();
      assert.equal(await group.getByText("已连接", { exact: true }).count(), 1);
      await page.screenshot({ path: fileURLToPath(new URL(theme + "-" + viewport.width + "x" + viewport.height + "-entry-on.png", output)) });
      await herdr.scrollIntoViewIfNeeded();
      await page.screenshot({ path: fileURLToPath(new URL(theme + "-" + viewport.width + "x" + viewport.height + "-entry-on-herdr.png", output)) });
      assert.equal(await page.evaluate(() => document.querySelector(".settings-content").scrollWidth > document.querySelector(".settings-content").clientWidth), false);
      assert.deepEqual(errors, []);
      console.log("PASS: " + theme + " " + viewport.width + "x" + viewport.height + " entry on: connecting in first, Herdr in one place (已连接)");
      return;
    }
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
    assert.deepEqual(names, ["Ghostty", "Herdr", "iTerm2", "kitty", "终端", "Super Cage 内置终端", "Alacritty", "Warp", "WezTerm"]);
    await group.getByText(herdrNote, { exact: true }).waitFor();
    assert.equal(await group.getByRole("radio", { name: /Herdr/ }).isDisabled(), false);
    assert.deepEqual(named, [context.connection_id], "the list is asked for the environment shown");
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
    assert.equal(named.at(-1), context.connection_id, "the save names the environment shown");
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
for (const theme of ["light", "dark"]) await run({ theme, viewport: { width: 1000, height: 700 }, entry: entryOn });
