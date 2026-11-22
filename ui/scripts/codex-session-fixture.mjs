import { createHash } from "node:crypto";

// Mocked Codex sessions for UI fixtures, one per owner state. Replies follow the
// workbench contract (internal/workbench/codex_sessions.go, mobile_cli_http.go):
// the desktop list carries Codex rows and "capabilities" only when asked with
// ?providers=claude,codex; /open on a desktop path binds a Codex terminal.
const defaultProject = "/Users/shihao/.claude-cage-workspace";
const at = "2026-11-20T10:00:00Z";
const turn = (id, prompt, text, status = "completed") => ({ id, prompt, text, status });

export function codexSessionFixtures(project = defaultProject) {
  return [
    { id: "c0d3c0d3-0000-4000-8000-000000000001", provider: "codex", workspace: project, title: "整理登录流程", owner: "", running: false, active: false,
      codex_account_id: "", account_label: "", pinned: false, revision: 3, generation: "fixture", updated_at: at,
      turns: [turn("c0d3c0d3-1000-4000-8000-000000000001", "看看登录页", "已检查登录页。")] },
    { id: "c0d3c0d3-0000-4000-8000-000000000002", provider: "codex", workspace: project, title: "电脑上打开的会话", owner: "desktop", running: true, active: true,
      codex_account_id: "aaaaaaaaaaaaaaaaaaaaaaaa", account_label: "工作", pinned: false, revision: 5, generation: "fixture", updated_at: at,
      turns: [turn("c0d3c0d3-1000-4000-8000-000000000002", "重构样式", "", "running")] },
    { id: "c0d3c0d3-0000-4000-8000-000000000003", provider: "codex", workspace: project, title: "手机上运行的一轮", owner: "phone", running: true, active: true,
      codex_account_id: "", account_label: "", pinned: true, revision: 7, generation: "fixture", updated_at: at,
      turns: [turn("c0d3c0d3-1000-4000-8000-000000000003", "跑一遍测试", "", "running")] },
    { id: "c0d3c0d3-0000-4000-8000-000000000004", provider: "codex", workspace: project, title: "其他进程中打开", owner: "external", running: true, active: false,
      codex_account_id: "", account_label: "", pinned: false, revision: 2, generation: "fixture", updated_at: at,
      turns: [turn("c0d3c0d3-1000-4000-8000-000000000004", "外部终端里的问题", "回答。")] },
  ];
}

const notices = {
  desktop: "这段会话正在电脑的 Codex 终端中打开，手机上可以查看和停止本轮。",
  external: "这段会话正在电脑上的 Codex 终端中打开。请先退出该终端中的 Codex，再从手机继续。",
};
const refusals = {
  phone: "手机发起的一轮仍在运行，完成后再在终端中继续。",
  external: "这段会话正在其他 Codex 进程中打开，请先退出该进程后再继续。",
};
const summary = ({ turns, viewContent, ...row }) => row;
const text = (route, status, body) => route.fulfill({ status, contentType: "text/plain; charset=utf-8", body });

function view({ viewContent, ...session }) {
  return { ...session, can_send: session.owner === "", terminal_available: undefined, connection_notice: notices[session.owner] || "",
    desktop_terminal_id: session.terminal_id || undefined };
}

function boundTerminal(state, session) {
  const terminal = { id: "term-" + ++state.sequence, title: session.title, workspace: session.workspace, kind: "codex", closed: false,
    cli_session_id: session.id, cli_session_provider: "codex", codex_account_id: session.codex_account_id, cli_starting: false };
  state.terminals.push(terminal);
  session.terminal_id = terminal.id;
  return terminal;
}

// Sets up the sessions once, with the desktop-owned one's terminal open as it
// is on a real server. Call it when the fixture state is made so that the
// terminal list has the tab from the start.
export function seedCodexSessions(state) {
  if (state.codexSessions) return state.codexSessions;
  state.codexSessions = codexSessionFixtures(state.status?.projects?.[0]);
  for (const session of state.codexSessions) if (session.owner === "desktop") boundTerminal(state, session);
  return state.codexSessions;
}

