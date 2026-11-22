import assert from "node:assert/strict";
import test from "node:test";
import {
  addDialog, agentJob, currentJob, failureOf, failureView, formatMb, jobEnded, normalizeAgents, progressView, removeDialog, requestFor, rowFor, sizeLine, waitLine,
} from "./other-clients-state.mjs";

const raw = (over = {}) => ({
  id: "codex", name: "Codex", vendor: "OpenAI", kind: "terminal", status: "available", with_environment: false, person_only: false, pinned_version: "0.171.0",
  present: null, update_available: false, supported: true, allowed: true, download_mb: 124, disk_mb: 310,
  hosts: { install: ["github.com"], signin: ["chatgpt.com"], run: ["api.openai.com"], telemetry: [] },
  telemetry_note: "已默认关闭使用数据上报和启动时的更新检查。", region_note: "", data_dirs: [{ path: "~/.codex" }], ...over,
});
const env = { running: true, diskFreeMb: 18637 };
const row = (over, context = {}) => rowFor(normalizeAgents({ agents: [raw(over)] }).entries[0], { environment: env, ...context });

test("the answer is read from agents, entries or a bare list; anything else is not an answer", () => {
  for (const payload of [{ agents: [raw()] }, { entries: [raw()] }, [raw()]]) assert.equal(normalizeAgents(payload).entries.length, 1);
  for (const payload of [null, {}, "x", { agents: "x" }]) assert.equal(normalizeAgents(payload), null);
});

test("hidden entries, Claude (with the environment) and an uninstalled withdrawn entry are not listed", () => {
  const data = normalizeAgents({ agents: [raw(), raw({ id: "h", status: "hidden" }), raw({ id: "claude-code", with_environment: true }), raw({ id: "w", status: "withdrawn" }), raw({ id: "w2", status: "withdrawn", present: { by: "cage", version: "1" } })] });
  assert.deepEqual(data.entries.map((entry) => entry.id), ["codex", "w2"]);
});

test("the environment is read with safe defaults", () => {
  assert.deepEqual(normalizeAgents({ agents: [] }).environment, { running: false, arch: "", guestSystem: "", diskFreeMb: 0, clientsKnown: true });
  assert.equal(normalizeAgents({ agents: [], environment: { clients_known: false, disk_free_mb: null } }).environment.clientsKnown, false);
  assert.equal(normalizeAgents({ agents: [], environment: { clients_known: false, disk_free_mb: null } }).environment.diskFreeMb, 0);
  assert.equal(normalizeAgents({ agents: [], environment: { running: true, disk_free_mb: 5 } }).environment.diskFreeMb, 5);
});

test("an available entry: size line and 添加; the sizes round and switch to GB", () => {
  const available = row();
  assert.equal(available.state, "available");
  assert.equal(available.meta, "下载约 124 MB，安装后约 310 MB");
  assert.deepEqual(available.parts, ["OpenAI", "终端"]);
  assert.deepEqual(available.actions.map((action) => [action.id, action.label, action.disabled]), [["add", "添加", false]]);
  assert.equal(formatMb(18637), "18.2 GB");
  assert.equal(formatMb(123.6), "124 MB");
  assert.equal(formatMb(0), "");
  assert.equal(sizeLine(normalizeAgents({ agents: [raw({ download_mb: 0 })] }).entries[0]), "");
});

test("an environment that is stopped, or another job running, disables the buttons", () => {
  assert.equal(rowFor(normalizeAgents({ agents: [raw()] }).entries[0], { environment: { running: false } }).actions[0].disabled, true);
  assert.equal(row({}, { anyJobRunning: true }).actions[0].disabled, true);
  assert.equal(waitLine({ running: false }, false, false), "启动环境后可以添加。");
  assert.equal(waitLine({ running: true }, true, false), "另一个操作仍在进行，完成后可以继续。");
  assert.equal(waitLine({ running: true }, true, true), "");
  assert.equal(waitLine({ running: true }, false, false), "");
});

