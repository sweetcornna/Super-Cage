// Subagents and background tasks of a session, as the server reports them on
// tool entries (kind, state, started_at, ended_at, agent_type, now, calls) and
// in the session's running summary (tasks: {count, running}). Plain values
// only, so the strip, the cards and the sheet can be tested without React.
// An older server sends none of these fields: nothing here then shows.

export const KIND_LABELS = { subagent: "子智能体", background_agent: "后台智能体", background_command: "后台命令" };
export const STATE_LABELS = { running: "运行中", completed: "已完成", failed: "失败", stopped: "已停止" };
// Rows the strip above the composer shows before "还有 N 个".
export const STRIP_ROWS = 3;

export const isTask = (tool) => Boolean(tool && typeof tool === "object" && Object.hasOwn(KIND_LABELS, tool.kind));

// A turn's tools: its subagents and background tasks, and the ordinary calls.
export function splitTools(tools) {
  const tasks = [], calls = [];
  for (const tool of tools || []) (isTask(tool) ? tasks : calls).push(tool);
  return { tasks, calls };
}

export const kindLabel = (task) => KIND_LABELS[task?.kind] || "任务";
export const stateLabel = (task) => STATE_LABELS[task?.state] || "";
// The description first, then the subagent type, then what it is.
export const taskLabel = (task) => task?.detail || task?.agent_type || kindLabel(task);
export function nowText(task) {
  const now = task?.now;
  if (!now?.name) return "";
  return now.detail ? now.name + " · " + now.detail : now.name;
}
export function callsText(task) {
  if (!Number.isInteger(task?.calls) || task.calls <= 0) return "";
  return (task.calls_partial ? "至少 " : "") + task.calls + " 次工具调用";
}
export const runningText = (count) => (count > 0 ? count + " 个任务运行中" : "");

// What a session has: running first (the server's summary, which outlives a
// trimmed turn; the turns themselves for a server without it), then the ended
// ones, newest first.
export function sessionTasks(session) {
  const seen = new Set(), all = [];
  for (const turn of session?.turns || []) {
    for (const tool of turn?.tools || []) {
      if (isTask(tool) && !seen.has(tool.id)) { seen.add(tool.id); all.push(tool); }
    }
  }
  const summary = session?.tasks;
  const running = Array.isArray(summary?.running) ? summary.running.filter(isTask) : all.filter((task) => task.state === "running");
  const count = Number.isInteger(summary?.count) && summary.count >= running.length ? summary.count : running.length;
  const live = new Set(running.map((task) => task.id));
  const time = (task) => task.ended_at || task.started_at || 0;
  const ended = all.filter((task) => !live.has(task.id) && task.state !== "running").sort((a, b) => time(b) - time(a));
  return { running, ended, count, more: Math.max(0, count - running.length) };
}

// Seconds a task has run (running: until now; ended: until its end), or null.
export function elapsed(task, nowSeconds) {
  if (!Number.isFinite(task?.started_at)) return null;
  if (task.state === "running") return Math.max(0, nowSeconds - task.started_at);
  if (Number.isFinite(task.ended_at)) return Math.max(0, task.ended_at - task.started_at);
  return null;
}

export function duration(seconds) {
  if (!Number.isFinite(seconds) || seconds < 0) return "";
  const s = Math.floor(seconds);
  if (s < 60) return s + " 秒";
  if (s < 3600) return Math.floor(s / 60) + " 分 " + String(s % 60).padStart(2, "0") + " 秒";
  return Math.floor(s / 3600) + " 小时 " + Math.floor((s % 3600) / 60) + " 分";
}

// The accessible name of a task row: kind, label, state, time and what it does now.
export function taskName(task, nowSeconds) {
  const spent = duration(elapsed(task, nowSeconds));
  const parts = [kindLabel(task) + "：" + taskLabel(task), stateLabel(task)];
  if (spent) parts.push((task.state === "running" ? "已运行 " : "用时 ") + spent);
  if (task.state === "running" && nowText(task)) parts.push("正在 " + nowText(task));
  return parts.filter(Boolean).join("，");
}

// One shared tick for every clock on the page: it runs only while someone
// listens and the page is visible, at most once a second.
export function createTicker({ every = 1000, hidden = () => false, schedule = setInterval, cancel = clearInterval, now = Date.now } = {}) {
  const listeners = new Set();
  let timer = null;
  const sync = () => {
    const wanted = listeners.size > 0 && !hidden();
    if (wanted && timer === null) timer = schedule(() => { const time = now(); for (const listener of listeners) listener(time); }, every);
    if (!wanted && timer !== null) { cancel(timer); timer = null; }
  };
  return {
    subscribe(listener) {
      listeners.add(listener);
      sync();
      return () => { listeners.delete(listener); sync(); };
    },
    // Call when the page's visibility changes; a page coming back is told the time at once.
    visibility() {
      sync();
      if (!hidden()) { const time = now(); for (const listener of listeners) listener(time); }
    },
    get running() { return timer !== null; },
  };
}
