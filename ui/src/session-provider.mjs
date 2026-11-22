// Pure state for Codex sessions beside Claude sessions in the workbench. The
// server contract is internal/workbench (codex_sessions.go, mobile_cli_http.go):
// rows carry provider "codex", owner and codex_account_id; bound terminals
// carry cli_session_provider "codex".
import { codexAccountName } from "./codex-accounts-state.mjs";
import { projectSessionRows } from "./session-order.mjs";

export const CODEX = "codex";

// The desktop lists Codex sessions only when it asks; an older server
// ignores the parameter and lists Claude sessions as before.
export const SESSION_PROVIDERS = "claude,codex";

export function sessionListPath({ archived = false, after = "", wait = 0, providers = SESSION_PROVIDERS } = {}) {
  const query = new URLSearchParams({ providers });
  if (archived) query.set("archived", "true");
  if (after) query.set("after", after);
  if (after && wait > 0) query.set("wait", String(wait));
  return "/api/cli/sessions?" + query.toString();
}

export const isCodexSession = (session) => session?.provider === CODEX;
export const isCodexTab = (tab) => tab?.cli_session_provider === CODEX;

export function providerLabel(provider) {
  return provider === CODEX ? "Codex" : "Claude";
}

export function providerChip(session) {
  const provider = isCodexSession(session) ? CODEX : "claude";
  return { provider, label: providerLabel(provider) };
}

// Who writes the thread now. Anything the server does not send is "".
const OWNERS = new Set(["", "desktop", "phone", "external"]);
export function sessionOwner(session) {
  if (!isCodexSession(session)) return "";
  const owner = session?.owner || "";
  return OWNERS.has(owner) ? owner : "external";
}

const OWNER_STATES = {
  "": { owner: "", label: "", tooltip: "", muted: false },
  desktop: { owner: "desktop", label: "电脑", tooltip: "在这台电脑的 Codex 终端中打开", muted: false },
  phone: { owner: "phone", label: "手机", tooltip: "手机上的一轮正在运行", muted: true },
  external: { owner: "external", label: "外部", tooltip: "在其他 Codex 进程中打开", muted: true },
};

export function ownerState(session) {
  const state = OWNER_STATES[sessionOwner(session)];
  return { ...state, live: Boolean(state.owner && session?.active) };
}

export const READONLY_REASONS = {
  phone: "手机发起的一轮仍在运行，完成后再在终端中继续。",
  external: "这段会话正在其他 Codex 进程中打开，请先退出该进程后再继续。",
};
// When "在终端中继续" becomes available, per owner.
export const CONTINUE_HINTS = { phone: "本轮完成后可用", external: "退出其他 Codex 进程后可用" };
// The server's owner notices (connection_notice) are worded for the phone
// ("再从手机继续"); the desktop pane states the owner in its own words and
// shows only the history notices.
export const PHONE_OWNER_NOTICES = new Set([
  "这段会话正在电脑的 Codex 终端中打开，手机上可以查看和停止本轮。",
  "这段会话正在电脑上的 Codex 终端中打开。请先退出该终端中的 Codex，再从手机继续。",
]);

// What a click on a Codex row does: focus its live tab, open it (the server
// reuses a terminal it already has), or show it read-only with the reason.
export function codexSessionAction(session, tabs = []) {
  const tab = tabs.find((item) => item.cli_session_id === session?.id && !item.closed);
  if (tab) return { action: "focus", tabId: tab.id, reason: "" };
  const owner = sessionOwner(session);
  if (owner === "phone" || owner === "external") return { action: "readonly", tabId: "", reason: READONLY_REASONS[owner] };
  return { action: "open", tabId: "", reason: "" };
}

// The read-only pane may continue in a terminal once nobody writes.
export const canContinueInTerminal = (session) => sessionOwner(session) === "";
export const canStopFromPane = (session) => {
  const owner = sessionOwner(session);
  const last = session?.turns?.at?.(-1);
  return (owner === "phone" || owner === "desktop") && last?.status === "running";
};

// Rows for one project with both providers, in the shared order. Codex rows
// stay clickable while another device owns the thread: they open read-only.
export function mixedSessionRows(tabs, sessions, saved, manualOrder, baseline) {
  const byId = new Map(sessions.map((session) => [session.id, session]));
  return projectSessionRows(tabs, sessions, saved, manualOrder, baseline).map((row) => {
    const session = byId.get(row.cli_session_id);
    const codex = isCodexSession(session) || isCodexTab(row);
    if (!codex) return { ...row, sessionProvider: "claude" };
    return {
      ...row,
      sessionProvider: CODEX,
      sessionRunningElsewhere: false,
      sessionClaudeDesktop: false,
      sessionOwner: session ? sessionOwner(session) : "desktop",
      sessionAction: session ? codexSessionAction(session, tabs).action : "focus",
    };
  });
}

