import assert from "node:assert/strict";
import test from "node:test";
import {
  commandRows,
  connectionSummary,
  entryStateLine,
  includeDialog,
  includeRow,
  loadError,
  relativeTime,
  revokeConfirm,
  shortFingerprint,
  sortedDevices,
  validDeviceName,
} from "./local-terminal-state.mjs";

const on = {
  enabled: true,
  supported: true,
  environment: "default",
  name: "默认环境",
  state: "idle",
  host_alias: "cage-default",
  connections: { total: 0, shell: 0, exec: 0, sftp: 0 },
  commands: { ssh: "ssh cage-default", cage_ssh: "cage ssh default", cage_on_path: true },
  include: { line: "Include ~/.ssh/super-cage/config", present: true },
  devices: [],
  herdr: false,
};

test("every state has one plain Chinese line", () => {
  const lines = ["off", "idle", "in_use", "paused", "stopped", "starting", "unsupported"].map((state) =>
    entryStateLine({ ...on, state, connections: { total: 2 } }).text);
  assert.deepEqual(lines, [
    "未开启",
    "已开启 · 没有连接",
    "已开启 · 2 个连接",
    "网络已暂停 · ssh 已断开，恢复网络后可重新连接",
    "环境已停止 · 启动环境后可连接",
    "环境正在启动 · 启动完成后可连接",
    "这个环境的系统暂不支持本机终端",
  ]);
  for (const line of lines) assert.doesNotMatch(line, /[A-Za-z]{4,}|error|undefined/);
  assert.equal(entryStateLine(on, { locked: true }).text, "Super Cage 已锁定 · 解锁后可连接");
  assert.match(entryStateLine({ ...on, revoked: true }).text, /钥匙已撤销/);
  assert.equal(entryStateLine({ ...on, state: "idle" }).tone, "ok");
  assert.equal(entryStateLine({ ...on, state: "paused" }).tone, "warn");
});

test("connections split by kind only once Herdr exists", () => {
  assert.equal(connectionSummary({ connections: { total: 1 } }), "1 个连接");
  assert.equal(connectionSummary({ herdr: true, connections: { total: 2, herdr: 1 } }), "2 个连接（ssh 1，Herdr 1）");
  assert.equal(connectionSummary({ connections: {} }), "没有连接");
});

test("the first command works with and without the Include line", () => {
  assert.deepEqual(commandRows(on).map((row) => row.command), ["ssh cage-default"]);
  const without = commandRows({ ...on, include: { present: false } });
  assert.equal(without[0].command, "cage ssh default");
  assert.match(without[0].caption, /不改动/);
  assert.deepEqual(commandRows({ ...on, enabled: false }), []);
});

test("Herdr and attach rows wait for their capabilities", () => {
  assert.equal(commandRows(on).some((row) => row.id === "herdr" || row.id === "attach"), false);
  const later = commandRows({ ...on, herdr: true, commands: { ...on.commands, attach: "cage attach default" } });
  assert.deepEqual(later.map((row) => row.command), ["ssh cage-default", "hc machine add cage-default", "cage attach default"]);
});

test("include row and dialog", () => {
  assert.equal(includeRow(on).state, "present");
  assert.equal(includeRow({ ...on, include: { present: false } }).state, "absent");
  assert.match(includeRow({ ...on, include: { error: "无法读取 ~/.ssh/config：permission denied" } }).text, /^无法读取 ~\/\.ssh\/config：permission denied。你仍可以用 cage ssh default 连接。$/);
  assert.equal(includeRow({ ...on, enabled: false }), null);
  const dialog = includeDialog({ line: "Include ~/.ssh/super-cage/config", display_path: "~/.ssh/config" });
  assert.equal(dialog.title, "让 ssh 认识这个环境");
  assert.equal(dialog.line, "Include ~/.ssh/super-cage/config");
  assert.deepEqual(Object.values(dialog.actions), ["取消", "不写入，只用 cage ssh", "写入并开启"]);
});

test("relative times", () => {
  const now = Date.parse("2026-11-21T12:00:00Z");
  assert.equal(relativeTime(undefined, now), "从未使用");
  assert.equal(relativeTime("2026-11-21T11:59:40Z", now), "最近使用 刚刚");
  assert.equal(relativeTime("2026-11-21T11:57:00Z", now), "最近使用 3 分钟前");
  assert.equal(relativeTime("2026-11-21T09:00:00Z", now), "最近使用 3 小时前");
  assert.equal(relativeTime("2026-11-18T12:00:00Z", now), "最近使用 3 天前");
  assert.match(relativeTime("2026-01-02T12:00:00Z", now), /^最近使用 2026-01-0[12]$/);
});

test("fingerprints, names and order", () => {
  assert.equal(shortFingerprint("SHA256:o/+qFR975llb1cjqdgUG0z2vIkIUeG0n2P6A+7yb6/A"), "SHA256:o/+qFR…b6/A");
  assert.equal(shortFingerprint(""), "");
  assert.equal(validDeviceName("MacBook Pro"), true);
  assert.equal(validDeviceName(" lead"), false);
  assert.equal(validDeviceName("a\nb"), false);
  assert.equal(validDeviceName("名".repeat(64)), true);
  assert.equal(validDeviceName("名".repeat(65)), false);
  const devices = sortedDevices([
    { id: "r", revoked: true, last_used: "2026-11-21T11:00:00Z" },
    { id: "o", last_used: "2026-11-20T11:00:00Z" },
    { id: "c", current: true },
    { id: "n", last_used: "2026-11-21T10:00:00Z" },
  ]);
  assert.deepEqual(devices.map((d) => d.id), ["c", "n", "o", "r"]);
  assert.equal(revokeConfirm({ name: "MacBook Pro" }).title, "撤销「MacBook Pro」？");
  assert.equal(revokeConfirm({ name: "x" }).danger, true);
});

test("load errors", () => {
  assert.equal(loadError({ status: 423 }).locked, true);
  assert.match(loadError({ status: 404 }).text, /需要更新 Super Cage/);
  assert.match(loadError({ status: 403 }).text, /这台电脑/);
  assert.equal(loadError({ message: "网络中断" }).text, "网络中断");
});
