import assert from "node:assert/strict";
import test from "node:test";
import {
  isPendingTerminal, loginTerminalKind, markPendingTab, pendingTerminalTab, renamePendingLayout, settlePendingTab,
} from "./terminal-pending.mjs";

test("a placeholder is a terminal tab that names its request", () => {
  const tab = pendingTerminalTab("/work/app", "claude");
  assert.equal(tab.kind, "terminal");
  assert.equal(tab.pending, true);
  assert.equal(tab.workspace, "/work/app");
  assert.equal(tab.terminalKind, "claude");
  assert.equal(tab.title, "Claude");
  assert.equal(pendingTerminalTab("/w", "codex").title, "Codex");
  assert.equal(pendingTerminalTab("/w", "shell").title, "终端");
  assert.ok(isPendingTerminal(tab.id));
  assert.notEqual(pendingTerminalTab("/w", "shell").id, pendingTerminalTab("/w", "shell").id);
  // Server ids are hex tokens; the prefix cannot collide with them.
  assert.equal(isPendingTerminal("terminal:0123456789abcdef"), false);
  assert.equal(isPendingTerminal(undefined), false);
});

test("login terminals keep their own progress in settings", () => {
  assert.equal(loginTerminalKind("claude-login"), true);
  assert.equal(loginTerminalKind("codex-login"), true);
  assert.equal(loginTerminalKind("claude"), false);
  assert.equal(loginTerminalKind("shell"), false);
});

test("the terminal replaces its placeholder in place", () => {
  const pending = pendingTerminalTab("/w", "shell");
  const web = { id: "w1", kind: "web" }, other = { id: "terminal:aa", kind: "terminal" };
  const tab = { id: "terminal:bb", kind: "terminal", sessionId: "bb" };
  assert.deepEqual(settlePendingTab([web, pending, other], pending.id, tab), [web, tab, other]);
  // A poll that already added the terminal leaves one copy, where the placeholder was.
  assert.deepEqual(settlePendingTab([web, pending, other, tab], pending.id, tab), [web, tab, other]);
  // The same work session in another tab is replaced too.
  const session = { ...tab, cli_session_id: "s1" };
  assert.deepEqual(settlePendingTab([pending, { id: "terminal:old", cli_session_id: "s1" }], pending.id, session), [session]);
  // Without its placeholder the terminal is added at the end.
  assert.deepEqual(settlePendingTab([web], pending.id, tab), [web, tab]);
});

test("errors are kept on the placeholder and cleared for a retry", () => {
  const pending = pendingTerminalTab("/w", "shell");
  const failed = markPendingTab([pending], pending.id, "请先通过打开项目添加该文件夹");
  assert.equal(failed[0].error, "请先通过打开项目添加该文件夹");
  assert.equal(markPendingTab(failed, pending.id, "")[0].error, "");
  assert.equal(markPendingTab([pending], "terminal:other", "x")[0], pending);
});

test("the layout follows the placeholder only where it still points at it", () => {
  const id = "terminal:pending-x";
  assert.deepEqual(renamePendingLayout({ active: id }, id, "terminal:bb"), { active: "terminal:bb" });
  assert.deepEqual(renamePendingLayout({ active: "w1", other: id, side: "left" }, id, "terminal:bb"),
    { active: "w1", other: "terminal:bb", side: "left" });
  const moved = { active: "w1" };
  assert.equal(renamePendingLayout(moved, id, "terminal:bb"), moved);
});