// A bound Codex terminal shows its output at once: no Claude startup veil and
// no Claude account header.
export function shouldGateStartup(tab) {
  return Boolean(tab?.cli_session_id && !tab.closed && !isCodexTab(tab));
}

export function showSessionAccount(tab) {
  return Boolean(tab?.cli_session_id) && !isCodexTab(tab);
}

export function codexSessionAccountLabel(session, accounts) {
  if (session?.account_label) return session.account_label;
  if (!session?.codex_account_id) return "默认登录";
  return codexAccountName(accounts?.find((account) => account.id === session.codex_account_id)) || "Codex 账号";
}

// provider: "codex", "" (Claude) or "mixed" (a selection of both). A bulk
// confirmation names the count even for one session, as it did before.
export function purgeCopy(provider, title, count = 1, bulk = count > 1) {
  const history = provider === CODEX ? "Codex 原始对话" : provider === "mixed" ? "原始对话" : "Claude Code 原始对话和子代理记录";
  if (bulk) return `永久删除所选的 ${count} 个会话及其${provider === "mixed" ? "" : " "}${history}？此操作无法撤销。`;
  return `永久删除“${title || "新会话"}”及其 ${history}？此操作无法撤销。`;
}

// The provider of a selection for purgeCopy.
export function selectionProvider(sessions) {
  const codex = sessions.filter(isCodexSession).length;
  return !codex ? "" : codex === sessions.length ? CODEX : "mixed";
}

export function codexCapabilities(answer) {
  const capabilities = answer?.capabilities;
  return { known: Boolean(capabilities), sessions: Boolean(capabilities?.codex_sessions), cli: capabilities?.codex_cli || null };
}

// The menu entry names what it makes.
export function codexMenuLabel(capabilities) {
  return capabilities?.sessions ? "Codex 会话" : "Codex 终端";
}

// GET /api/cli/sessions/{id}: after (the view's revision) and epoch (its
// generation) answer 204 while unchanged; wait holds that (a server whose list
// carries revisions).
export function sessionViewPath(id, { after = "", epoch = "", wait = 0 } = {}) {
  const query = new URLSearchParams();
  if (after !== "" && after !== undefined && after !== null) {
    query.set("after", String(after));
    if (epoch) query.set("epoch", epoch);
    if (wait > 0) query.set("wait", String(wait));
  }
  const text = query.toString();
  return "/api/cli/sessions/" + encodeURIComponent(id) + (text ? "?" + text : "");
}

// The archive confirmation says what archiving does to whoever writes now.
export function archiveCopy(session, { closing = false } = {}) {
  const kept = "历史将保留，可从设置中的已归档会话恢复。";
  if (!isCodexSession(session)) {
    return { title: "归档这段会话？", body: "归档会关闭关联终端。" + kept, confirm: "归档会话" };
  }
  const owner = sessionOwner(session);
  const body = owner === "desktop" ? "关闭后这段会话会归档：Codex 终端会关闭，正在进行的一轮会停止。"
    : owner === "phone" ? "归档会停止手机上正在运行的这一轮。"
      : owner === "external" ? "归档只在 Super Cage 中收起这段会话，不会停止其他进程中的 Codex。"
        : "";
  return { title: closing ? "关闭并归档这段会话？" : "归档这段会话？", body: body + kept, confirm: closing ? "关闭并归档" : "归档会话" };
}

// The state word in the header of a Codex session's pane.
export function codexStateWord(session, { closed = false } = {}) {
  if (closed) return "已结束";
  const owner = sessionOwner(session);
  if (owner === "phone") return session?.active ? "手机正在运行" : "手机上打开";
  if (owner === "external") return "在其他进程中打开";
  if (owner === "desktop") return session?.active ? "运行中" : "空闲";
  return "空闲";
}

export const ERROR_COPY = {
  desktopOwned: "这段会话正在电脑的 Codex 终端中打开。可在手机的终端页打开该终端继续，或等电脑上退出 Codex 后再发送。",
  phoneRunning: READONLY_REASONS.phone,
  otherTerminal: "这段会话已在另一个电脑终端中打开。",
  otherProcess: READONLY_REASONS.external,
  command: "Codex 启动命令不是 codex，无法以会话方式打开。请在设置中恢复为 codex 后重试。",
  phoneClose: "这段会话正在电脑的 Codex 终端中打开，请在电脑上关闭或归档。",
};