test("installing: the download line with megabytes, a bar fraction, 取消; other phases by name", () => {
  const job = agentJob({ id: "j", kind: "agent-install:codex", running: true, setup_progress: { phase: "download", label: "下载", received: 34 * 1048576, total: 124 * 1048576 } });
  const installing = row({}, { job, anyJobRunning: true });
  assert.equal(installing.state, "installing");
  assert.equal(installing.meta, "正在下载 34 / 124 MB");
  assert.ok(Math.abs(installing.bar.fraction - 34 / 124) < 1e-9);
  assert.deepEqual(installing.actions.map((action) => [action.id, action.disabled]), [["cancel", false]]);
  const check = agentJob({ kind: "agent-install:codex", running: true, setup_progress: { phase: "check", label: "检查环境", received: 0, total: 0 } });
  assert.equal(progressView(check).text, "正在检查环境");
  assert.equal(progressView(agentJob({ kind: "agent-install:codex", running: true, setup_progress: { label: "正在校验" } })).text, "正在校验");
  assert.equal(progressView(agentJob({ kind: "agent-update:codex", running: true })).text, "正在更新");
  assert.equal(progressView(agentJob({ kind: "agent-install:codex", running: true })).text, "正在准备");
  assert.equal(progressView(agentJob({ kind: "agent-install:codex", running: true, setup_progress: { phase: "download", received: 9 * 1048576, total: 5 * 1048576 } })).text, "正在下载 9 / 5 MB");
  assert.equal(progressView(agentJob({ kind: "agent-install:codex", running: true, setup_progress: { phase: "unpack", label: "解包", received: 5, total: 10 } })).fraction, 0.5);
});

test("a job of another entry, or of another kind, is not this row's", () => {
  assert.equal(agentJob({ kind: "start", running: true }), null);
  assert.equal(agentJob({ kind: "agent-bogus:codex" }), null);
  assert.equal(agentJob(undefined), null);
  const other = agentJob({ kind: "agent-install:other", running: true });
  assert.equal(row({}, { job: other, anyJobRunning: true }).state, "available");
});

test("remove running: a line without a bar or a cancel", () => {
  const removing = row({ present: { by: "cage", version: "0.171.0" } }, { job: agentJob({ kind: "agent-remove:codex", running: true }), anyJobRunning: true });
  assert.equal(removing.state, "removing");
  assert.equal(removing.meta, "正在移除");
  assert.equal(removing.bar, null);
  assert.deepEqual(removing.actions, []);
});

test("failed: the host's line verbatim, the error colour, 重试 for the same operation", () => {
  const line = "下载中断，已保存进度。重试会继续下载。";
  const failed = row({}, { job: agentJob({ kind: "agent-install:codex", running: false, error: line }) });
  assert.equal(failed.state, "failed");
  assert.deepEqual(failed.error, { line, tone: "error" });
  assert.deepEqual(failed.actions.map((action) => [action.id, action.label]), [["retry:install", "重试"]]);
  const update = row({ present: { by: "cage", version: "0.170.0" }, update_available: true }, { job: agentJob({ kind: "agent-update:codex", running: false, error: line }) });
  assert.equal(update.actions[0].id, "retry:update");
  assert.deepEqual(update.parts, ["0.170.0"]);
});

test("a failed add that found the person's own install shows the own row, not the failure", () => {
  const line = "已经安装了 Codex（自行安装，/home/dev/.local/bin/codex），Super Cage 不会覆盖它。";
  const own = row({ present: { by: "own", version: "0.171.0", state: "ok" }, last_failure: { code: "own-install", message: line } });
  assert.equal(own.state, "own");
  assert.equal(row({ present: { by: "own", version: "0.171.0", state: "ok" } }, { job: agentJob({ kind: "agent-install:codex", running: false, error: line }) }).state, "own");
  // A failed repair or update keeps its line.
  assert.equal(row({ present: { by: "cage", version: "0.171.0", state: "drifted" } }, { job: agentJob({ kind: "agent-install:codex", running: false, error: "请先结束正在运行的 Codex 终端，再更新或修复。" }) }).state, "failed");
});