// The list's revision changes with what it shows, as the server's does.
const revisionOf = (answer) => createHash("sha256").update(JSON.stringify(answer)).digest("hex").slice(0, 32);
const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// A held request looks again every 50 ms and answers as soon as its answer
// changes, like the server (which wakes within 250 ms). state.codexHoldMs
// caps the hold (default: the requested wait).
async function hold(state, wait, unchanged) {
  const deadline = Date.now() + Math.min(wait * 1000, state.codexHoldMs ?? wait * 1000);
  while (unchanged() && Date.now() < deadline) await pause(Math.min(50, Math.max(1, deadline - Date.now())));
  return unchanged();
}

function listAnswer(state, url) {
  const sessions = seedCodexSessions(state);
  const providers = url.searchParams.getAll("providers");
  const archived = url.searchParams.get("archived") === "true";
  // An older server ignores providers, after and wait: Claude rows only, no
  // capabilities, no revision.
  if (state.codexOlderServer) return { sessions: (state.claudeSessions || []).filter((item) => Boolean(item.archived) === archived), notice: "" };
  const codex = providers.length === 1 && providers[0].split(",").includes("codex");
  const claude = (state.claudeSessions || []).filter((item) => Boolean(item.archived) === archived);
  const answer = { sessions: [...claude, ...(codex ? sessions.filter((item) => Boolean(item.archived) === archived).map(summary) : [])], notice: "" };
  if (codex) answer.capabilities = { codex_sessions: state.codexMenuSessions !== false, codex_cli: { installed: true, complete: true, version: "0.171.0", resume: true, no_daemon: true, resume_no_daemon: true, delete: true, writer_locks: true } };
  answer.revision = revisionOf(answer);
  return answer;
}

// A view's revision moves whenever what it shows moves (the server's does
// too); tests may change a session directly.
function viewOf(session) {
  const content = JSON.stringify(view({ ...session, revision: 0 }));
  if (session.viewContent !== undefined && session.viewContent !== content) session.revision++;
  session.viewContent = content;
  return view(session);
}

// Serves /api/cli/sessions* for a workbench fixture. state.codexSessions holds
// the sessions; tests change an owner there to move a session between states.
// state.codexRefusals["<id>/<action>"] = [status, text] makes the next call of
// that action refuse with the server's text (a refusal the UI shows in place).
// A held list (after + wait) answers 204 after state.codexHoldMs (500 ms by
// default) when nothing changed, so a long-poll loop does not spin.
export async function codexSessionsRoute(route, request, url, body, state) {
  const path = url.pathname.replace(/^\/api/, "");
  const sessions = seedCodexSessions(state);
  if (path === "/cli/sessions" && request.method() === "GET") {
    const providers = url.searchParams.getAll("providers");
    if (!state.codexOlderServer && (providers.length > 1 || providers.length === 1 && providers[0].split(",").some((name) => !["claude", "codex"].includes(name))))
      return text(route, 400, "会话类型无效");
    let answer = listAnswer(state, url);
    if (state.codexOlderServer) return route.fulfill({ json: answer });
    const after = url.searchParams.get("after") || "";
    const wait = Number(url.searchParams.get("wait") || 0);
    if (after && !/^[0-9a-f]{32}$/.test(after) || wait > 0 && !after) return text(route, 400, "会话列表版本无效");
    if (after && wait > 0 && await hold(state, wait, () => (answer = listAnswer(state, url)).revision === after)) return route.fulfill({ status: 204 });
    if (after && after === answer.revision) return route.fulfill({ status: 204 });
    return route.fulfill({ json: answer });
  }
  const match = path.match(/^\/cli\/sessions\/([^/]+)(?:\/([a-z]+))?$/);
  const session = match && sessions.find((item) => item.id === match[1]);
  if (!session) return text(route, 404, "404 page not found");
  const action = match[2] || "";
  if (!action && request.method() === "GET") {
    const after = url.searchParams.get("after");
    const wait = Number(url.searchParams.get("wait") || 0);
    if (after !== null && after !== "") {
      const unchanged = () => String(viewOf(session).revision) === after && (!url.searchParams.get("epoch") || url.searchParams.get("epoch") === session.generation);
      if (wait > 0 && !state.codexOlderServer ? await hold(state, wait, unchanged) : unchanged()) return route.fulfill({ status: 204 });
    }
    return route.fulfill({ json: viewOf(session) });
  }
  if (request.method() !== "POST") return text(route, 405, "Method Not Allowed");
  const refusal = state.codexRefusals?.[session.id + "/" + action];
  if (refusal) {
    delete state.codexRefusals[session.id + "/" + action];
    return text(route, ...refusal);
  }
  if (action === "account") return route.fulfill({ json: { session: { id: session.id }, supported: false, candidates: [] } });
  if (action === "open") {
    if (session.archived) return text(route, 410, "此会话已归档，请先在设置中的已归档会话撤销归档");
    if (refusals[session.owner]) return text(route, 409, refusals[session.owner]);
    const terminal = state.terminals.find((item) => item.cli_session_id === session.id && !item.closed) || boundTerminal(state, session);
    Object.assign(session, { owner: "desktop", running: true, terminal_id: terminal.id });
    return route.fulfill({ json: view(session) });
  }
  if (action === "stop") {
    const last = session.turns.at(-1);
    if (!last || last.status !== "running" || last.id !== body?.request_id) return text(route, 409, "本轮运行状态已变化，请重新读取");
    if (session.owner === "external") return text(route, 409, "这一轮不在 Super Cage 的终端中运行，请在运行它的终端中停止");
    // The desktop's Codex gets Esc and the server answers at once, with the
    // turn still running until Codex writes that it stopped.
    if (session.owner === "desktop") return route.fulfill({ json: view(session) });
    last.status = "stopped";
    Object.assign(session, { active: false, owner: "", running: false });
    return route.fulfill({ json: view(session) });
  }
  if (action === "metadata") {
    if (typeof body?.title === "string") session.title = body.title;
    if (typeof body?.pinned === "boolean") session.pinned = body.pinned;
    return route.fulfill({ json: summary(session) });
  }
  if (action === "purge") {
    if (!body?.confirm) return text(route, 400, "需要确认永久删除该会话及其原始历史");
    if (session.owner) return text(route, 400, "这段会话正在电脑的 Codex 终端中打开，请先关闭该终端再删除");
    state.codexSessions = sessions.filter((item) => item !== session);
    return route.fulfill({ json: { ok: true } });
  }
  if (action === "archive" || action === "restore") {
    session.archived = action === "archive";
    if (session.archived) {
      state.terminals = state.terminals.filter((item) => item.cli_session_id !== session.id);
      Object.assign(session, { owner: "", running: false, active: false, terminal_id: undefined });
    }
    return route.fulfill({ json: { ok: true } });
  }
  return text(route, 404, "404 page not found");
}

