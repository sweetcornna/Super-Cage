// Errors that only an environment restart clears; progress text ("正在重启环境…")
// and confirmation titles do not match.
export function needsEnvironmentRestart(message) {
  return typeof message === "string" && /需要重启环境|请[^，。；？]*?重启环境|停止并重启环境/.test(message);
}

export function isSafetyCheckMessage(message) {
  return typeof message === "string" && /关键(?:检查|检测)(?:尚未|未|失败|证据不完整)|完成关键检查|外网(?:未放行|保持关闭)|浏览器配置检查未通过/.test(message);
}

// The lease on this client's clock. A newer server sends
// safety.gate.lease_remaining_ms beside until (its own wall clock, projected
// on every read so that a host sleep does not leave it in the past); a status
// read keeps the remaining time as lease_end = when it arrived + remaining.
// Both lease_end and until are kept in whole seconds and the remaining time
// is dropped, so that answers that say the same stay equal (no re-render on
// every poll). An older server sends until only, which is compared as before.
export function withLeaseClock(status, receivedAt = Date.now()) {
  const gate = status?.safety?.gate;
  const remaining = gate?.lease_remaining_ms;
  if (!gate || typeof remaining !== "number" || !Number.isFinite(remaining)) return status;
  const { lease_remaining_ms: _remaining, ...rest } = gate;
  const lease_end = Math.round((receivedAt + Math.max(0, remaining)) / 1000) * 1000;
  const until = Date.parse(gate.until);
  return { ...status, safety: { ...status.safety, gate: { ...rest, lease_end,
    ...(Number.isFinite(until) && { until: new Date(Math.round(until / 1000) * 1000).toISOString() }) } } };
}

const leaseEnd = (gate) => (Number.isFinite(gate?.lease_end) ? gate.lease_end : Date.parse(gate?.until));

// Gate state plus, when it describes that state, the server's explanation
// (status.safety.explanation): why networking is closed or at risk, what
// happens next and what the user can do. Older servers send none.
export function networkState(status, now = Date.now()) {
  const state = gateState(status, now);
  const explanation = networkExplanation(status?.safety?.explanation, state.mode, now, status?.safety?.gate);
  return explanation ? { ...state, explanation } : state;
}

// What is short of memory when the explanation says so (round 2 field).
const MEMORY_CAUSES = ["environment", "browser", "machine"];

function networkExplanation(value, mode, now, gate) {
  if (!value || typeof value !== "object" || typeof value.state !== "string") return null;
  const fits =
    value.state === "unconfirmed"
      ? mode === "open"
      : value.state === "rechecking"
        ? mode === "closed" || mode === "checking"
        : mode === "closed";
  if (!fits) return null;
  const at = (text) => {
    const time = Date.parse(text);
    return Number.isFinite(time) ? time : null;
  };
  const seconds = (time) => (time === null ? null : Math.max(0, Math.ceil((time - now) / 1000)));
  const recheckAt = at(value.recheck_at);
  // The unconfirmed lease is the gate's own: its remaining time when the
  // server sent it, else lease_until as before.
  const leaseUntil = value.state === "unconfirmed" && Number.isFinite(gate?.lease_end) ? gate.lease_end : at(value.lease_until);
  // A newer server's stable sentences plus the one moment they count down to
  // (deadline_ms, unix milliseconds). The unconfirmed lease keeps the gate's
  // own clock reading.
  const deadline = Number.isFinite(value.deadline_ms)
    ? value.state === "unconfirmed" && Number.isFinite(leaseUntil) ? leaseUntil : value.deadline_ms
    : null;
  return {
    state: value.state,
    reason: typeof value.reason === "string" ? value.reason : "",
    next: typeof value.next === "string" ? value.next : "",
    advice: Array.isArray(value.advice) ? value.advice.filter((item) => typeof item === "string") : [],
    memoryShort: value.memory_short === true,
    memoryCause: value.memory_short === true && MEMORY_CAUSES.includes(value.memory_cause) ? value.memory_cause : "",
    recheckAt,
    recheckInSeconds: seconds(recheckAt),
    leaseUntil,
    leaseEndsInSeconds: seconds(leaseUntil),
    deadline,
  };
}

