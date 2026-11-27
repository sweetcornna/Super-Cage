import assert from "node:assert/strict";
import test from "node:test";
import { readTerminalFocus, terminalFocusAction, terminalFocusFor, writeTerminalFocus } from "./terminal-focus.mjs";

const id = "231e52f40454ccdb";
const otherID = "492e106a9c9a41ed";
const conversation = "fe161328-67e8-4e3d-9500-cfe7cfe8a477";

test("a saved terminal prefers its existing PTY or the replacement for the same conversation", () => {
  const focus = { terminal: id, conversation };
  const exact = { id, kind: "claude", cli_session_id: conversation };
  const unrelated = { id: otherID, kind: "claude" };
  const bound = { ...unrelated, cli_session_id: conversation };
  assert.deepEqual(terminalFocusAction(focus, [exact, bound]), { terminal: exact });
  assert.deepEqual(terminalFocusAction(focus, [bound]), { terminal: bound });
  assert.deepEqual(terminalFocusAction(focus, [unrelated]), { recover: conversation });
  assert.deepEqual(terminalFocusAction({ terminal: id }, [unrelated]), { missing: true });
});

test("only confirmed terminal and conversation IDs survive a page restart", (t) => {
  const values = new Map();
  const original = globalThis.localStorage;
  globalThis.localStorage = { getItem: key => values.get(key), setItem: (key, value) => values.set(key, value), removeItem: key => values.delete(key) };
  t.after(() => { if (original === undefined) delete globalThis.localStorage; else globalThis.localStorage = original; });
  const tab = { kind: "terminal", sessionId: id, cli_session_id: conversation, terminalKind: "claude", title: "private", command: "prompt" };
  const focus = terminalFocusFor(tab);
  writeTerminalFocus("local", focus);
  assert.deepEqual(readTerminalFocus("local"), { terminal: id, conversation });
  assert.equal(readTerminalFocus("other VM"), null);
  assert.equal(terminalFocusFor({ ...tab, pending: true }), null);
  assert.equal(terminalFocusFor({ ...tab, terminalKind: "claude-login" }), null);
  writeTerminalFocus("local", null);
  assert.equal(readTerminalFocus("local"), null);
});
