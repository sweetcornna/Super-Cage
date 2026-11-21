import assert from "node:assert/strict";
import test from "node:test";
import { appUpdateButton, appUpdateConfirmation, appUpdateSettings, formatWhen, readStatusFailure } from "./app-update-state.mjs";

const now = Date.parse("2026-11-21T03:00:00Z");
const manualStatus = { mode: "manual", silent_available: true, pending: null, waiting: null, last_attempt: null, chain: null };
const silentStatus = (extra = {}) => ({ ...manualStatus, mode: "silent", pending: { version: "0.1.16", installable: true }, ...extra });
const job = (label) => ({ id: "job:x", kind: "job", label, interruptible: false });
const terminal = { id: "terminal-command:7", kind: "terminal", label: "终端里还有命令在运行（make）", interruptible: true };

// Every state the button can be in: [name, input, label, action, busy, tone, hint fragment].
const buttonTable = [
  ["nothing new", { shell: { phase: "current" }, status: manualStatus }, null],
  ["checking only", { shell: { phase: "idle" }, status: null }, null],
  ["available, manual", { shell: { phase: "available", latestVersion: "0.1.16" }, status: manualStatus }, "升级到 0.1.16", "confirm", false, "normal", "不点击不会升级"],
  ["available, status unreadable", { shell: { phase: "available", latestVersion: "0.1.16" }, status: null }, "升级到 0.1.16", "confirm", false, "normal", "不点击不会升级"],
  ["ready after an earlier download", { shell: { phase: "ready", latestVersion: "0.1.16" }, status: manualStatus }, "升级到 0.1.16", "confirm", false, "normal", ""],
  ["waiting for idle", { shell: { phase: "available", latestVersion: "0.1.16" }, status: silentStatus({ waiting: { code: "busy", message: "等待空闲：终端里还有命令在运行（make）", blockers: [terminal] } }) }, "升级到 0.1.16", "confirm", false, "waiting", "等待空闲：终端里还有命令在运行（make）"],
  ["waiting for the release to age", { shell: { phase: "available", latestVersion: "0.1.16" }, status: silentStatus({ waiting: { code: "too_new", message: "新版本发布未满 24 小时，稍后再自动升级" } }) }, "升级到 0.1.16", "confirm", false, "waiting", "未满 24 小时"],
  ["silent refused after a failure", { shell: { phase: "available", latestVersion: "0.1.16" }, status: silentStatus({ waiting: { code: "failed_before", message: "0.1.16 上次升级没有完成，不会自动重试；可以点击升级重试" } }) }, "升级到 0.1.16", "confirm", false, "normal", "不会自动重试"],
  ["silent request written", { shell: { phase: "available", latestVersion: "0.1.16" }, status: silentStatus({ pending: { version: "0.1.16", installable: true, requested: true } }) }, "升级到 0.1.16", "confirm", false, "normal", "即将自动升级"],
  ["silent, shell not checked yet", { shell: {}, status: silentStatus() }, "升级到 0.1.16", "confirm", false, "normal", "电脑空闲时会自动升级"],
  ["downloading", { shell: { phase: "downloading", latestVersion: "0.1.16", receivedBytes: 42, totalBytes: 100 }, status: manualStatus }, "正在下载 42%", null, true, "normal", ""],
  ["verifying", { shell: { phase: "verifying", latestVersion: "0.1.16" }, status: manualStatus }, "正在验证", null, true, "normal", ""],
  ["stopping the environment", { shell: { phase: "installing", latestVersion: "0.1.16", message: "正在停止环境，准备安装" }, status: manualStatus }, "正在停止环境", null, true, "normal", "正在停止环境"],
  ["installing", { shell: { phase: "installing", latestVersion: "0.1.16", message: "正在安装并重启 Super Cage" }, status: manualStatus }, "正在安装，完成后自动重启", null, true, "normal", ""],
  ["must wait for a job", { shell: { phase: "failed", retryable: true, latestVersion: "0.1.16", message: "以下操作完成后才能升级：「更新环境组件」正在进行", blocked: { code: "must_wait", message: "以下操作完成后才能升级：「更新环境组件」正在进行" } }, status: manualStatus }, "升级到 0.1.16", "confirm", false, "waiting", "「更新环境组件」"],
  ["new work since the confirmation", { shell: { phase: "failed", retryable: true, latestVersion: "0.1.16", message: "有新的任务正在进行，请确认后再升级", blocked: { code: "confirm_required", message: "有新的任务正在进行，请确认后再升级" } }, status: manualStatus }, "升级到 0.1.16", "confirm", false, "waiting", "重新确认"],
  ["failed", { shell: { phase: "failed", latestVersion: "0.1.16", message: "环境未能安全停止，未安装更新" }, status: manualStatus }, "重试升级", "confirm", false, "failed", "环境未能安全停止"],
  ["rate limited", { shell: { phase: "failed", latestVersion: "0.1.16", message: "更新源请求受限", retryAt: "2026-11-21T04:00:00Z" }, status: manualStatus }, "等待重试", null, false, "failed", "后重试"],
  ["not installable in the app", { shell: { phase: "manual", latestVersion: "0.1.16", message: "此版本的 macOS 安装包未经 Apple 公证，暂不能在应用内安装。" }, status: manualStatus }, "查看新版本", "releases", false, "normal", "未经 Apple 公证"],
  ["restoring after relaunch", { shell: { phase: "current" }, status: { ...manualStatus, chain: { step: "restoring", from: "0.1.15", to: "0.1.16" } } }, "正在恢复环境", null, true, "normal", "已升级到 0.1.16"],
  ["waiting for unlock", { shell: { phase: "current" }, status: { ...manualStatus, chain: { step: "restoring", to: "0.1.16", note: "解锁 Super Cage 后继续恢复环境" } } }, "正在恢复环境", null, true, "normal", "解锁"],
  ["syncing components", { shell: { phase: "current" }, status: { ...manualStatus, chain: { step: "components", to: "0.1.16" } } }, "正在同步环境组件", null, true, "normal", ""],
  ["checking the network", { shell: { phase: "current" }, status: { ...manualStatus, chain: { step: "network", to: "0.1.16" } } }, "正在检查网络", null, true, "normal", ""],
  ["environment did not start", { shell: { phase: "current" }, status: { ...manualStatus, chain: { step: "failed", to: "0.1.16", next: "start_environment", error: "已升级到 0.1.16，但环境未能启动：磁盘已满" } } }, "启动环境", "start_environment", false, "failed", "磁盘已满"],
  ["network check failed", { shell: { phase: "current" }, status: { ...manualStatus, chain: { step: "failed", to: "0.1.16", next: "check_network", error: "联网检查未通过" } } }, "检查联网", "check_network", false, "failed", "联网检查未通过"],
  ["network fixed since", { shell: { phase: "current" }, status: { ...manualStatus, chain: { step: "failed", to: "0.1.16", next: "check_network" } }, environment: { networkOk: true } }, null],
  ["installer rolled back", { shell: { phase: "failed", latestVersion: "0.1.16" }, status: { ...manualStatus, chain: { step: "failed", to: "0.1.16", next: "retry_update", error: "签名校验失败，已恢复原应用" } } }, "重试升级", "confirm", false, "failed", "签名校验失败"],
  ["unfinished without a result", { shell: { phase: "available", latestVersion: "0.1.16" }, status: { ...manualStatus, chain: { step: "installing", to: "0.1.16", note: "升级没有完成，环境保持停止；可以重新点击升级，或手动启动环境" } } }, "升级没有完成", "confirm", false, "failed", "环境保持停止"],
  ["unknown record", { shell: { phase: "current" }, status: { ...manualStatus, chain: { step: "attention", error: "升级进度记录无法读取，可以重新点击升级" } } }, "升级需要处理", null, false, "failed", "无法读取"],
  ["done", { shell: { phase: "current", message: "Super Cage 已更新至 0.1.16" }, status: { ...manualStatus, chain: { step: "done", to: "0.1.16" } } }, null],
];

