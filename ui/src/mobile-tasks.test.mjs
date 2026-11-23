import assert from "node:assert/strict";
import test from "node:test";
import { callsText, createTicker, duration, elapsed, isTask, nowText, runningText, sessionTasks, splitTools, taskLabel, taskName } from "./mobile-cli/tasks.mjs";

const agent = { id: "a", name: "Agent", status: "completed", kind: "subagent", state: "running", detail: "梳理目录", agent_type: "Explore", started_at: 100, now: { name: "Read", detail: "src/app.py" }, calls: 3 };
const shell = { id: "b", name: "Bash", status: "completed", kind: "background_command", state: "failed", started_at: 100, ended_at: 108.5 };
const plain = { id: "c", name: "Read", status: "completed", detail: "a.md" };

test("a turn's tasks are split from its ordinary calls; an older server's tools are all ordinary", () => {
  assert.deepEqual(splitTools([agent, plain, shell]), { tasks: [agent, shell], calls: [plain] });
  assert.deepEqual(splitTools([plain, { ...plain, id: "d", kind: "future_kind" }]).tasks, []);
  assert.deepEqual(splitTools(undefined), { tasks: [], calls: [] });
  assert.equal(isTask({ kind: "constructor" }), false);
});

test("labels follow the display rule and never invent text", () => {
  assert.equal(taskLabel(agent), "梳理目录");
  assert.equal(taskLabel({ kind: "subagent", agent_type: "Explore" }), "Explore");
  assert.equal(taskLabel({ kind: "background_command" }), "后台命令");
  assert.equal(nowText(agent), "Read · src/app.py");
  assert.equal(nowText({ now: { name: "Poll" } }), "Poll");
  assert.equal(nowText({}), "");
  assert.equal(callsText(agent), "3 次工具调用");
  assert.equal(callsText({ calls: 12, calls_partial: true }), "至少 12 次工具调用");
  assert.equal(callsText({}), "");
  assert.equal(runningText(2), "2 个任务运行中");
  assert.equal(runningText(0), "");
});

test("elapsed time needs no server clock and never goes negative", () => {
  assert.equal(elapsed(agent, 112.9), 12.900000000000006);
  assert.equal(elapsed(agent, 90), 0);
  assert.equal(elapsed(shell, 999), 8.5);
  assert.equal(elapsed({ state: "stopped", started_at: 1 }, 50), null);
  assert.equal(elapsed({ state: "running" }, 50), null);
  assert.equal(duration(8.5), "8 秒");
  assert.equal(duration(65), "1 分 05 秒");
  assert.equal(duration(3725), "1 小时 2 分");
  assert.equal(duration(null), "");
  assert.equal(taskName(agent, 112), "子智能体：梳理目录，运行中，已运行 12 秒，正在 Read · src/app.py");
  assert.equal(taskName(shell, 112), "后台命令：后台命令，失败，用时 8 秒");
});

test("the session summary outlives trimmed turns; ended tasks come newest first", () => {
  const old = { id: "x", kind: "background_command", state: "running", started_at: 50 };
  const session = { turns: [{ tools: [agent, plain] }, { tools: [shell, { ...shell, id: "e", state: "completed", ended_at: 120 }] }], tasks: { count: 20, running: [agent, old] } };
  const tasks = sessionTasks(session);
  assert.deepEqual(tasks.running.map((t) => t.id), ["a", "x"]);
  assert.deepEqual(tasks.ended.map((t) => t.id), ["e", "b"]);
  assert.equal(tasks.count, 20);
  assert.equal(tasks.more, 18);
  // A server without the summary: the turns' running tools.
  assert.deepEqual(sessionTasks({ turns: session.turns }).running.map((t) => t.id), ["a"]);
  assert.deepEqual(sessionTasks({ turns: [{ tools: [plain] }] }), { running: [], ended: [], count: 0, more: 0 });
  assert.deepEqual(sessionTasks(null), { running: [], ended: [], count: 0, more: 0 });
});

test("one shared tick runs only while listened to and visible", () => {
  let hidden = false, scheduled = 0, cancelled = 0, tick = null, clock = 1000;
  const ticker = createTicker({ hidden: () => hidden, now: () => clock, schedule: (fn) => { scheduled++; tick = fn; return scheduled; }, cancel: () => { cancelled++; tick = null; } });
  assert.equal(ticker.running, false);
  const seen = [];
  const stopA = ticker.subscribe((t) => seen.push(["a", t]));
  const stopB = ticker.subscribe((t) => seen.push(["b", t]));
  assert.equal(scheduled, 1, "two clocks share one timer");
  clock = 2000; tick();
  assert.deepEqual(seen, [["a", 2000], ["b", 2000]]);
  hidden = true; ticker.visibility();
  assert.equal(ticker.running, false);
  assert.equal(cancelled, 1);
  hidden = false; clock = 9000; ticker.visibility();
  assert.equal(ticker.running, true);
  assert.deepEqual(seen.at(-1), ["b", 9000], "a page coming back is told the time at once");
  stopA(); stopB();
  assert.equal(ticker.running, false);
  assert.equal(scheduled, 2);
});
