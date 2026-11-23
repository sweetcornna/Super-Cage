import React, { useEffect, useMemo, useState } from "react";
// tasks.mjs holds the plain rules; this file draws them.
import { Bot, Check, ChevronRight, CircleAlert, CircleStop, SquareTerminal } from "lucide-react";
import { Sheet } from "./sheet";
import { callsText, createTicker, duration, elapsed, kindLabel, nowText, runningText, sessionTasks, splitTools, stateLabel, STRIP_ROWS, taskLabel, taskName } from "./tasks.mjs";
import "./tasks.css";

const ticker = createTicker({ hidden: () => document.hidden });
document.addEventListener("visibilitychange", () => ticker.visibility());

// The current time in seconds, moved by the shared tick only while active.
function useClock(active) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return undefined;
    setNow(Date.now());
    return ticker.subscribe(setNow);
  }, [active]);
  return now / 1000;
}

const icons = { completed: Check, failed: CircleAlert, stopped: CircleStop };

function StateMark({ task }) {
  if (task.state === "running") return <i className="task-dot" aria-hidden="true" />;
  const Icon = icons[task.state] || Check;
  return <Icon size={15} className={"task-state-icon " + (task.state || "")} aria-hidden="true" />;
}

// What still runs, pinned above the composer while anything runs. Any row
// opens the full list.
export function TaskStrip({ session, onOpen }) {
  const { running, count } = useMemo(() => sessionTasks(session), [session]);
  const now = useClock(running.length > 0);
  if (!running.length) return null;
  const shown = running.slice(0, STRIP_ROWS);
  return <section className="task-strip" aria-label={runningText(count)}>
    <ul>
      {shown.map((task) => <li key={task.id}>
        <button type="button" className="task-row" aria-label={taskName(task, now)} onClick={onOpen}>
          <StateMark task={task} />
          <span className="task-title"><strong>{taskLabel(task)}</strong><small>{kindLabel(task)}</small></span>
          <span className="task-time">{duration(elapsed(task, now))}</span>
          {nowText(task) && <span className="task-now">{nowText(task)}</span>}
        </button>
      </li>)}
    </ul>
    {count > shown.length && <button type="button" className="task-more full" onClick={onOpen}>还有 {count - shown.length} 个<ChevronRight size={14} aria-hidden="true" /></button>}
    {count > 1 && <button type="button" className="task-more compact" onClick={onOpen}>还有 {count - 1} 个<ChevronRight size={14} aria-hidden="true" /></button>}
  </section>;
}

function TaskItem({ task, now }) {
  const spent = duration(elapsed(task, now));
  const Icon = task.kind === "background_command" ? SquareTerminal : Bot;
  return <li className={"task-item " + (task.state || "")} aria-label={taskName(task, now)}>
    <Icon size={17} className="task-kind-icon" aria-hidden="true" />
    <span className="task-title"><strong>{taskLabel(task)}</strong><small>{kindLabel(task)}{task.agent_type && task.detail ? " · " + task.agent_type : ""}</small></span>
    <span className={"task-outcome " + (task.state || "")}><StateMark task={task} />{stateLabel(task)}</span>
    {(spent || callsText(task)) && <span className="task-meta">{[spent && (task.state === "running" ? "已运行 " : "用时 ") + spent, callsText(task)].filter(Boolean).join(" · ")}</span>}
    {task.state === "running" && nowText(task) && <span className="task-now">正在 {nowText(task)}</span>}
  </li>;
}

// This session's subagents and background tasks: running first, then ended.
export function TaskSheet({ session, onClose }) {
  const { running, ended, more } = useMemo(() => sessionTasks(session), [session]);
  const now = useClock(running.length > 0);
  return <Sheet title="子智能体与后台任务" onClose={onClose} className="task-sheet">
    {running.length > 0 && <section aria-label="运行中">
      <h3>运行中</h3>
      <ul className="task-list">{running.map((task) => <TaskItem key={task.id} task={task} now={now} />)}</ul>
      {more > 0 && <p className="note">还有 {more} 个运行中的任务未列出。</p>}
    </section>}
    {ended.length > 0 && <section aria-label="已结束">
      <h3>已结束</h3>
      <ul className="task-list">{ended.map((task) => <TaskItem key={task.id} task={task} now={now} />)}</ul>
    </section>}
    {!running.length && !ended.length && <p className="note">这段会话里没有子智能体或后台任务。</p>}
  </Sheet>;
}

// A turn's subagents and background tasks as their own small cards.
export function TaskCards({ tools, onOpen }) {
  const { tasks } = splitTools(tools);
  if (!tasks.length) return null;
  return <ul className="task-cards" aria-label="本轮启动的任务">
    {tasks.map((task) => {
      const spent = task.state === "running" ? "" : duration(elapsed(task, 0));
      return <li key={task.id}>
        <button type="button" className={"task-card " + (task.state || "")} onClick={onOpen}
          aria-label={kindLabel(task) + "：" + taskLabel(task) + "，" + stateLabel(task) + (spent ? "，用时 " + spent : "")}>
          <StateMark task={task} />
          <span className="task-title"><strong>{taskLabel(task)}</strong><small>{kindLabel(task)}</small></span>
          <span className={"task-outcome " + (task.state || "")}>{stateLabel(task)}{spent && <small>{spent}</small>}</span>
        </button>
      </li>;
    })}
  </ul>;
}

export function TaskCount({ session }) {
  const { count } = useMemo(() => sessionTasks(session), [session]);
  return count > 0 ? <span className="task-count">{runningText(count)}</span> : null;
}