// What the workbench says about the gate when the server explains it
// (networkState(...).explanation): a short label for the status bar, a title,
// the server's reason and next step, a countdown and the advice as given.
// unconfirmed (still online) is a quiet hint; only check_required is an alarm.
const EXPLAINED = {
  unconfirmed: { tone: "quiet", label: "已联网 · 监测重试中", title: "联网监测正在重试", countdown: "lease", recheck: false },
  waiting_memory: { tone: "warn", label: "外网未放行 · 内存不足", title: "外网已暂停：环境内存不足", countdown: "recheck", recheck: true },
  recheck_scheduled: { tone: "warn", label: "外网未放行 · 等待自动重检", title: "外网已暂停，等待自动重检", countdown: "recheck", recheck: true },
  rechecking: { tone: "warn", label: "外网未放行 · 正在自动重检", title: "正在自动重检", countdown: "", recheck: false },
  check_required: { tone: "alarm", label: "外网未放行 · 需要重新检测", title: "外网需要重新检测", countdown: "", recheck: true },
  paused: { tone: "warn", label: "外网已手动暂停", title: "外网已手动暂停", countdown: "", recheck: true },
};

// The title says what is short; the server's reason and advice say how.
const MEMORY_TITLES = {
  environment: "外网已暂停：环境内存不足",
  browser: "外网已暂停：隔离浏览器内存不足",
  machine: "外网已暂停：这台机器内存不足",
};

// Advice that points at the environment's memory setting ("在资源设置中…",
// "改为自动（推荐）后可以调到 N GB", "可以把内存调到 N GB", "调高内存上限")
// gets a link to 设置 → 环境与恢复 → 计算资源. Advice about tabs, programs or
// the machine itself does not.
export function adviceAction(text) {
  return typeof text === "string" && /资源设置|调到\s*\d+(?:\.\d+)?\s*GB|内存上限/.test(text) ? "resources" : "";
}

export function countdownText(seconds) {
  if (!Number.isFinite(seconds)) return "";
  if (seconds <= 0) return "即将";
  if (seconds < 60) return `约 ${seconds} 秒后`;
  const minutes = Math.floor(seconds / 60);
  const rest = seconds % 60;
  return rest ? `约 ${minutes} 分 ${rest} 秒后` : `约 ${minutes} 分钟后`;
}

// The live countdown inside the next-step sentence: exact, not rounded.
export function countdownWords(seconds) {
  if (seconds < 60) return `${seconds} 秒后`;
  const minutes = Math.floor(seconds / 60);
  const rest = seconds % 60;
  return rest ? `${minutes} 分 ${rest} 秒后` : `${minutes} 分钟后`;
}

// What the recheck waits for, as the next sentence names it.
const MEMORY_SUBJECTS = { environment: "环境内存", browser: "浏览器内存", machine: "这台机器的内存" };

// The next-step sentence for a state that has a deadline, as [before, timer,
// after]: the one live countdown sits in the middle (null timer: the time has
// come). Other states are described by the server's own sentence.
function nextParts(explanation, seconds) {
  const when = seconds > 0 ? countdownWords(seconds) : null;
  switch (explanation.state) {
    case "waiting_memory": {
      const subject = MEMORY_SUBJECTS[explanation.memoryCause] || "内存";
      return when ? [`${subject}回落后会自动重检并恢复联网，最迟 `, when, "。"] : [`${subject}回落后会自动重检并恢复联网，马上就会重检。`, null, ""];
    }
    case "recheck_scheduled":
      return when ? ["将在 ", when, "自动重检，通过后恢复联网。"] : ["即将自动重检，通过后恢复联网。", null, ""];
    case "unconfirmed":
      return when ? ["监测恢复后会自动延长联网许可，否则外网将在 ", when, "自动暂停。"] : ["联网许可即将到期，监测确认后会自动延长。", null, ""];
    default:
      return null;
  }
}