test("every button state has its label, action and hint", () => {
  for (const [name, input, label, action, busy, tone, hint] of buttonTable) {
    const b = appUpdateButton({ ...input, now });
    if (label === null) {
      assert.equal(b.visible, false, name);
      continue;
    }
    assert.equal(b.visible, true, name);
    assert.equal(b.label, label, name);
    assert.equal(b.action, action, name);
    assert.equal(b.busy, busy, name);
    assert.equal(b.tone, tone, name);
    assert.equal(b.disabled, busy || action === null, name);
    assert.ok(b.hint.includes(hint), `${name}: ${b.hint}`);
    assert.ok(b.hint.length > 0, name);
  }
  assert.equal(appUpdateButton({ shell: { phase: "downloading", latestVersion: "0.1.16", receivedBytes: 42, totalBytes: 100 } }).percent, 42);
});

test("manual mode never claims that anything installs by itself", () => {
  for (const [name, input] of buttonTable) {
    if (input.status?.mode === "silent") continue;
    const b = appUpdateButton({ ...input, now });
    assert.doesNotMatch(b.hint, /自动升级|静默/, name);
  }
  const rows = appUpdateSettings({ status: manualStatus }).rows.map((row) => row.text).join("\n");
  assert.match(rows, /不会自动更新/);
  assert.doesNotMatch(rows, /即将自动升级|等待空闲/);
});

