// The three tools' own settings pages, opened from Super Cage's settings:
// T3 Code and Magpie ask the server for their settings page (page:
// "settings"); Herdr opens its tab and gets the key its config binds to
// settings (here ctrl+b then s), typed once its full-screen client draws.
import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

const screenshot = process.env.SCREENSHOT || "";
const herdrState = {
  enabled: true, installed: true, supports_managed: true, server_running: true, version: "0.9.5",
  takeover_terminals: false, theme: "default", workspaces_count: 1, agents_count: 0, blocked_agents_count: 0,
  settings_keys: { onboarding: false, chunks: ["\u0002", "s"], shortcut: "ctrl+b s" },
};
const tool = (name) => ({
  enabled: true, mode: "external", external_url: "https://" + name + ".example.test", open_mode: "workbench",
  installed: true, running: true, supports_managed: true, direct_available: false, focus_chrome: true,
});
const { browser, page, errors } = await createWorkbench({ mockWebSockets: false });
try {
  const opens = [];
  const inputs = [];
  let herdrTerminal = null;
  let drawn = false;
  await page.route("**/api/herdr**", (route) => route.fulfill({ json: herdrState }));
  for (const name of ["t3code", "magpie"]) {
    await page.route("**/api/" + name, (route) => route.fulfill({ json: tool(name) }));
    await page.route("**/api/" + name + "?*", (route) => route.fulfill({ json: tool(name) }));
    await page.route("**/api/" + name + "/open", (route) => {
      opens.push({ name, body: route.request().postDataJSON() });
      return route.fulfill({ json: { mode: "workbench", tab_id: name + "-tab", url: "https://" + name + ".example.test/", title: name, settings_opened: true } });
    });
  }
  await page.route("**/api/terminals", (route) => {
    if (route.request().method() === "GET") return route.fulfill({ json: herdrTerminal ? [herdrTerminal] : [] });
    const body = route.request().postDataJSON();
    assert.equal(body.kind, "herdr");
    herdrTerminal = { id: "herdr-1", kind: "herdr", workspace: body.workspace, title: "Herdr", closed: false };
    return route.fulfill({ json: herdrTerminal });
  });
  await page.routeWebSocket("**/api/**", (socket) => {
    if (!socket.url().includes("/terminals/")) return socket.close();
    socket.onMessage((message) => {
      try {
        const parsed = JSON.parse(String(message));
        if (parsed.type === "input") inputs.push({ data: parsed.data, drawn });
      } catch { /* not JSON: resize or binary */ }
    });
    // Herdr takes a moment before it switches to its full screen.
    setTimeout(() => {
      drawn = true;
      socket.send(Buffer.from("\u001b[?1049h\u001b[H\u001b[2JHerdr · workspace design-lab"));
    }, 700);
  });
  await page.reload();
  await page.getByRole("button", { name: "启动 Super Cage 环境", exact: true }).click();
  await page.getByRole("button", { name: "设置", exact: true }).click();
  await page.getByRole("button", { name: "Agent CLI", exact: true }).click();

  // T3 Code and Magpie: one request each for their settings page.
  await page.getByRole("button", { name: "T3 Code 官方设置", exact: true }).click();
  await expectOpen("t3code");
  await page.getByRole("button", { name: "设置", exact: true }).click();
  await page.getByRole("button", { name: "Agent CLI", exact: true }).click();
  await page.getByRole("button", { name: "Magpie 官方设置", exact: true }).click();
  await expectOpen("magpie");

  // Herdr: its tab opens and gets ctrl+b, then s, after it has drawn.
  await page.getByRole("button", { name: "设置", exact: true }).click();
  await page.getByRole("button", { name: "Agent CLI", exact: true }).click();
  const herdrSettings = page.getByRole("button", { name: "Herdr 官方设置", exact: true });
  if (screenshot) {
    await herdrSettings.scrollIntoViewIfNeeded();
    await page.screenshot({ path: screenshot + "-card.png" });
  }
  await herdrSettings.click();
  await page.locator(".xterm-rows > div").filter({ hasText: "Herdr · workspace" }).first().waitFor();
  await waitFor(() => inputs.length >= 2, "Herdr never got its settings key");
  await page.waitForTimeout(400);
  assert.deepEqual(inputs.map((input) => input.data), ["\u0002", "s"]);
  assert.ok(inputs.every((input) => input.drawn), "keys were typed before Herdr drew its screen");
  if (screenshot) await page.screenshot({ path: screenshot + "-herdr.png" });
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
console.log("T3 Code, Magpie and Herdr each open on their own settings page.");
