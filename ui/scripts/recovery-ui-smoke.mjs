import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { join } from "node:path";
import { smokeCaptureDirectory } from "./smoke-captures.mjs";
import { createWorkbench } from "./workbench-fixture.mjs";

const output = await smokeCaptureDirectory("recovery-ui");
const calls = [];
let update = { phase: "available", currentVersion: "0.2.0-alpha.1", latestVersion: "0.2.0-alpha.2", message: "发现新版本，可下载更新", retryable: true };
const fixture = await createWorkbench({ nativeRequest: async (body) => {
  calls.push(body);
  if (body.action === "workbenchReady") return true;
  if (body.action !== "appUpdate") return null;
  if (body.operation === "install") update = { ...update, phase: "downloading", receivedBytes: 50, totalBytes: 100, message: "正在下载更新" };
  if (body.operation === "cancel") update = { ...update, phase: "available", message: "更新已暂停，可稍后继续" };
  return update;
}, beforeNavigate: async (page) => {
  // The workbench's side of the click: manual mode, and one running terminal
  // command the upgrade would interrupt.
  await page.route("**/api/app-update", (route) => route.fulfill({ json: { mode: "manual", silent_available: true, pending: null, waiting: null, last_attempt: null, chain: null } }));
  await page.route("**/api/app-update/interruptions", (route) => route.fulfill({ json: {
    blockers: [{ id: "terminal-command:7", kind: "terminal", label: "终端里还有命令在运行（make）", interruptible: true }],
    must_wait: [], confirm: ["terminal-command:7"], reopen: [], environment_running: true,
  } }));
} });
const { browser, page, state, errors } = fixture;
const checks = [];
try {
  const button = page.getByRole("button", { name: "升级到 0.2.0-alpha.2", exact: true });
  await button.waitFor();
  assert.match(await button.getAttribute("title"), /会被中断的任务.*停止环境、安装并重启.*不点击不会升级/);
  const bounds = await button.boundingBox();
  assert.ok(bounds.x < 248 && bounds.y > 700, "update action is not in the bottom-left sidebar");
  await page.screenshot({ path: join(output, "update-available.png") });
  // The click lists what will be interrupted; nothing installs before 继续升级.
  await button.click();
  const confirmation = page.getByRole("dialog", { name: "升级到 0.2.0-alpha.2" });
  await confirmation.getByText("终端里还有命令在运行（make）", { exact: true }).waitFor();
  assert.equal(calls.filter((call) => call.operation === "install").length, 0, "the click installed before the confirmation");
  await confirmation.getByRole("button", { name: "继续升级", exact: true }).click();
  await confirmation.waitFor({ state: "hidden" });
  const installs = calls.filter((call) => call.operation === "install");
  assert.equal(installs.length, 1);
  assert.deepEqual(installs[0].confirmed, ["terminal-command:7"], "the install carries exactly what was confirmed");
  await page.getByRole("progressbar", { name: "更新下载进度" }).waitFor();
  assert.equal(await page.getByRole("progressbar").getAttribute("aria-valuenow"), "50");
  assert.equal(await page.getByRole("dialog", { name: "Super Cage 更新" }).count(), 0);
  await page.evaluate(() => window.dispatchEvent(new CustomEvent("cage-app-update", { detail: { phase: "failed", latestVersion: "0.2.0-alpha.2", message: "更新源暂不可访问，仓库尚未公开", retryable: true } })));
  await page.getByRole("button", { name: "重试升级", exact: true }).waitFor();
  await page.getByRole("alert").filter({ hasText: "更新源暂不可访问，仓库尚未公开" }).waitFor();
  checks.push("bottom-left update affordance", "confirmation lists the interruption, then a single install with the confirmed ids", "inline ring progress", "private source failure visible");

  const downloads = [{ id: "qa-download", name: "report.txt", state: "failed", error: "目标目录暂时无法写入", retryable: true }];
  let retries = 0, editorRestarts = 0, componentUpdates = 0;
  await page.route("**/api/browser/tabs", (route) => route.fulfill({ json: { tabs: state.tabs, active: state.active, downloads } }));
  await page.route("**/api/browser/download-retry", (route) => {
    assert.deepEqual(route.request().postDataJSON(), { id: "qa-download" });
    retries++;
    Object.assign(downloads[0], { state: "saved", retryable: false, error: "", path: "/Users/you/Cage/Downloads/report.txt" });
    return route.fulfill({ json: { ok: true } });
  });
  await page.route("**/api/browser/editor-restart", (route) => {
    assert.equal(route.request().postDataJSON().workspace, state.settings.workspace);
    editorRestarts++;
    return route.fulfill({ json: { ok: true } });
  });
  await page.route("**/api/environment/versions", (route) => route.fulfill({ json: {
    vm_running: true, controller: { version: "0.2.0-alpha.2" }, components: { status: "update_required", update_required: true },
    tools: { chromium: { current: "156", required: "系统安全更新" }, code_server: { current: "4.100", required: "4.144.0" } }, tools_update_allowed: false,
  } }));
  await page.route("**/api/environment/update", (route) => { componentUpdates++; return route.fulfill({ json: { id: "qa-components" } }); });
  await page.getByRole("button", { name: "启动 Super Cage 环境", exact: true }).click();
  await page.getByRole("heading", { name: "Super Cage 环境已就绪", exact: true }).waitFor();
  await page.getByRole("button", { name: "打开浏览器", exact: true }).click();
  await page.getByRole("button", { name: "浏览器下载", exact: true }).click();
  await page.getByRole("dialog", { name: "下载文件" }).getByText("目标目录暂时无法写入", { exact: true }).waitFor();
  await page.getByRole("button", { name: "重试保存", exact: true }).click();
  await page.getByText("已保存到本机", { exact: true }).waitFor();
  assert.equal(retries, 1);
  await page.screenshot({ path: join(output, "download-recovery.png") });
  await page.getByRole("dialog", { name: "下载文件" }).getByRole("button", { name: "关闭面板" }).click();
  await page.getByRole("button", { name: "打开 .claude-cage-workspace 的编辑器", exact: true }).click();
  await page.getByRole("button", { name: "浏览器菜单", exact: true }).click();
  await page.getByRole("menuitem", { name: "恢复编辑器", exact: true }).click();
  await page.getByRole("dialog", { name: "重启编辑器？" }).getByRole("button", { name: "重启编辑器", exact: true }).click();
  await page.getByText("编辑器已恢复", { exact: true }).waitFor();
  assert.equal(editorRestarts, 1);
  assert.equal(state.status.vm_running, true);
  checks.push("download retry reports durable save", "editor recovery leaves VM running");
  await page.getByRole("button", { name: "设置", exact: false }).first().click();
  await page.getByRole("button", { name: "环境与恢复", exact: true }).click();
  await page.getByRole("region", { name: "环境版本" }).getByText("需要更新", { exact: true }).waitFor();
  assert.equal(await page.getByRole("button", { name: "更新环境工具", exact: true }).isDisabled(), true);
  await page.getByRole("button", { name: "更新环境工具", exact: true }).scrollIntoViewIfNeeded();
  await page.screenshot({ path: join(output, "environment-versions.png") });
  await page.getByRole("button", { name: "更新环境组件", exact: true }).click();
  await page.getByRole("dialog", { name: "更新环境组件？" }).getByRole("button", { name: "更新组件", exact: true }).click();
  await page.waitForTimeout(50);
  assert.equal(componentUpdates, 1);
  assert.ok(calls.some((call) => call.action === "workbenchReady"), "successful initialized workbench did not acknowledge launch");
  assert.deepEqual(errors, []);
  checks.push("environment versions and guarded tool update", "explicit component update", "initialized workbench launch acknowledgement");
} finally { await browser.close(); }

let prematureReady = false;
const broken = await createWorkbench({ startupFailure: true, nativeRequest: async (body) => {
  if (body.action === "workbenchReady") prematureReady = true;
  return { phase: "current", currentVersion: "0.2.0-alpha.2" };
} });
try {
  await broken.page.waitForResponse((response) => response.url().endsWith("/api/status") && response.status() === 503);
  await broken.page.waitForTimeout(100);
  assert.equal(prematureReady, false, "failed API initialization acknowledged update success");
  checks.push("startup failure never acknowledges upgrade");
} finally { await broken.browser.close(); }
await fs.writeFile(join(output, "ui-checks.json"), JSON.stringify({ checks }, null, 2));
console.log(JSON.stringify({ checks }));