test("the confirmation lists what will be interrupted", () => {
  const c = appUpdateConfirmation({
    version: "0.1.16",
    interruptions: {
      blockers: [terminal, { id: "unknown:terminals", kind: "unknown", label: "无法确认终端里是否还有命令在运行", interruptible: true, unknown: true }],
      must_wait: [],
      confirm: ["terminal-command:7", "unknown:terminals"],
      reopen: [{ title: "dev", kind: "shell" }, { title: "claude", kind: "claude" }],
      environment_running: true,
    },
  });
  assert.equal(c.title, "升级到 0.1.16");
  assert.equal(c.body, "升级会停止环境并重启 Super Cage，完成后自动恢复原来的环境。以下内容会被中断：");
  assert.deepEqual(c.items.map((item) => item.label), [terminal.label, "无法确认终端里是否还有命令在运行"]);
  assert.equal(c.items[1].unknown, true);
  assert.deepEqual(c.confirmed, ["terminal-command:7", "unknown:terminals"]);
  assert.equal(c.note, "这些终端会关闭，升级后可以重新打开：dev、claude");
  assert.equal(c.canContinue, true);
  assert.equal(c.continueLabel, "继续升级");
  assert.equal(c.cancelLabel, "稍后");
});

test("the confirmation cannot continue past work that must finish", () => {
  const c = appUpdateConfirmation({ version: "0.1.16", interruptions: { blockers: [], must_wait: [job("「更新环境组件」正在进行")], confirm: [], reopen: [], environment_running: true } });
  assert.equal(c.canContinue, false);
  assert.equal(c.waitMessage, "请等待「更新环境组件」完成后再升级");
  const storage = appUpdateConfirmation({ version: "0.1.16", interruptions: { blockers: [], must_wait: [{ id: "storage", label: "环境存储等待恢复" }], confirm: [], reopen: [], environment_running: false } });
  assert.equal(storage.waitMessage, "以下情况处理完成后才能升级：环境存储等待恢复");
  const idle = appUpdateConfirmation({ version: "0.1.16", interruptions: { blockers: [], must_wait: [], confirm: [], reopen: [], environment_running: true } });
  assert.equal(idle.body, "升级会停止环境并重启 Super Cage，完成后自动恢复原来的环境。现在没有正在进行的任务。");
  assert.equal(appUpdateConfirmation({ version: "0.1.16", interruptions: { blockers: [], must_wait: [], confirm: [], reopen: [], environment_running: false } }).body, "升级会重启 Super Cage。");
});

test("the shell's refusal names what to confirm when the workbench cannot be asked", () => {
  const blocked = { code: "confirm_required", message: "有新的任务正在进行，请确认后再升级", blockers: [terminal, { id: "job:x", label: "「更新环境组件」正在进行", interruptible: false }] };
  const c = appUpdateConfirmation({ version: "0.1.16", error: { status: 409, message: "应用升级由本机管理，请切换到本机后查看" }, blocked });
  assert.deepEqual(c.items.map((item) => item.id), ["terminal-command:7"]);
  assert.deepEqual(c.confirmed, ["terminal-command:7"]);
  assert.equal(c.canContinue, false);
  assert.equal(c.waitMessage, "请等待「更新环境组件」完成后再升级");
});

test("a failed chain hides its action once the workbench reports it done", () => {
  const chain = { step: "failed", to: "0.1.16", next: "start_environment", error: "已升级到 0.1.16，但环境未能启动" };
  assert.equal(appUpdateButton({ shell: { phase: "current" }, status: { ...manualStatus, chain, environment: { running: false } } }).label, "启动环境");
  assert.equal(appUpdateButton({ shell: { phase: "current" }, status: { ...manualStatus, chain, environment: { running: true, network_ok: true, components_current: true } } }).visible, false);
});

