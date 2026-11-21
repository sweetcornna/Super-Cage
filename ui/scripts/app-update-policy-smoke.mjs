import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { createWorkbench } from "./workbench-fixture.mjs";

// The update policy surfaces: the mode setting in 设置 → 关于, the button's
// waiting hint in silent mode, a confirmation blocked by work that must
// finish, a refusal that asks for a new confirmation, and a failed
// post-update step. The workbench is faked at /api/app-update*.
const output = process.env.CAGE_UPDATE_QA_OUTPUT || "/tmp/cage-update-diagnosis/policy";
await fs.mkdir(output, { recursive: true });
const terminal = { id: "terminal-command:7", kind: "terminal", label: "终端里还有命令在运行（make）", interruptible: true };
const job = { id: "job:environment-update", kind: "job", label: "「更新环境组件」正在进行", interruptible: false };
const workbench = {
  status: { mode: "manual", silent_available: true, pending: null, waiting: null, last_attempt: { version: "0.1.15", from: "0.1.14", trigger: "click", result: "ok", started_at: "2026-11-20T19:10:00Z", finished_at: "2026-11-20T19:12:00Z" }, chain: null },
  interruptions: { blockers: [terminal], must_wait: [job], confirm: [terminal.id], reopen: [], environment_running: true },
  policies: [],
  started: 0,
};
let update = { phase: "available", currentVersion: "0.1.15", latestVersion: "0.1.16" };
const installs = [];
const fixture = await createWorkbench({
  nativeRequest: async body => {
    if (body.action !== "appUpdate") return body.action === "workbenchReady" ? true : null;
    if (body.operation === "install") installs.push(body.confirmed);
    return update;
  },
  beforeNavigate: async page => {
    await page.route("**/api/app-update", route => route.fulfill({ json: workbench.status }));
    await page.route("**/api/app-update/interruptions", route => route.fulfill({ json: workbench.interruptions }));
    await page.route("**/api/app-update/policy", route => {
      const { mode } = route.request().postDataJSON();
      workbench.policies.push(mode);
      workbench.status = { ...workbench.status, mode };
      return route.fulfill({ json: workbench.status });
    });
    await page.route("**/api/start", route => { workbench.started++; return route.fulfill({ json: { id: "start", kind: "start", running: true } }); });
  },
});
const { page, browser, errors } = fixture;
const publish = async value => {
  update = value;
  await page.evaluate(detail => window.dispatchEvent(new CustomEvent("cage-app-update", { detail })), value);
};
try {
  // 设置 → 关于: manual by default, the explanation, the last result.
  await page.getByRole("button", { name: "设置", exact: true }).click();
  await page.getByRole("navigation", { name: "设置分组" }).getByRole("button", { name: "关于", exact: true }).click();
  const section = page.getByRole("region", { name: "应用更新", exact: true });
  await section.getByText("不会自动更新。Super Cage 只会检查是否有新版本，不会自行下载或安装。").waitFor();
  assert.ok(await section.getByRole("radio", { name: /手动更新（默认）/ }).isChecked());
  await section.getByText(/上次升级：已升级到 0.1.15（/).waitFor();
  await page.screenshot({ path: `${output}/about-manual.png` });
  // The choice is saved by the workbench; the radio follows its answer.
  await section.getByRole("radio", { name: /空闲时自动静默更新/ }).click();
  await page.waitForFunction(() => document.querySelector('input[name="app-update-mode"][value="silent"]')?.checked);
  await section.getByText(/只在没有运行中的任务、终端命令、下载、手机或远程连接/).waitFor();
  assert.deepEqual(workbench.policies, ["silent"], "choosing silent posts the mode once");
  await page.screenshot({ path: `${output}/about-silent.png` });

  // Where silent mode is unavailable the option says why and cannot be chosen.
  workbench.status = { ...workbench.status, mode: "manual", silent_available: false, silent_unavailable_reason: "Windows 版本暂不支持静默更新，可以点击升级" };
  await page.getByRole("navigation", { name: "设置分组" }).getByRole("button", { name: "通用", exact: true }).click().catch(() => {});
  await page.getByRole("navigation", { name: "设置分组" }).getByRole("button", { name: "关于", exact: true }).click();
  await section.getByText("Windows 版本暂不支持静默更新，可以点击升级").first().waitFor();
  assert.ok(await section.getByRole("radio", { name: /空闲时自动静默更新/ }).isDisabled());
  await page.keyboard.press("Escape");

  // Silent mode waiting for idle: the button says what it waits for.
  workbench.status = { ...workbench.status, mode: "silent", silent_available: true, pending: { version: "0.1.16", installable: true }, waiting: { code: "busy", message: "等待空闲：终端里还有命令在运行（make）", blockers: [terminal] } };
  await publish({ ...update });
  const button = page.getByRole("button", { name: "升级到 0.1.16", exact: true });
  await button.waitFor();
  await page.waitForFunction(() => /等待空闲：终端里还有命令在运行/.test(document.querySelector(".app-update-button")?.title || ""));

  // Work that must finish first: the confirmation lists it and cannot continue.
  await button.click();
  const dialog = page.getByRole("dialog", { name: "升级到 0.1.16" });
  await dialog.getByText("请等待「更新环境组件」完成后再升级").waitFor();
  assert.ok(await dialog.getByRole("button", { name: "继续升级", exact: true }).isDisabled());
  await page.waitForTimeout(400);
  await page.screenshot({ path: `${output}/confirmation-must-wait.png` });
  await dialog.getByRole("button", { name: "稍后", exact: true }).click();
  assert.equal(installs.length, 0);

  // A refusal asking for a new confirmation reopens it once, with the list.
  workbench.interruptions = { blockers: [terminal], must_wait: [], confirm: [terminal.id], reopen: [], environment_running: true };
  await publish({ ...update, phase: "failed", retryable: true, message: "有新的任务正在进行，请确认后再升级", blocked: { code: "confirm_required", message: "有新的任务正在进行，请确认后再升级", blockers: [terminal] } });
  await dialog.getByText("终端里还有命令在运行（make）").waitFor();
  await dialog.getByRole("button", { name: "继续升级", exact: true }).click();
  await dialog.waitFor({ state: "hidden" });
  assert.deepEqual(installs, [[terminal.id]]);

  // After the update: the environment did not start; the button offers it.
  workbench.status = { ...workbench.status, mode: "manual", pending: null, waiting: null, chain: { step: "failed", from: "0.1.15", to: "0.1.16", trigger: "click", failed_step: "restoring", error: "已升级到 0.1.16，但环境未能启动：磁盘空间不足", next: "start_environment" }, environment: { running: false, network_ok: false, components_current: false } };
  await publish({ phase: "current", currentVersion: "0.1.16" });
  const start = page.getByRole("button", { name: "启动环境", exact: true }).and(page.locator(".app-update-button"));
  await start.waitFor();
  assert.match(await start.getAttribute("title"), /磁盘空间不足/);
  await page.screenshot({ path: `${output}/chain-failed.png` });
  await start.click();
  await page.waitForFunction(() => true);
  for (let attempt = 0; attempt < 50 && !workbench.started; attempt++) await new Promise(resolve => setTimeout(resolve, 100));
  assert.equal(workbench.started, 1, "the follow-up action uses the existing start");
  assert.deepEqual(errors, []);
  console.log("PASS: update mode setting, unavailable reason, waiting hint, must-wait confirmation, refusal reopens the confirmation, failed post-update step");
} finally { await browser.close(); }