test("the hosts open in full only when the line leaves something out", () => {
  const few = normalizeAgents({ agents: [raw()] }).entries[0];
  assert.equal(addDialog(few, env).rows.find((item) => item.label === "联网").details, undefined);
  const many = normalizeAgents({ agents: [raw({ hosts: { install: ["github.com"], signin: [], run: ["a", "b", "c"], telemetry: [] } })] }).entries[0];
  assert.equal(addDialog(many, env).rows.find((item) => item.label === "联网").details.length, 2);
});

test("the host's last_failure by code: the line verbatim, canceled is quiet, some codes have no retry", () => {
  const lines = {
    download: "下载中断，已保存进度。重试会继续下载。", digest: "安装文件校验失败，已停止安装，原有版本保持不变。",
    gone: "OpenAI 已不再提供 Codex 0.171.0 的下载。请更新 Super Cage 以获取新的固定版本。", disk: "客机磁盘空间不足：需要 505 MB，当前只有 212 MB。",
    verify: "安装文件无法在这个客机运行，已停止安装，原有文件保持不变。", "in-use": "请先结束正在运行的 Codex 终端，再更新或修复。",
    "own-install": "已经安装了 Codex（自行安装，/home/dev/.local/bin/codex），Super Cage 不会覆盖它。", userns: "这个环境不允许普通用户创建命名空间，Codex 的沙箱无法运行，因此没有安装。",
    arch: "Codex 的固定版本适用于 amd64，这个客机的架构是 arm64，无法安装。", libc: "Codex 需要 glibc 2.31 或更高版本，这个客机是 2.28，无法安装。",
    withdrawn: "Codex 已不再受 Super Cage 支持，无法添加或更新。", canceled: "已取消，已下载的部分会在下次继续使用。", changed: "程序文件在安装时发生了变化。", failed: "添加 Codex失败",
  };
  const noRetry = new Set(["gone", "own-install", "userns", "arch", "libc", "withdrawn"]);
  for (const [code, message] of Object.entries(lines)) {
    const failed = row({ last_failure: { code, message } });
    assert.equal(failed.state, "failed", code);
    assert.deepEqual(failed.error, { line: message, tone: code === "canceled" ? "muted" : "error" }, code);
    assert.equal(failed.actions.length, noRetry.has(code) ? 0 : 1, code + " retry");
  }
  assert.equal(failureOf({ code: "failed", message: "" }).line, "操作未完成。");
  assert.equal(failureOf({ code: "canceled", message: "" }).line, "已取消，已下载的部分会在下次继续使用。");
});

test("a refused request has no code: its sentence decides", () => {
  assert.deepEqual(failureView("另一个操作仍在进行，请稍候"), { tone: "error", retry: true });
  assert.deepEqual(failureView("已经安装了 Codex（自行安装），Super Cage 不会覆盖它。"), { tone: "error", retry: false });
  assert.deepEqual(failureView("已取消，已下载的部分会在下次继续使用。"), { tone: "muted", retry: true });
  assert.equal(failureView("x", true).tone, "muted");
});

test("重试 repeats what was asked in this visit, else what the row's state suggests", () => {
  const failure = { code: "download", message: "下载中断，已保存进度。重试会继续下载。" };
  assert.equal(row({ last_failure: failure }).actions[0].id, "retry:install");
  assert.equal(row({ last_failure: failure, present: { by: "cage", version: "0.170.0" }, update_available: true }).actions[0].id, "retry:update");
  assert.equal(row({ last_failure: failure, present: { by: "cage", version: "0.171.0", state: "drifted" } }).actions[0].id, "retry:install");
  assert.equal(row({ last_failure: failure, present: { by: "cage", version: "0.171.0" } }).actions[0].id, "retry:remove");
  assert.equal(row({ last_failure: failure, present: { by: "cage", version: "0.171.0" } }, { operations: { codex: "update" } }).actions[0].id, "retry:update");
  assert.deepEqual(row({ last_failure: failure, present: { by: "cage", version: "0.171.0" } }).menu.map((item) => item.id), ["remove"]);
  assert.deepEqual(row({ last_failure: failure }).menu, []);
});

