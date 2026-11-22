import assert from "node:assert/strict";
import { test } from "node:test";
import { sessionKey } from "./session-order.mjs";
import { canContinueInTerminal, canStopFromPane, codexCapabilities, codexMenuLabel, codexSessionAccountLabel, codexSessionAction,
  codexLaunchRefused, endedNote, terminalLastLine, isCodexSession, isCodexTab, mixedSessionRows, archiveCopy, codexStateWord, sessionViewPath, ownerState, providerChip, providerLabel, purgeCopy, selectionProvider, sessionListPath, sessionOwner,
  shouldGateStartup, showSessionAccount } from "./session-provider.mjs";

const codex = (id, owner = "", extra = {}) => ({ id, provider: "codex", owner, title: id, ...extra });
const claude = (id, extra = {}) => ({ id, title: id, ...extra });

test("the list asks for both providers and keeps the hold parameters", () => {
  assert.equal(sessionListPath(), "/api/cli/sessions?providers=claude%2Ccodex");
  assert.equal(sessionListPath({ archived: true, after: "ab", wait: 25 }), "/api/cli/sessions?providers=claude%2Ccodex&archived=true&after=ab&wait=25");
  assert.equal(sessionListPath({ wait: 25 }), "/api/cli/sessions?providers=claude%2Ccodex", "wait needs a revision");
});

test("provider marks", () => {
  assert.equal(isCodexSession(codex("a")), true);
  assert.equal(isCodexSession(claude("b")), false);
  assert.equal(isCodexTab({ cli_session_provider: "codex" }), true);
  assert.equal(isCodexTab({ cli_session_id: "x" }), false);
  assert.deepEqual(providerChip(codex("a")), { provider: "codex", label: "Codex" });
  assert.deepEqual(providerChip(claude("b")), { provider: "claude", label: "Claude" });
  assert.equal(providerLabel(undefined), "Claude");
});

test("owner states map to labels and unknown owners fail closed", () => {
  assert.equal(sessionOwner(claude("c", { owner: "phone" })), "");
  assert.equal(sessionOwner(codex("a", "surprise")), "external");
  assert.deepEqual(ownerState(codex("a")), { owner: "", label: "", tooltip: "", muted: false, live: false });
  assert.equal(ownerState(codex("a", "desktop", { active: true })).live, true);
  assert.equal(ownerState(codex("a", "phone")).label, "手机");
  assert.equal(ownerState(codex("a", "phone")).muted, true);
  assert.equal(ownerState(codex("a", "external")).tooltip, "在其他 Codex 进程中打开");
});

test("clicking a Codex row focuses, opens or shows it read-only", () => {
  const tabs = [{ id: "t1", cli_session_id: "live" }, { id: "t2", cli_session_id: "ended", closed: true }];
  assert.deepEqual(codexSessionAction(codex("live", "desktop"), tabs), { action: "focus", tabId: "t1", reason: "" });
  assert.equal(codexSessionAction(codex("ended"), tabs).action, "open");
  assert.equal(codexSessionAction(codex("other-window", "desktop"), tabs).action, "open", "the server reuses its terminal");
  const phone = codexSessionAction(codex("p", "phone"), tabs);
  assert.equal(phone.action, "readonly");
  assert.match(phone.reason, /手机/);
  assert.equal(codexSessionAction(codex("x", "external"), tabs).action, "readonly");
  assert.equal(canContinueInTerminal(codex("p", "phone")), false);
  assert.equal(canContinueInTerminal(codex("p", "")), true);
  assert.equal(canStopFromPane(codex("p", "phone", { turns: [{ status: "running" }] })), true);
  assert.equal(canStopFromPane(codex("p", "external", { turns: [{ status: "running" }] })), false);
  assert.equal(canStopFromPane(codex("p", "phone", { turns: [{ status: "completed" }] })), false);
});

test("mixed rows keep one order, attach tabs by session and never disable Codex rows", () => {
  const sessions = [claude("one", { pinned: true }), codex("two", "phone", { running: true, active: true }), claude("three", { running: true }), codex("four")];
  const tabs = [{ id: "terminal:four", cli_session_id: "four", cli_session_provider: "codex", kind: "terminal" }];
  const rows = mixedSessionRows(tabs, sessions, {}, undefined, new Map());
  assert.deepEqual(rows.map(sessionKey), ["session:two", "session:one", "session:three", "session:four"]);
  const by = Object.fromEntries(rows.map((row) => [row.cli_session_id, row]));
  assert.equal(by.two.sessionProvider, "codex");
  assert.equal(by.two.sessionRunningElsewhere, false);
  assert.equal(by.two.sessionAction, "readonly");
  assert.equal(by.three.sessionProvider, "claude");
  assert.equal(by.three.sessionRunningElsewhere, true, "Claude rows keep today's rule");
  assert.equal(by.four.id, "terminal:four", "the live Codex tab is the row");
  assert.equal(by.four.sessionAction, "focus");
  const manual = ["session:four", "session:three", "session:two", "session:one"];
  assert.deepEqual(mixedSessionRows(tabs, sessions, {}, manual, new Map()).map(sessionKey), manual);
});

test("bound Codex terminals show at once and have no Claude account header", () => {
  assert.equal(shouldGateStartup({ cli_session_id: "a" }), true);
  assert.equal(shouldGateStartup({ cli_session_id: "a", cli_session_provider: "codex" }), false);
  assert.equal(shouldGateStartup({ cli_session_id: "a", closed: true }), false);
  assert.equal(showSessionAccount({ cli_session_id: "a" }), true);
  assert.equal(showSessionAccount({ cli_session_id: "a", cli_session_provider: "codex" }), false);
  assert.equal(showSessionAccount({}), false);
});