// POST /api/terminals?providers=claude,codex {kind:"codex"}: the menu makes a
// session with its terminal bound at once (codex_sessions only).
export function codexMenuTerminal(state, body) {
  const sessions = seedCodexSessions(state);
  const id = "c0d3c0d3-0000-4000-9000-" + String(++state.sequence).padStart(12, "0");
  const session = { id, provider: "codex", workspace: body?.workspace || state.status?.projects?.[0], title: "新 Codex 会话", owner: "desktop", running: true,
    active: false, codex_account_id: body?.codex_account_id || "", account_label: "", pinned: false, revision: 1, generation: "fixture",
    updated_at: new Date().toISOString(), turns: [] };
  sessions.push(session);
  return boundTerminal(state, session);
}

// Terminal routes of bound Codex terminals: metadata (rename, pin) and close
// (a live one archives its session; an ended one only goes). Returns false
// for any other terminal.
export function codexTerminalRoute(route, request, path, body, state) {
  const match = path.match(/^\/terminals\/([^/]+)(\/metadata)?$/);
  const terminal = match && state.terminals.find((item) => item.id === match[1] && item.cli_session_provider === "codex");
  const session = terminal && seedCodexSessions(state).find((item) => item.id === terminal.cli_session_id);
  if (!session) return false;
  if (match[2] && request.method() === "POST") {
    if (typeof body?.title === "string") terminal.title = session.title = body.title;
    if (typeof body?.pinned === "boolean") terminal.pinned = session.pinned = body.pinned;
    terminal.custom_title = Boolean(body?.title);
    terminal.metadata_set = true;
    return route.fulfill({ json: { title: terminal.title, custom_title: terminal.custom_title, pinned: Boolean(terminal.pinned) } });
  }
  if (!match[2] && request.method() === "DELETE") {
    state.terminals = state.terminals.filter((item) => item !== terminal);
    if (!terminal.closed) Object.assign(session, { archived: true, owner: "", running: false, active: false, terminal_id: undefined });
    return route.fulfill({ json: { ok: true } });
  }
  return false;
}