test("the host says a job of this entry runs: the row shows it even before the poll does", () => {
  const installing = row({ operation: "install" }, { anyJobRunning: true });
  assert.equal(installing.state, "installing");
  assert.equal(installing.meta, "正在准备");
  assert.equal(installing.bar.fraction, null);
  assert.deepEqual(installing.actions.map((action) => action.id), ["cancel"]);
  assert.equal(row({ operation: "remove", present: { by: "cage", version: "1" } }, { anyJobRunning: true }).state, "removing");
  assert.equal(row({ operation: "bogus" }).state, "available");
});

test("installed: the version, remove behind the menu; the one-time line after an add", () => {
  const installed = row({ present: { by: "cage", version: "0.171.0", state: "ok" } });
  assert.equal(installed.state, "installed");
  assert.deepEqual(installed.parts, ["0.171.0"]);
  assert.deepEqual(installed.actions, []);
  assert.deepEqual(installed.menu.map((item) => item.id), ["remove"]);
  assert.equal(installed.meta, "");
  assert.equal(row({ present: { by: "cage", version: "0.171.0" } }, { added: "codex" }).meta, "已添加。可以在 新建 中打开。");
  assert.equal(row({ present: { by: "cage", version: "0.171.0" } }, { added: "other" }).meta, "");
});

test("update available: 更新 beside the menu; the new version is named", () => {
  const update = row({ present: { by: "cage", version: "0.171.0" }, update_available: true, pinned_version: "0.180.0" });
  assert.equal(update.state, "update");
  assert.equal(update.meta, "有新版本 0.180.0");
  assert.deepEqual(update.actions.map((action) => action.id), ["update"]);
  assert.deepEqual(update.menu.map((item) => item.id), ["remove"]);
  assert.deepEqual(update.parts, ["0.171.0"]);
});

test("drifted: 修复 (the install route) takes the place of 更新", () => {
  const drifted = row({ present: { by: "cage", version: "0.171.0", state: "drifted" }, update_available: true });
  assert.equal(drifted.state, "drifted");
  assert.equal(drifted.meta, "程序文件与安装时不同");
  assert.deepEqual(drifted.actions.map((action) => [action.id, action.label]), [["repair", "修复"]]);
  assert.deepEqual(requestFor("repair", "codex"), ["/agents/codex/install", {}]);
});

test("own install: no button, named as the person's own", () => {
  const own = row({ present: { by: "own", version: "0.171.0", state: "ok" } });
  assert.equal(own.state, "own");
  assert.deepEqual(own.parts, ["已安装（自行安装）", "0.171.0"]);
  assert.deepEqual(own.actions, []);
  assert.deepEqual(own.menu, []);
});

test("unsupported and not allowed: muted, the reason in place of the size line, no button", () => {
  const unsupported = row({ supported: false, unsupported_code: "guest", unsupported_reason: "macOS 客机暂不支持" });
  assert.equal(unsupported.state, "unsupported");
  assert.equal(unsupported.muted, true);
  assert.equal(unsupported.meta, "macOS 客机暂不支持");
  assert.deepEqual(unsupported.actions, []);
  assert.equal(row({ supported: false }).meta, "这个环境暂不支持");
  const denied = row({ allowed: false });
  assert.equal(denied.state, "not-allowed");
  assert.equal(denied.meta, "这个服务器的管理员未开放");
  assert.deepEqual(denied.actions, []);
});

