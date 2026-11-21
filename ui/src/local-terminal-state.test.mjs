import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import {
  APP_NAMES, BUILT_IN, BUILT_IN_NAME, MENU_LABEL, OPEN_PATH, automationNotice, chooserOptions, copyCommand,
  launchFailure, menuItem, missingChoice, normalizeSettings, openRequest, settingsChanged,
} from "./local-terminal-state.mjs";

const apps = [
  { id: "ghostty", name: "Ghostty", automation: true, hosts: true, installed: false },
  { id: "iterm2", name: "iTerm2", automation: true, hosts: true, installed: true },
  { id: "terminal", name: "终端", automation: false, hosts: true, installed: true },
  { id: "warp", name: "Warp", automation: false, hosts: true, installed: false },
  { id: "wezterm", name: "WezTerm", automation: false, hosts: true, installed: true },
  { id: "kitty", name: "kitty", automation: false, hosts: true, installed: true },
  { id: "alacritty", name: "Alacritty", automation: false, hosts: true, installed: false },
  { id: "herdr", name: "Herdr", automation: false, hosts: false, installed: true, note: "Herdr 通过本机终端的 SSH 入口连接，稍后的版本提供" },
];
const desktop = { desktop_gateway: true, connection_id: "c".repeat(64), mode: "local" };

