// What a session shows about its account at a quota limit and at a switch.
// Desktop and phone render the same sentences from the same server reply.

export const WINDOW_NAMES = { five_hour: "5 小时限额", seven_day: "周限额" };

export function resetLabel(seconds, timeZone) {
  if (!seconds) return "";
  const parts = Object.fromEntries(new Intl.DateTimeFormat("en-US", {
    month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit", hourCycle: "h23", timeZone,
  }).formatToParts(new Date(seconds * 1000)).map((part) => [part.type, part.value]));
  return `${parts.month}/${parts.day} ${parts.hour}:${parts.minute}`;
}

// "A 的额度已用完（周限额，11/27 18:00 重置）"
export function limitPhrase(label, window, resetsAt, timeZone) {
  const when = resetsAt ? `，${resetLabel(resetsAt, timeZone)} 重置` : "";
  return `${label} 的额度已用完（${WINDOW_NAMES[window] || "额度限制"}${when}）`;
}

// The server reply tells which fields and actions it knows; an older server
// gets only what it accepted before.
export const handoffAPI = (view) => Number(view?.handoff_api) || 0;

export function handoffQuestion(view, timeZone) {
  const handoff = view?.handoff, question = handoff?.question;
  if (!question || handoffAPI(view) < 2) return null;
  const source = view.session?.account_label || "当前账号";
  return {
    target: question,
    source,
    text: `${limitPhrase(source, handoff.window, handoff.resets_at, timeZone)}。要换到 ${question.label} 继续吗？会话历史会带到 ${question.label}。`,
  };
}

// One quiet line per handoff at a limit; a switch the person made needs none.
export function handoffLine(session, timeZone) {
  const sw = session?.account_switch;
  if (!sw || sw.phase !== "completed") return "";
  if (sw.reason === "five_hour" || sw.reason === "seven_day") {
    return `${limitPhrase(sw.source_label || "原账号", sw.reason, sw.resets_at, timeZone)}，已换到 ${sw.target_label} 继续`;
  }
  if (sw.reason === "reset") return `${sw.target_label} 的额度已恢复，会话已继续`;
  return "";
}

export function waitingLine(view, timeZone) {
  const handoff = view?.handoff;
  if (!handoff?.waiting || handoffAPI(view) < 2) return "";
  const source = view.session?.account_label || "当前账号";
  const when = resetLabel(handoff.wait_until, timeZone);
  if (handoff.waiting === "source") {
    return `${limitPhrase(source, handoff.window, handoff.resets_at, timeZone)}，正在等它恢复。也可以在账号菜单中直接换一个账号。`;
  }
  const first = when ? `${handoff.wait_label} 将在 ${when} 最先恢复` : "要等账号恢复";
  return `可以接续的账号额度都已用完：${first}${handoff.automatic ? "，届时自动继续" : ""}。也可以在账号菜单中直接选一个账号。`;
}

// Every account the person has is a target; the note says what to expect.
export function switchTargets(view) {
  if (Array.isArray(view?.targets)) return view.targets;
  return (view?.candidates || []).map((option) => ({ ...option, status: "ready", enabled: true, remaining: -1 }));
}

export function targetNote(target, timeZone) {
  if (target.status !== "ready") return "未登录";
  if (target.exhausted) return `额度已用完，${resetLabel(target.resets_at, timeZone)} 重置`;
  const notes = [];
  if (target.remaining >= 0) notes.push(`剩余 ${Math.round(target.remaining)}%`);
  else notes.push("额度未知");
  if (!target.enabled) notes.push("未参与自动分配");
  return notes.join(" · ");
}

export const targetSelectable = (target) => target?.status === "ready";

export const needsConfirm = (view) => handoffAPI(view) >= 2 && !!view.source_busy;

export const confirmText = (label) => `会停止当前任务并切换到 ${label}，继续吗？`;

// A refusal that the one confirm answers, from a server that saw work the
// client did not.
export const refusalNeedsConfirm = (view, message) => handoffAPI(view) >= 2 && /停止当前任务/.test(message || "");

export function switchBody(view, target, requestId, { force = false } = {}) {
  const body = { action: "switch", target, request_id: requestId, source_id: view.source_id, confirm: true, continue: true };
  if (handoffAPI(view) < 2) body.continue = false; // an older server resumed without continuing
  if (force && handoffAPI(view) >= 2) body.force = true;
  return body;
}

export function answerBody(view, answer, requestId, target) {
  const body = { action: "answer", answer, request_id: requestId, source_id: view.source_id, confirm: true };
  if (target) body.target = target;
  return body;
}

export const undoBody = (view, requestId, force) => ({ action: "undo", request_id: requestId, source_id: view.source_id, confirm: true, ...(force ? { force: true } : {}) });

export const SESSION_RULES = [
  ["default", "跟随调度设置"], ["ask", "每次询问"], ["allow", "自动换账号继续"], ["wait", "等原账号恢复"],
];

export function sessionRule(view) {
  if (handoffAPI(view) < 2) return view?.switch_policy || "ask";
  return view.switch_policy_own ? view.switch_policy : "default";
}

export function chipState(view) {
  const session = view?.session || {}, sw = session.account_switch;
  if (sw && sw.phase !== "completed") return "正在交接";
  if (handoffQuestion(view)) return "额度已用完";
  if (view?.handoff?.waiting) return "等待额度恢复";
  if (session.account_failure) return "需要处理";
  return "会话绑定";
}
