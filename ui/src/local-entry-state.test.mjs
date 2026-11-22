import assert from "node:assert/strict";
import test from "node:test";
import {
  commandRows,
  connectionSummary,
  entryStateLine,
  includeDialog,
  includeRow,
  keptRows,
  endKeptConfirm,
  loadError,
  relativeTime,
  revokeConfirm,
  shortFingerprint,
  sortedDevices,
  unfilteredNote,
  unreachedNote,
  validDeviceName,
} from "./local-entry-state.mjs";

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
  assert.deepEqual(commandRows(on).filter((row) => !row.later).map((row) => row.command), ["ssh cage-default"]);
  const without = commandRows({ ...on, include: { present: false } });
  assert.equal(without[0].command, "cage ssh default");
  assert.match(without[0].caption, /不改动/);
  assert.deepEqual(commandRows({ ...on, enabled: false }), []);
});

test("Herdr and attach rows wait for their capabilities", () => {
  assert.equal(commandRows(on).some((row) => row.id === "attach"), false);
  // Herdr is named, as 稍后提供, until the server offers it.
  assert.deepEqual(commandRows(on).filter((row) => row.later).map((row) => [row.id, row.name, row.command]), [["herdr", "Herdr", undefined]]);
  const later = commandRows({ ...on, herdr: true, commands: { ...on.commands, attach: "cage attach" } });
  assert.deepEqual(later.map((row) => row.command), ["ssh cage-default", "cage attach", "hc machine add cage-default"]);
  assert.equal(later.some((row) => row.later), false);
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
  assert.equal(dialog.block, "");
  // The confirmation shows the host name and the whole block it brings in.
  const shown = includeDialog({ line: "Include ~/.ssh/super-cage/config", alias: "cage-lab1", block: "Host cage-lab1\n  User cage\n" });
  assert.equal(shown.aliasText, "之后用 ssh cage-lab1 连接。这一行引入的配置：");
  assert.equal(shown.block, "Host cage-lab1\n  User cage\n");
  // A file Super Cage must not change, or a taken host name, is said as it
  // is, with the line to add by hand.
  const readOnly = includeRow({ ...on, include: { error: "~/.ssh/config 不可写（只读、带不可更改标记或在只读的文件系统上），Super Cage 不会修改它：要直接用 ssh 连接，请自己在它的第一行加入 Include ~/.ssh/super-cage/config" } });
  assert.match(readOnly.text, /^~\/\.ssh\/config 不可写.*Super Cage 不会修改它：.*Include ~\/\.ssh\/super-cage\/config。你仍可以用 cage ssh default 连接。$/);
  // A BOM is warned about before anything is written.
  assert.equal(includeDialog({ warning: "~/.ssh/config 以 BOM（字节顺序标记）开头" }).warning, "~/.ssh/config 以 BOM（字节顺序标记）开头");
  assert.equal(includeDialog({}).warning, "");
});

test("the page says exactly what is filtered", () => {
  const note = unfilteredNote();
  for (const word of ["ssh -T", "scp", "sftp", "rsync", "不经过滤", "ssh -t -s", "和 cage attach 一样的过滤", "拖入的图片不上传"]) assert.ok(note.includes(word), word);
  assert.equal(note.includes("会被拒绝"), false);
});

test("a revocation that did not reach another environment says so", () => {
  assert.equal(unreachedNote([]), "");
  assert.equal(unreachedNote(undefined), "");
  assert.equal(unreachedNote(["研究机"]), "另一个环境暂时没有响应，已撤销的钥匙会在它下次连接时被拒绝（研究机）");
});

test("kept terminals are listed with their limit and 结束", () => {
  assert.deepEqual(keptRows(on).rows, []);
  const kept = keptRows({ ...on, kept_limit: 4, kept: [{ id: "a", title: "终端 3", device: "MacBook Pro" }, { id: "b", title: "终端 4", device: "MacBook Pro", ended: true }] });
  assert.match(kept.help, /最多 4 个/);
  assert.deepEqual(kept.rows.map((row) => row.detail), ["来自 MacBook Pro", "来自 MacBook Pro · 已结束"]);
  assert.equal(endKeptConfirm(kept.rows[0]).title, "结束「终端 3」？");
  assert.equal(endKeptConfirm(kept.rows[0]).danger, true);
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