test("account labels, purge copy, capabilities and the menu entry", () => {
  const accounts = [{ id: "acc", label: "工作" }, { id: "mail", email: "m@example.test" }];
  assert.equal(codexSessionAccountLabel(codex("a", "", { account_label: "服务器名" }), accounts), "服务器名");
  assert.equal(codexSessionAccountLabel(codex("a", "", { codex_account_id: "mail" }), accounts), "m@example.test");
  assert.equal(codexSessionAccountLabel(codex("a", "", { codex_account_id: "gone" }), accounts), "Codex 账号");
  assert.equal(codexSessionAccountLabel(codex("a"), accounts), "默认登录");
  assert.equal(purgeCopy("codex", "修复登录"), "永久删除“修复登录”及其 Codex 原始对话？此操作无法撤销。");
  assert.equal(purgeCopy("", ""), "永久删除“新会话”及其 Claude Code 原始对话和子代理记录？此操作无法撤销。");
  assert.match(purgeCopy("codex", "", 3), /所选的 3 个会话/);
  // A bulk confirmation keeps its wording for one session; Claude only reads as before.
  assert.equal(purgeCopy("", "", 1, true), "永久删除所选的 1 个会话及其 Claude Code 原始对话和子代理记录？此操作无法撤销。");
  assert.equal(purgeCopy("mixed", "", 2), "永久删除所选的 2 个会话及其原始对话？此操作无法撤销。");
  assert.equal(purgeCopy("codex", "", 2), "永久删除所选的 2 个会话及其 Codex 原始对话？此操作无法撤销。");
  assert.equal(selectionProvider([claude("a"), claude("b")]), "");
  assert.equal(selectionProvider([codex("a"), codex("b")]), "codex");
  assert.equal(selectionProvider([codex("a"), claude("b")]), "mixed");
  assert.deepEqual(codexCapabilities({ sessions: [] }), { known: false, sessions: false, cli: null });
  const caps = codexCapabilities({ capabilities: { codex_sessions: true, codex_cli: { version: "0.171.0" } } });
  assert.deepEqual(caps, { known: true, sessions: true, cli: { version: "0.171.0" } });
  assert.equal(codexMenuLabel(caps), "Codex 会话");
  assert.equal(codexMenuLabel(codexCapabilities({})), "Codex 终端");
});

test("the view path carries the hold only with a revision", () => {
  assert.equal(sessionViewPath("a b"), "/api/cli/sessions/a%20b");
  assert.equal(sessionViewPath("x", { after: 0 }), "/api/cli/sessions/x?after=0");
  assert.equal(sessionViewPath("x", { after: 7, epoch: "g1", wait: 25 }), "/api/cli/sessions/x?after=7&epoch=g1&wait=25");
  assert.equal(sessionViewPath("x", { wait: 25 }), "/api/cli/sessions/x", "no wait without after");
});

test("archive copy names what stops", () => {
  assert.equal(archiveCopy(claude("a")).body, "归档会关闭关联终端。历史将保留，可从设置中的已归档会话恢复。");
  const closing = archiveCopy(codex("a", "desktop"), { closing: true });
  assert.equal(closing.title, "关闭并归档这段会话？");
  assert.match(closing.body, /^关闭后这段会话会归档/);
  assert.match(archiveCopy(codex("a", "phone")).body, /停止手机上/);
  assert.match(archiveCopy(codex("a", "external")).body, /不会停止其他进程/);
  assert.equal(archiveCopy(codex("a", "")).body, "历史将保留，可从设置中的已归档会话恢复。");
  assert.match(archiveCopy(codex("a", "surprise")).body, /不会停止其他进程/, "unknown owners read as external");
});

test("a refused Codex launch is not an ordinary close", () => {
  const ended = (fields) => ({ cli_session_provider: "codex", closed: true, exit_code: 75, ...fields });
  assert.equal(codexLaunchRefused(ended()), true);
  assert.equal(codexLaunchRefused(ended({ exit_code: 0 })), false, "Codex quit on its own");
  assert.equal(codexLaunchRefused(ended({ exit_code: undefined })), false, "an older server sends no status");
  assert.equal(codexLaunchRefused(ended({ closed: false })), false, "only an ended terminal");
  assert.equal(codexLaunchRefused(ended({ cli_session_provider: "" })), false, "a Claude or plain terminal ending with 75 is its own business");
  assert.equal(endedNote(ended(), "会话账号已变化，请重新读取会话列表"), "未能打开：会话账号已变化，请重新读取会话列表");
  assert.match(endedNote(ended()), /^未能打开：电脑拒绝了这次 Codex 启动/);
  assert.equal(endedNote(ended({ exit_code: 0 })), "终端会话已结束。可点击项目右侧的终端图标新建终端。");
  assert.equal(codexStateWord(codex("a", "desktop"), { closed: true, refused: true }), "未能打开");
  const lines = ["", "$ codex", "会话账号已变化，请重新读取会话列表", "  ", ""];
  const buffer = { length: lines.length, getLine: (index) => ({ translateToString: () => lines[index] }) };
  assert.equal(terminalLastLine(buffer), "会话账号已变化，请重新读取会话列表", "the last line with text");
  assert.equal(terminalLastLine({ length: 0, getLine: () => undefined }), "");
  assert.equal(terminalLastLine(undefined), "");
});

test("state words", () => {
  assert.equal(codexStateWord(codex("a", "desktop", { active: true })), "运行中");
  assert.equal(codexStateWord(codex("a", "desktop")), "空闲");
  assert.equal(codexStateWord(codex("a", "phone", { active: true })), "手机正在运行");
  assert.equal(codexStateWord(codex("a", "external")), "在其他进程中打开");
  assert.equal(codexStateWord(codex("a", "desktop"), { closed: true }), "已结束");
});
