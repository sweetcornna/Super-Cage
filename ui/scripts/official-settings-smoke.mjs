// The tools' own settings pages, opened from Super Cage's settings: T3 Code
// and Magpie ask the server for their settings page (page: "settings").
// Herdr (herdr-core) has no settings page of its own any more: its card
// shows its version, state and terminals, no theme or 官方设置, and the old
// Herdr TUI's line, whose 卸载旧版 waits until the TUI holds no terminals.
import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

const screenshot = process.env.SCREENSHOT || "";
const herdrState = {
  enabled: true, installed: true, supports_managed: true, server_running: true, version: "0.1.22",
  binary_path: "/home/cage/.local/bin/herdr-core", takeover_terminals: true, terminals_count: 3,
  keeps_running_after_logout: true, legacy_installed: true,
};
const tool = (name) => ({
  enabled: true, mode: "external", external_url: "https://" + name + ".example.test", open_mode: "workbench",
  installed: true, running: true, supports_managed: true, direct_available: false, focus_chrome: true,
});
const { browser, page, errors } = await createWorkbench({ mockWebSockets: false });
try {
  const opens = [];
  const legacyRemovals = [];
  let legacy = { installed: true, server_running: true, panes: 2, busy: true };
  await page.route("**/api/herdr**", (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/herdr/legacy") return route.fulfill({ json: legacy });
    if (path === "/api/herdr/legacy/uninstall") {
      legacyRemovals.push(route.request().postDataJSON());
      legacy = { installed: false, server_running: false, panes: 0, busy: false };
      herdrState.legacy_installed = false;
      return route.fulfill({ json: legacy });
    }
    return route.fulfill({ json: herdrState });
  });
  for (const name of ["t3code", "magpie"]) {
    await page.route("**/api/" + name, (route) => route.fulfill({ json: tool(name) }));
    await page.route("**/api/" + name + "?*", (route) => route.fulfill({ json: tool(name) }));
    await page.route("**/api/" + name + "/open", (route) => {
      opens.push({ name, body: route.request().postDataJSON() });
      return route.fulfill({ json: { mode: "workbench", tab_id: name + "-tab", url: "https://" + name + ".example.test/", title: name, settings_opened: true } });
    });
  }
  await page.reload();
  await page.getByRole("button", { name: "启动 Super Cage 环境", exact: true }).click();
  await page.getByRole("button", { name: "设置", exact: true }).click();
  await page.getByRole("button", { name: "T3 Code", exact: true }).click();

  // T3 Code and Magpie: one request each for their settings page.
  await page.getByRole("button", { name: "T3 Code 官方设置", exact: true }).click();
  await expectOpen("t3code");
  await page.getByRole("button", { name: "设置", exact: true }).click();
  await page.getByRole("button", { name: "Agent CLI", exact: true }).click();
  await page.getByRole("button", { name: "Magpie 官方设置", exact: true }).click();
  await expectOpen("magpie");

  // Herdr: no settings page of its own, no theme; its version, state and
  // terminals; the old TUI's line.
  await page.getByRole("button", { name: "设置", exact: true }).click();
  await page.getByRole("button", { name: "Agent CLI", exact: true }).click();
  const card = page.getByRole("region", { name: "Herdr 持久终端", exact: true });
  await card.scrollIntoViewIfNeeded();
  assert.equal(await page.getByRole("button", { name: "Herdr 官方设置", exact: true }).count(), 0);
  assert.equal(await card.getByRole("combobox").count(), 0, "the Herdr card still offers a theme");
  await card.getByText("v0.1.22", { exact: true }).waitFor();
  await card.getByText("3 个终端", { exact: true }).waitFor();
  await card.getByText("旧版 Herdr 仍在运行，里面有 2 个终端", { exact: true }).waitFor();
  const removeLegacy = card.getByRole("button", { name: "卸载旧版", exact: true });
  assert.equal(await removeLegacy.isDisabled(), true, "the old TUI could be removed with terminals in it");
  if (screenshot) await page.screenshot({ path: screenshot + "-card.png" });
  // Once the old TUI holds nothing it goes, after a confirmation.
  legacy = { installed: true, server_running: false, panes: 0, busy: false };
  const nav = page.getByRole("navigation", { name: "设置分组" });
  await nav.getByRole("button", { name: "通用", exact: true }).click();
  await nav.getByRole("button", { name: "Agent CLI", exact: true }).click();
  await card.getByText("旧版 Herdr 还留在环境里", { exact: true }).waitFor();
  await removeLegacy.click();
  await page.getByRole("dialog").getByRole("button", { name: "卸载旧版", exact: true }).click();
  await card.getByText(/旧版 Herdr/).waitFor({ state: "detached" });
  assert.deepEqual(legacyRemovals, [{}]);
  assert.deepEqual(errors, []);

  async function expectOpen(name) {
    await waitFor(() => opens.some((open) => open.name === name), name + " was not opened");
    const open = opens.find((entry) => entry.name === name);
    assert.equal(open.body.page, "settings", name + " opened without its settings page");
  }
  async function waitFor(check, message, timeout = 10000) {
    const started = Date.now();
    while (!check()) {
      if (Date.now() - started > timeout) throw new Error(message);
      await new Promise((resolve) => setTimeout(resolve, 50));
    }
  }
} finally { await browser.close(); }
console.log("T3 Code and Magpie open on their own settings page; Herdr shows its state and the old TUI's line, with no settings page or theme.");