test("withdrawn and installed: 不再受支持, only 移除", () => {
  const withdrawn = row({ status: "withdrawn", present: { by: "cage", version: "0.170.0" } });
  assert.equal(withdrawn.state, "withdrawn");
  assert.equal(withdrawn.meta, "不再受支持");
  assert.deepEqual(withdrawn.actions, []);
  assert.deepEqual(withdrawn.menu.map((item) => item.id), ["remove"]);
});

test("marks: Codex keeps its drawn mark, others their monogram (derived when the entry sends none)", () => {
  assert.deepEqual(row().mark, { builtin: "codex" });
  assert.deepEqual(row({ id: "x", name: "Antigravity CLI", mark: { monogram: "Ag" } }).mark, { monogram: "Ag" });
  assert.deepEqual(row({ id: "x", name: "Antigravity CLI" }).mark, { monogram: "An" });
  assert.deepEqual(row({ id: "x", name: "组合客户端" }).mark, { monogram: "组" });
});

test("the add dialog: source, size with the free space, hosts with details, telemetry, region, the wait", () => {
  const entry = normalizeAgents({ agents: [raw({ region_note: "OpenAI 暂不向中国大陆和香港提供服务；登录取决于所选代理的出口地区。", hosts: { install: ["github.com"], signin: ["auth.openai.com", "chatgpt.com"], run: ["chatgpt.com", "api.openai.com", "x.openai.com"], telemetry: ["t.example"] } })] }).entries[0];
  const dialog = addDialog(entry, env);
  assert.equal(dialog.title, "添加 Codex？");
  assert.deepEqual(dialog.rows.map((item) => item.label), ["来源", "占用", "联网", "遥测", "地区", "安装期间"]);
  const by = Object.fromEntries(dialog.rows.map((item) => [item.label, item]));
  assert.equal(by["来源"].text, "OpenAI 在 GitHub 发布的 0.171.0，已核对校验值");
  assert.equal(by["占用"].text, "下载约 124 MB，安装后约 310 MB（客机剩余 18.2 GB）");
  assert.equal(by["联网"].text, "下载 github.com · 登录 auth.openai.com、chatgpt.com · 使用 chatgpt.com、api.openai.com 等");
  assert.deepEqual(by["联网"].details.map((group) => group.label), ["下载", "登录", "使用", "遥测上报"]);
  assert.equal(by["安装期间"].text, "不能新建终端（包括 Claude 终端），约一分钟。");
  assert.equal(dialog.confirm, "添加");
});

test("the add dialog of a person-only entry adds the terms row, and its telemetry line is the entry's own", () => {
  const entry = normalizeAgents({ agents: [raw({ id: "antigravity-cli", name: "Antigravity CLI", vendor: "Google", person_only: true, telemetry_note: "这个客户端的遥测是账号级设置，Super Cage 无法预先关闭。可在登录后于设置 › 账号中关闭。", hosts: { install: ["storage.googleapis.com"], signin: [], run: [], telemetry: [] } })] }).entries[0];
  const dialog = addDialog(entry, { running: true, diskFreeMb: 0 });
  assert.deepEqual(dialog.rows.map((item) => item.label), ["来源", "占用", "联网", "遥测", "条款", "安装期间"]);
  assert.equal(dialog.rows[0].text, "Google 在 storage.googleapis.com 发布的 0.171.0，已核对校验值");
  assert.equal(dialog.rows[1].text, "下载约 124 MB，安装后约 310 MB");
  assert.equal(dialog.rows.find((item) => item.label === "条款").text, "Google 的条款不允许由其他程序代为运行它，所以它只在你自己打开的终端中使用。");
});

