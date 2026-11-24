import test from "node:test";
import assert from "node:assert/strict";
import { loginReturnTerminal, orphanLoginTerminals } from "./login-terminals.mjs";

const login = { id: "terminal:a", kind: "terminal", terminalKind: "agent-login", workspace: "" };
const shell = { id: "terminal:b", kind: "terminal", terminalKind: "shell", workspace: "" };
const inProject = { id: "terminal:c", kind: "terminal", terminalKind: "claude-login", workspace: "/home/cage/projects/quant" };
const page = { id: "tab-1", kind: "web", login: true };

test("login terminals outside every project stay listed", () => {
  const tabs = [login, shell, inProject, page];
  assert.deepEqual(orphanLoginTerminals(tabs, ["/home/cage/projects/quant"], []).map((tab) => tab.id), ["terminal:a"]);
  assert.deepEqual(orphanLoginTerminals(tabs, [], []).map((tab) => tab.id), ["terminal:a", "terminal:c"]);
});

test("a project group's extra folders count as the project", () => {
  const extra = { ...inProject, workspace: "/home/cage/data" };
  const groups = [{ workspace: "/home/cage/projects/quant", folders: ["/home/cage/projects/quant", "/home/cage/data"] }];
  assert.deepEqual(orphanLoginTerminals([extra], ["/home/cage/projects/quant"], groups), []);
});

test("the authorization page leads back to its open login terminal", () => {
  const returns = { "tab-1": "terminal:a" };
  assert.equal(loginReturnTerminal(returns, "tab-1", [login, page]), login);
  assert.equal(loginReturnTerminal(returns, "tab-2", [login, page]), null);
  assert.equal(loginReturnTerminal(returns, "tab-1", [{ ...login, closed: true }, page]), null);
  assert.equal(loginReturnTerminal(returns, "tab-1", [page]), null);
  assert.equal(loginReturnTerminal(undefined, "tab-1", [login]), null);
});
