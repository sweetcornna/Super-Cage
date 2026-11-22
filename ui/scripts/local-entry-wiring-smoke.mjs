import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createWorkbench } from "./workbench-fixture.mjs";

// The wiring of 本机终端 outside its settings page: the sidebar's connections
// group opens it, a terminal an ssh session opened says "来自 <设备名>", and
// its menu offers 保留终端. Run against a dev server:
//   CAGE_UI_URL=http://127.0.0.1:5242 node scripts/local-entry-wiring-smoke.mjs
const output = new URL("../../output/local-entry/", import.meta.url);
await fs.mkdir(output, { recursive: true });
const keeps = [];
const fixture = await createWorkbench({ beforeNavigate: async (page, state) => {
  state.status.vm_running = true;
  state.status.ready = true;
  state.terminals.push({ id: "ssh-1", title: "终端 1", kind: "shell", workspace: state.settings.workspace, origin: { kind: "ssh", device: "林间的 MacBook Pro", kept: false } });
  state.terminals.push({ id: "web-1", title: "终端 2", kind: "shell", workspace: state.settings.workspace });
  state.terminals.push({ id: "ssh-2", title: "终端 3", kind: "shell", workspace: state.settings.workspace, origin: { kind: "ssh", device: "林间的 MacBook Pro", kept: true } });
  await page.route("**/api/terminals/*/keep", (route) => {
    const id = new URL(route.request().url()).pathname.split("/")[3];
    keeps.push(id);
    const terminal = state.terminals.find((item) => item.id === id);
    terminal.origin = { ...terminal.origin, kept: true };
    return route.fulfill({ json: terminal });
  });
  await page.route("**/api/entry", (route) => route.fulfill({ json: { enabled: false, supported: true, environment: "default", name: "默认环境", state: "off", connections: { total: 0 }, devices: [] } }));
} });
const { page, browser, errors } = fixture;
try {
  const row = page.locator('[data-tab-id="terminal:ssh-1"]');
  await row.getByText("来自 林间的 MacBook Pro", { exact: true }).waitFor();
  assert.equal(await page.locator('[data-tab-id="terminal:web-1"]').getByText(/^来自 /).count(), 0, "a web terminal has no origin caption");
  // A kept one says so.
  await page.locator('[data-tab-id="terminal:ssh-2"]').getByText("来自 林间的 MacBook Pro · 已保留", { exact: true }).waitFor();
  await page.screenshot({ path: fileURLToPath(new URL("wiring-sidebar.png", output)) });
  // A sidebar row's right click focuses its actions; 保留终端 is one of them.
  await row.hover();
  await page.getByRole("group", { name: "终端 1 的操作" }).getByRole("button", { name: "保留终端" }).click();
  for (let i = 0; i < 50 && !keeps.length; i++) await page.waitForTimeout(20);
  assert.deepEqual(keeps, ["ssh-1"]);
  assert.equal(await page.getByRole("group", { name: "终端 2 的操作" }).getByRole("button", { name: "保留终端" }).count(), 0, "only ssh terminals can be kept");
  // The connections group: Tailscale · SSH · 本机终端.
  await page.getByRole("button", { name: "本机终端", exact: true }).click();
  const nav = page.getByRole("navigation", { name: "设置分组" });
  assert.equal(await nav.getByRole("button", { name: "本机终端", exact: true }).getAttribute("aria-current"), "page");
  await page.locator(".settings-content").getByRole("heading", { name: "在你自己的终端里使用这个环境" }).waitFor();
  assert.deepEqual(errors, []);
  console.log("PASS: sidebar 本机终端 opens its settings section; ssh terminals say 来自 <设备名> and can be kept");
} finally {
  await browser.close();
}