export function explanationView(explanation, now = Date.now()) {
  if (!explanation) return null;
  const kind = EXPLAINED[explanation.state] || EXPLAINED.check_required;
  // A newer server's explanation carries its deadline: the reason is the
  // server's stable sentence and the next step one sentence with the single
  // live countdown in it. An older server's text and countdown stay as they were.
  const stable = explanation.deadline !== null && explanation.deadline !== undefined && Boolean(nextParts(explanation, 1));
  const at = stable ? explanation.deadline : kind.countdown === "lease" ? explanation.leaseUntil : kind.countdown === "recheck" ? explanation.recheckAt : null;
  const seconds = at === null || at === undefined ? null : Math.max(0, Math.ceil((at - now) / 1000));
  const when = countdownText(seconds);
  const parts = stable ? nextParts(explanation, seconds) : null;
  const countdown = seconds === null ? null : {
    at, seconds,
    text: stable ? "" : kind.countdown === "lease"
      ? (seconds > 0 ? `联网许可${when}到期，监测确认后会自动延长。` : "联网许可即将到期，监测确认后会自动延长。")
      : explanation.state === "waiting_memory"
        ? (seconds > 0 ? `最迟${when}自动完整重检。` : "即将自动完整重检。")
        : (seconds > 0 ? `${when}自动重检。` : "即将自动重检。"),
  };
  return {
    state: EXPLAINED[explanation.state] ? explanation.state : "check_required",
    tone: kind.tone,
    label: kind.label,
    title: explanation.state === "waiting_memory" && MEMORY_TITLES[explanation.memoryCause] || kind.title,
    reason: explanation.reason,
    next: parts ? parts.filter(Boolean).join("") : explanation.next,
    nextParts: parts,
    advice: explanation.advice.map((text) => ({ text, action: adviceAction(text) })),
    memoryShort: explanation.memoryShort,
    memoryCause: explanation.memoryCause || "",
    countdown,
    recheck: kind.recheck,
  };
}

function gateState(status, now) {
  if (!status?.vm_running) return { mode: "stopped", label: "环境已停止" };
  const gate = status.safety?.gate;
  if (!gate) return { mode: "unknown", label: "外网状态待确认" };
  const until = leaseEnd(gate);
  if (gate.mode === "open" && Number.isFinite(until) && until > now)
    return { mode: "open", label: "外网已放行" };
  if (gate.mode === "checking" && Number.isFinite(until) && until > now)
    return { mode: "checking", label: "正在检查" };
  if (gate.mode === "closed" || (Number.isFinite(until) && until <= now))
    return { mode: "closed", label: "外网已关闭" };
  return { mode: "unknown", label: "外网状态待确认" };
}

export function canonicalExitPolicy(value = {}) {
  return {
    mode: value.mode || "dynamic",
    scope: value.scope || "country",
    ip: (value.ip || "").trim(),
    country_code: (value.country_code || "").trim().toUpperCase(),
    region: (value.region || "").trim().replace(/\s+/g, " "),
    city: (value.city || "").trim().replace(/\s+/g, " "),
  };
}

export function reportForSharing(report) {
  return {
    policy: report.policy,
    created_at: report.created_at,
    checks: report.checks,
    scope: "采样与运行时策略检查；不代表平台账号资格或完整虚拟化审计",
  };
}

export function isGuestLocalURL(value) {
  if (value === "about:blank") return true;
  try {
    const url = new URL(value);
    return (
      ["http:", "https:"].includes(url.protocol) &&
      ["localhost", "127.0.0.1", "[::1]"].includes(url.hostname)
    );
  } catch {
    return false;
  }
}
