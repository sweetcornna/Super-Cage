import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import {
  MENU_LABEL, OPEN_PATH, automationNotice, copyCommand, launchFailure, menuItem, normalizeSettings, openRequest, openerLine, settingsChanged,
} from "./local-terminal-state.mjs";

const desktop = { desktop_gateway: true, connection_id: "c".repeat(64), mode: "local" };
const ghostty = { id: "ghostty", name: "Ghostty", automation: true };
const terminal = { id: "terminal", name: "系统终端", automation: false };

test("the Go table has Ghostty first and the system Terminal second, and nothing else", () => {
  const source = readFileSync(new URL("../../internal/localterm/apps.go", import.meta.url), "utf8");
  const table = [...source.matchAll(/\{ID: "([a-z0-9]+)", Name: "([^"]+)"[^\n]*?(Automation: true)?,?\s*$/gm)].map(match => [match[1], match[2], Boolean(match[3])]);
  assert.deepEqual(table.map(row => row.slice(0, 2)), [["ghostty", ghostty.name], ["terminal", terminal.name]]);
  // The notice before the first open depends on what the server says about
  // Automation: Ghostty is asked about by macOS, the system Terminal is not.
  assert.deepEqual(table.map(row => row[2]), [ghostty.automation, terminal.automation]);
});

test("settings are the two switches, on by default as in the web terminal", () => {
  assert.deepEqual(normalizeSettings(undefined), { image_upload: true, clipboard_write: true });
  assert.deepEqual(normalizeSettings({ image_upload: false }), { image_upload: false, clipboard_write: true });
  // The application an earlier build kept is not read, and never sent back.
  assert.deepEqual(normalizeSettings({ app: "kitty", image_upload: false, clipboard_write: false }), { image_upload: false, clipboard_write: false });
  assert.equal(settingsChanged({ app: "kitty" }, { app: "iterm2", image_upload: true }), false);
  assert.equal(settingsChanged({}, { clipboard_write: false }), true);
  assert.equal(settingsChanged({ image_upload: true }, { image_upload: false }), true);
});

test("one line says which application opens, with no choice offered", () => {
  assert.equal(openerLine(ghostty), "用 Ghostty 打开");
  assert.equal(openerLine(terminal), "用系统终端打开；安装 Ghostty 后自动改用 Ghostty");
  assert.match(openerLine(terminal), new RegExp("^用" + terminal.name + "打开"));
  assert.match(openerLine(null), /找不到 Ghostty 或系统终端/);
  assert.match(openerLine(ghostty, false), /暂不支持/);
  for (const line of [openerLine(ghostty), openerLine(terminal), openerLine(null), openerLine(null, false)]) assert.doesNotMatch(line, /选择|选好|换一个|Herdr|iTerm|kitty/);
});

test("the menu item shows on a computer that can open, only in the desktop app", () => {
  const target = { kind: "terminal", id: "0123456789abcdef" };
  assert.equal(menuItem({ target, context: { ...desktop, desktop_gateway: false } }).visible, false);
  assert.equal(menuItem({ supported: false, target, context: desktop }).visible, false);
  const item = menuItem({ supported: true, target, context: desktop });
  assert.deepEqual(item, { visible: true, label: MENU_LABEL, icon: "square-arrow-out-up-right", shortcut: "⌘⇧O", enabled: true, reason: "" });
  assert.equal(menuItem({ supported: true, target: { kind: "project", path: "/work" }, context: desktop }).shortcut, "");
  // Nothing is stored for the menu to read: no application, no choice.
  assert.equal(menuItem({ target, context: desktop }).enabled, true);
});

test("the menu item says why it cannot open", () => {
  const reason = (target, context = desktop, environment) => menuItem({ supported: true, target, context, environment }).reason;
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
  assert.equal(copyCommand({ kind: "terminal", id: "0123456789abcdef" }), "cage attach --terminal 0123456789abcdef current");
  assert.equal(copyCommand({ kind: "project", path: "/work/it's mine", run: "codex", codexAccount: "acct-1" }),
    "cage attach --new codex --project '/work/it'\\''s mine' --codex-account acct-1 current");
});

test("messages follow the design's wording", () => {
  assert.equal(launchFailure("Ghostty", "应用没有响应。"), "没能打开 Ghostty：应用没有响应。");
  assert.equal(launchFailure("系统终端", ""), "没能打开 系统终端：未知原因。");
  const notice = automationNotice(ghostty);
  assert.equal(notice.title, "Super Cage 需要控制 Ghostty 来打开窗口");
  assert.match(notice.body, /^macOS 接下来会询问是否允许 Super Cage 控制 Ghostty。.*如果不允许，窗口不会打开/);
  assert.doesNotMatch(notice.body + launchFailure("Ghostty", "x"), /换一个|选择/);
  assert.deepEqual([notice.cancel, notice.confirm], ["取消", "继续"]);
  // Asked once: after the owner continued, and for the system Terminal, never.
  assert.equal(automationNotice(ghostty, true), null);
  assert.equal(automationNotice(terminal), null);
  assert.equal(automationNotice(null), null);
});

test("a project path with any control character is refused, by code point", () => {
  for (const path of ["/w/a\u001b[201~b", "/w/a\u007fb", "/w/a\u0085b", "/w/a\u009bb", "/w/a\u000fb", "/w/a\tb", "/w/a\ud800b"]) {
    assert.throws(() => copyCommand({ kind: "project", path }), /无法识别这个项目/, JSON.stringify(path));
  }
  assert.equal(copyCommand({ kind: "project", path: "/w/项目 一" }), "cage attach --new shell --project '/w/项目 一' current");
});