test("the names match the Go table of applications", () => {
  const source = readFileSync(new URL("../../internal/localterm/apps.go", import.meta.url), "utf8");
  const table = Object.fromEntries([...source.matchAll(/\{ID: "([a-z0-9]+)", Name: "([^"]+)"/g)].map(match => [match[1], match[2]]));
  assert.deepEqual(table, { ...APP_NAMES });
});

test("settings default to the web terminal's behaviour and the built-in terminal", () => {
  assert.deepEqual(normalizeSettings(undefined), { app: BUILT_IN, image_upload: true, clipboard_write: true });
  assert.deepEqual(normalizeSettings({ app: "kitty", image_upload: false }), { app: "kitty", image_upload: false, clipboard_write: true });
  assert.equal(settingsChanged({ app: "kitty" }, { app: "kitty", image_upload: true }), false);
  assert.equal(settingsChanged({ app: "kitty" }, { app: "kitty", clipboard_write: false }), true);
});

test("the chooser lists found applications, then the built-in terminal, then the rest", () => {
  const options = chooserOptions(apps, "wezterm");
  assert.deepEqual(options.map(option => option.name), ["iTerm2", "kitty", "WezTerm", "终端", BUILT_IN_NAME, "Herdr", "Alacritty", "Ghostty", "Warp"]);
  assert.deepEqual(options.map(option => option.group), ["ready", "ready", "ready", "ready", "built-in", "waiting", "missing", "missing", "missing"]);
  assert.deepEqual(options.filter(option => option.checked).map(option => option.id), ["wezterm"]);
  assert.ok(options.filter(option => !option.selectable).every(option => option.chip));
  assert.equal(options.find(option => option.id === "ghostty").chip, "未安装");
  assert.match(options.find(option => option.id === "iterm2").note, /允许 Super Cage 控制 iTerm2/);
  assert.match(options.find(option => option.id === "herdr").note, /SSH 入口/);
  assert.equal(chooserOptions(null)[0].id, BUILT_IN);
  assert.equal(chooserOptions(apps)[4].checked, true);
});

test("a saved application that is gone is named", () => {
  assert.equal(missingChoice({ app: "ghostty" }, apps), "Ghostty");
  assert.equal(missingChoice({ app: "kitty" }, apps), "");
  assert.equal(missingChoice({ app: BUILT_IN }, apps), "");
  assert.equal(missingChoice({ app: "kitty" }, null), "");
});

test("the menu item is hidden for the built-in terminal and outside the desktop", () => {
  const terminal = { kind: "terminal", id: "0123456789abcdef" };
  assert.equal(menuItem({ settings: { app: BUILT_IN }, target: terminal, context: desktop }).visible, false);
  assert.equal(menuItem({ settings: { app: "kitty" }, target: terminal, context: { ...desktop, desktop_gateway: false } }).visible, false);
  const item = menuItem({ settings: { app: "kitty" }, target: terminal, context: desktop });
  assert.deepEqual(item, { visible: true, label: MENU_LABEL, detail: "kitty", icon: "square-arrow-out-up-right", shortcut: "⌘⇧O", enabled: true, reason: "" });
  // Without detection the name still comes from the table.
  assert.equal(menuItem({ settings: { app: "iterm2" }, target: { kind: "project", path: "/work" }, context: desktop }).shortcut, "");
  assert.equal(menuItem({ settings: { app: "iterm2" }, apps: null, target: terminal, context: desktop }).detail, "iTerm2");
});

test("the menu item says why it cannot open", () => {
  const settings = { app: "kitty" };
  const reason = (target, context = desktop, environment) => menuItem({ settings, target, context, environment }).reason;
  assert.equal(reason({ kind: "terminal", id: "0123456789abcdef", closed: true }), "终端已结束");
  assert.equal(reason({ kind: "terminal", id: "0123456789abcdef" }, { ...desktop, mode: "none" }), "先选择一个工作环境");
  assert.equal(reason({ kind: "terminal", id: "0123456789abcdef" }, desktop, { running: false }), "环境已停止");
  assert.equal(reason({ kind: "terminal", id: "0123456789abcdef" }, desktop, { locked: true }), "Super Cage 已锁定");
  assert.equal(reason({ kind: "project", path: "relative" }), "无法识别这个项目");
  assert.equal(reason({ kind: "session", id: "x;rm" }), "无法识别这个会话");
  assert.equal(reason({ kind: "terminal", id: "../x" }), "无法识别这个终端");
});

test("the open request names the current connection and carries ids only", () => {
  assert.deepEqual(openRequest({ kind: "terminal", id: "0123456789abcdef" }, desktop), {
    path: OPEN_PATH, body: { terminal_id: "0123456789abcdef" }, headers: { "X-Cage-Connection": desktop.connection_id },
  });
  assert.deepEqual(openRequest({ kind: "project", path: "/work/my app", run: "claude", claudeAccount: "work", codexAccount: "x" }, desktop).body,
    { project: "/work/my app", kind: "claude", claude_account_id: "work" });
  assert.deepEqual(openRequest({ kind: "session", id: "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0" }, desktop).body, { session_id: "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0" });
  assert.throws(() => openRequest({ kind: "terminal", id: "nope" }, desktop));
});

test("the copied command is quoted for any shell", () => {
  assert.equal(copyCommand({ kind: "terminal", id: "0123456789abcdef" }), "cage attach current --terminal 0123456789abcdef");
  assert.equal(copyCommand({ kind: "project", path: "/work/it's mine", run: "codex", codexAccount: "acct-1" }),
    "cage attach current --new codex --project '/work/it'\\''s mine' --codex-account acct-1");
});

test("messages follow the design's wording", () => {
  assert.equal(launchFailure("Ghostty", "应用没有响应。"), "没能打开 Ghostty：应用没有响应。可在设置 → 本机终端应用里换一个。");
  assert.equal(automationNotice({ name: "Ghostty", automation: true }).title, "Super Cage 需要控制 Ghostty 来打开窗口");
  assert.deepEqual([automationNotice({ name: "Ghostty", automation: true }).cancel, automationNotice({ name: "Ghostty", automation: true }).confirm], ["改用其他应用", "继续"]);
  assert.equal(automationNotice({ name: "Ghostty", automation: true }, true), null);
  assert.equal(automationNotice({ name: "kitty", automation: false }), null);
});

test("a project path with any control character is refused, by code point", () => {
  for (const path of ["/w/a\u001b[201~b", "/w/a\u007fb", "/w/a\u0085b", "/w/a\u009bb", "/w/a\u000fb", "/w/a\tb", "/w/a\ud800b"]) {
    assert.throws(() => copyCommand({ kind: "project", path }), /无法识别这个项目/, JSON.stringify(path));
  }
  assert.equal(copyCommand({ kind: "project", path: "/w/项目 一" }), "cage attach current --new shell --project '/w/项目 一'");
});