test("the confirmation still offers the click when the list cannot be read", () => {
  const cloud = appUpdateConfirmation({ version: "0.1.16", error: { status: 409, message: "应用升级由本机管理，请切换到本机后查看" } });
  assert.equal(cloud.canContinue, true);
  assert.match(cloud.note, /云端任务不受影响/);
  const unknown = appUpdateConfirmation({ version: "0.1.16", error: { status: 502, message: "远程连接已中断" } });
  assert.match(unknown.note, /升级前还会再检查一次/);
});

test("settings rows for every mode", () => {
  const manual = appUpdateSettings({ status: manualStatus });
  assert.equal(manual.mode, "manual");
  assert.equal(manual.editable, true);
  assert.deepEqual(manual.options.map((o) => [o.value, o.label, o.disabled]), [["manual", "手动更新（默认）", false], ["silent", "空闲时自动静默更新", false]]);
  assert.deepEqual(manual.rows.map((r) => r.key), ["explain"]);

  const waiting = appUpdateSettings({ status: silentStatus({ waiting: { code: "busy", message: "等待空闲", blockers: [terminal] }, last_attempt: { version: "0.1.15", result: "ok", finished_at: "2026-11-20T19:12:00Z" } }), timeZone: "Asia/Shanghai" });
  assert.equal(waiting.mode, "silent");
  assert.deepEqual(waiting.rows.map((r) => r.text), [
    "只在没有运行中的任务、终端命令、下载、手机或远程连接，且电脑 30 分钟无人操作时升级；失败会恢复原版本，并且不会自动重试同一版本。",
    "新版本 0.1.16 已就绪，等待空闲：终端里还有命令在运行（make）",
    "上次升级：已升级到 0.1.15（11月21日 03:12）",
  ]);

  const failed = appUpdateSettings({ status: silentStatus({ waiting: { code: "failed_before", message: "0.1.16 上次升级没有完成，不会自动重试；可以点击升级重试" }, last_attempt: { version: "0.1.16", result: "failed", reason: "环境未能安全停止，未安装更新" } }) });
  assert.equal(failed.rows.at(-1).text, "上次升级未完成：环境未能安全停止，未安装更新（不会自动重试同一版本）");

  const unsupported = appUpdateSettings({ status: { ...manualStatus, silent_available: false, silent_unavailable_reason: "Linux 版本由系统安装包管理，不支持静默更新" } });
  assert.equal(unsupported.options[1].disabled, true);
  assert.equal(unsupported.rows.find((r) => r.key === "unavailable").text, "此安装方式不支持静默更新：Linux 版本由系统安装包管理，不支持静默更新");

  const corrupt = appUpdateSettings({ status: { ...manualStatus, mode_error: "更新方式记录无效，已按手动更新处理" } });
  assert.equal(corrupt.rows[0].text, "更新方式记录无效，已按手动更新处理");

  const notInstallable = appUpdateSettings({ status: silentStatus({ pending: { version: "0.1.16", installable: false, reason: "此版本的 Windows 安装包没有发布者签名" } }) });
  assert.match(notInstallable.rows.find((r) => r.key === "waiting").text, /需要手动安装/);
});

test("settings when the workbench cannot answer", () => {
  for (const [error, kind, text] of [
    [{ status: 409, message: "应用升级由本机管理，请切换到本机后查看" }, "local_required", "应用升级由本机管理，请切换到本机后设置"],
    [{ status: 409, message: "连接不可用或已切换，请重新选择工作环境" }, "unreachable", "连接不可用或已切换，请重新选择工作环境"],
    [{ status: 423, message: "Super Cage 已锁定" }, "locked", "解锁 Super Cage 后可查看升级设置"],
    [{ status: 403 }, "forbidden", "只能在本机 Super Cage 窗口中查看升级设置"],
    [{ status: 502, message: "远程连接已中断" }, "unreachable", "远程连接已中断"],
  ]) {
    assert.equal(readStatusFailure(error).kind, kind);
    const s = appUpdateSettings({ error });
    assert.equal(s.editable, false);
    assert.ok(s.options.every((o) => o.disabled));
    assert.equal(s.rows[0].text, text);
  }
});

test("dates read as 11月21日 03:12", () => {
  assert.equal(formatWhen("2026-11-20T19:12:00Z", "Asia/Shanghai"), "11月21日 03:12");
  assert.equal(formatWhen("not a date"), "");
});