test("the remove dialog offers deleting data only when the entry has data folders", () => {
  assert.equal(removeDialog(normalizeAgents({ agents: [raw()] }).entries[0]).checkbox, "同时删除登录信息和设置");
  assert.equal(removeDialog(normalizeAgents({ agents: [raw({ data_dirs: [] })] }).entries[0]).checkbox, "");
  assert.equal(removeDialog(normalizeAgents({ agents: [raw()] }).entries[0]).title, "移除 Codex？");
});

test("requests: install, update, remove with or without data, cancel with the job, retry as the failed operation", () => {
  assert.deepEqual(requestFor("add", "codex"), ["/agents/codex/install", {}]);
  assert.deepEqual(requestFor("update", "codex"), ["/agents/codex/update", {}]);
  assert.deepEqual(requestFor("remove", "codex"), ["/agents/codex/remove", {}]);
  assert.deepEqual(requestFor("remove", "codex", { removeData: true }), ["/agents/codex/remove", { remove_data: true }]);
  assert.deepEqual(requestFor("cancel", "codex", { job: "j1" }), ["/agents/cancel", { job: "j1" }]);
  assert.deepEqual(requestFor("retry:update", "codex"), ["/agents/codex/update", {}]);
  assert.deepEqual(requestFor("retry:remove", "codex", { removeData: true }), ["/agents/codex/remove", { remove_data: true }]);
});

test("a job ends: only an agent job that was running", () => {
  const running = { id: "a", kind: "agent-install:codex", running: true };
  assert.equal(jobEnded(running, { ...running, running: false }), true);
  assert.equal(jobEnded(running, running), false);
  assert.equal(jobEnded({ id: "s", kind: "start", running: true }, { id: "s", kind: "start", running: false }), false);
  assert.equal(jobEnded(undefined, running), false);
});

test("between accepting a request and the next poll the accepted job stands in; a refusal shows at once", () => {
  const local = { id: "codex", operation: "install", jobId: "j1" };
  const idle = { id: "old", kind: "start", running: false };
  assert.deepEqual(currentJob(idle, local), { operation: "install", id: "codex", running: true, error: "", canceled: false, progress: null, jobId: "j1" });
  assert.equal(currentJob({ id: "j1", kind: "agent-install:codex", running: true }, local).running, true);
  assert.equal(currentJob(idle, null), null);
  const refused = currentJob(idle, { id: "codex", operation: "install", error: "另一个操作仍在进行，请稍候" });
  assert.deepEqual([refused.running, refused.error], [false, "另一个操作仍在进行，请稍候"]);
});

test("a job that has ended is not a job here: its outcome is the entry's last_failure", () => {
  const over = { id: "j9", kind: "agent-install:codex", running: false, error: "下载中断，已保存进度。重试会继续下载。" };
  assert.equal(currentJob(over, null), null);
  assert.equal(currentJob(over, { id: "codex", operation: "install", jobId: "j9" }), null);
  assert.equal(currentJob({ id: "j10", kind: "start", running: true }, null), null);
});

test("the host's progress: phase agent, the installer's step in operation, bytes only for download and unpack", () => {
  const download = agentJob({ id: "j", kind: "agent-install:codex", running: true, setup_progress: { phase: "agent", operation: "download", label: "下载", received: 34 * 1048576, total: 119 * 1048576, bytes_per_second: 2097152 } });
  assert.equal(progressView(download).text, "正在下载 34 / 119 MB");
  assert.ok(Math.abs(progressView(download).fraction - 34 / 119) < 1e-9);
  const step = (operation, label, extra = {}) => progressView(agentJob({ kind: "agent-install:codex", running: true, setup_progress: { phase: "agent", operation, label, ...extra } })).text;
  assert.equal(step("check", "检查环境"), "正在检查环境");
  assert.equal(step("digest", "校验"), "正在校验");
  assert.equal(step("unpack", "解包", { received: 5, total: 10 }), "正在解包");
  assert.equal(step("verify", "验证"), "正在验证");
  assert.equal(step("done", "完成"), "正在完成");
  assert.equal(step("failed", "未完成"), "未完成");
});
