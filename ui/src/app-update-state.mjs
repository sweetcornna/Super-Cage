// What the application-update button, its confirmation and the 设置 → 关于
// rows say. Inputs are the native shell's updater state (native
// "appUpdate"), GET /api/app-update (internal/appupdate/policy/status.go)
// and GET /api/app-update/interruptions (internal/workbench/app_update.go).
// No React and no network here; app-updates.jsx and the settings page draw it.

export const activeShellPhases = new Set(["checking", "downloading", "downloaded", "verifying", "installing"]);

const chainBusyLabels = {
  relaunched: "正在恢复环境",
  restoring: "正在恢复环境",
  components: "正在同步环境组件",
  tools: "正在更新环境工具",
  network: "正在检查网络",
};

// The action a failed post-update step offers (policy.NextAction).
const nextActions = {
  retry_update: { label: "重试升级", action: "confirm" },
  start_environment: { label: "启动环境", action: "start_environment" },
  update_components: { label: "更新环境组件", action: "update_components" },
  update_tools: { label: "更新环境工具", action: "update_tools" },
  check_network: { label: "检查联网", action: "check_network" },
};

// Codes in GET /api/app-update "waiting" that mean "nothing is wrong, the
// computer is simply not idle yet".
const idleWaits = new Set(["busy", "not_held", "changed", "user_active", "user_unknown", "too_new"]);

const versionOf = (shell, status) => shell?.latestVersion || status?.pending?.version || "";
const percentOf = (shell) => (shell?.totalBytes > 0 ? Math.max(0, Math.min(100, Math.floor(((shell.receivedBytes || 0) / shell.totalBytes) * 100))) : null);

// The gateway's plain-text refusal for a cloud connection
// (appUpdateLocalRequired in internal/workbench/app_update.go).
const localRequired = "应用升级由本机管理";

// Maps a failed GET /api/app-update (an Error carrying .status and .message,
// as ui/src/api.js throws them).
export function readStatusFailure(error) {
  if (!error) return null;
  const message = error.message || "暂时无法读取升级设置";
  if (error.status === 409 && message.includes(localRequired)) {
    return { kind: "local_required", message: "应用升级由本机管理，请切换到本机后设置" };
  }
  if (error.status === 423) return { kind: "locked", message: "解锁 Super Cage 后可查看升级设置" };
  if (error.status === 403) return { kind: "forbidden", message: "只能在本机 Super Cage 窗口中查看升级设置" };
  return { kind: "unreachable", message };
}

// "11月21日 03:12" in the given (or local) time zone.
export function formatWhen(iso, timeZone) {
  const date = new Date(iso || "");
  if (Number.isNaN(date.getTime())) return "";
  const parts = Object.fromEntries(
    new Intl.DateTimeFormat("en-US", { timeZone, month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit", hourCycle: "h23" })
      .formatToParts(date)
      .map((part) => [part.type, part.value]),
  );
  return `${parts.month}月${parts.day}日 ${parts.hour}:${parts.minute}`;
}

const hidden = { visible: false, label: "", hint: "", disabled: true, busy: false, tone: "normal", percent: null, action: null };
const show = (fields) => ({ ...hidden, visible: true, disabled: false, ...fields });

/**
 * The title-bar button.
 * @param {object} input
 * @param {object} input.shell        native updater state
 * @param {object|null} input.status  GET /api/app-update body, null when unavailable
 * @param {object} [input.environment] {running, networkOk, componentsCurrent}; unknown fields keep a failure visible
 * @param {number} [input.now]
 * @returns {{visible, label, hint, disabled, busy, tone, percent, action}}
 *   action: "confirm" opens the confirmation; "releases" opens the release
 *   page; the rest are the existing environment actions.
 */
export function appUpdateButton({ shell = {}, status = null, environment = {}, now = Date.now() } = {}) {
  const chain = status?.chain;
  const version = versionOf(shell, status);
  // 1. After a relaunch: the new version is bringing the environment back.
  if (chain && chainBusyLabels[chain.step]) {
    const label = chainBusyLabels[chain.step];
    return show({ label, busy: true, disabled: true, hint: chain.note || `已升级到 ${chain.to}，${label}，完成前请勿退出 Super Cage` });
  }
  if (chain && (chain.step === "stopping" || chain.step === "installing") && !activeShellPhases.has(shell.phase)) {
    return show({ label: "升级没有完成", tone: "failed", action: "confirm", hint: chain.note || "升级没有完成，原应用已保留；可以重新点击升级" });
  }
  if (chain?.step === "failed") {
    const next = nextActions[chain.next] || nextActions.retry_update;
    const settled =
      (chain.next === "start_environment" && environment.running === true) ||
      (chain.next === "check_network" && environment.networkOk === true) ||
      (chain.next === "update_components" && environment.componentsCurrent === true) ||
      (chain.next === "retry_update" && !version);
    if (!settled && !activeShellPhases.has(shell.phase)) {
      return show({ label: next.label, tone: "failed", action: next.action, hint: chain.error || "升级没有完成" });
    }
  }
  if (chain?.step === "attention" && !activeShellPhases.has(shell.phase)) {
    return show({ label: "升级需要处理", tone: "failed", action: version ? "confirm" : null, disabled: !version, hint: chain.error || "升级记录需要处理，可以重新点击升级" });
  }
  // 2. The shell is downloading, verifying or installing.
  if (activeShellPhases.has(shell.phase)) {
    const percent = shell.phase === "downloading" ? percentOf(shell) : null;
    const label =
      shell.phase === "downloading" ? `正在下载${percent === null ? "" : ` ${percent}%`}`
      : shell.phase === "verifying" || shell.phase === "downloaded" ? "正在验证"
      : shell.phase === "checking" ? "正在准备升级"
      : /停止/.test(shell.message || "") ? "正在停止环境"
      : "正在安装，完成后自动重启";
    return show({ label, busy: true, disabled: true, percent, hint: shell.message || label });
  }
  // 3. The workbench refused this click or this unattended attempt. The
  // shell reports it as a retryable failure carrying the refusal.
  if (shell.blocked && version) {
    const blocked = shell.blocked;
    if (blocked.code === "confirm_required") {
      return show({ label: `升级到 ${version}`, action: "confirm", tone: "waiting", hint: "有新的任务正在进行，请重新确认后再升级" });
    }
    return show({ label: `升级到 ${version}`, action: "confirm", tone: "waiting", hint: blocked.message || shell.message || "当前不能升级，请稍后再试" });
  }
  if (shell.phase === "failed" && version) {
    const retryAt = Date.parse(shell.retryAt || "");
    if (retryAt > now) {
      return show({ label: "等待重试", disabled: true, tone: "failed", hint: `${shell.message || "更新源请求受限"}，可于 ${formatWhen(shell.retryAt)} 后重试` });
    }
    return show({ label: "重试升级", tone: "failed", action: "confirm", hint: `${shell.message || "升级没有完成"}，点击重试` });
  }
  if (shell.phase === "manual" && version) {
    return show({ label: "查看新版本", action: "releases", hint: `新版本 ${version} 已发布。${shell.message || ""}` });
  }
  // 4. A newer version is ready to be installed with a click.
  if (version && (["available", "ready"].includes(shell.phase) || (status?.pending?.installable && !shell.phase))) {
    const label = `升级到 ${version}`;
    if (status?.mode === "silent") {
      if (status.pending?.requested) return show({ label, action: "confirm", hint: "电脑空闲，即将自动升级；也可以现在点击升级" });
      const waiting = status.waiting;
      if (waiting && waiting.code !== "allowed") {
        const tone = idleWaits.has(waiting.code) ? "waiting" : "normal";
        return show({ label, action: "confirm", tone, hint: `已开启空闲时自动静默更新。${waiting.message}；也可以现在点击升级` });
      }
      return show({ label, action: "confirm", hint: "已开启空闲时自动静默更新，电脑空闲时会自动升级；也可以现在点击升级" });
    }
    return show({ label, action: "confirm", hint: "点击后先列出会被中断的任务，确认后停止环境、安装并重启。不点击不会升级。" });
  }
  return hidden;
}

/**
 * The confirmation shown after the click, before anything stops.
 * @param {object} input
 * @param {string} input.version
 * @param {object|null} input.interruptions  GET /api/app-update/interruptions body
 * @param {{status?: number, message?: string, body?: string}|null} [input.error]  failed GET
 */
export function appUpdateConfirmation({ version, interruptions = null, error = null }) {
  const title = `升级到 ${version}`;
  const failure = readStatusFailure(error);
  if (failure || !interruptions) {
    const note =
      failure?.kind === "local_required"
        ? "当前连接的是云端工作区。升级只重启本机的 Super Cage，云端任务不受影响。"
        : "暂时无法列出本机正在进行的任务。升级前还会再检查一次，有任务时不会中断它。";
    return { title, body: "升级会重启 Super Cage。", note, items: [], mustWait: [], reopen: [], confirmed: [], canContinue: true, continueLabel: "继续升级", cancelLabel: "稍后", waitMessage: "" };
  }
  const items = (interruptions.blockers || []).map((b) => ({ id: b.id, label: b.label, unknown: !!b.unknown }));
  const mustWait = (interruptions.must_wait || []).map((b) => b.label);
  const reopen = (interruptions.reopen || []).map((t) => t.title).filter(Boolean);
  const running = !!interruptions.environment_running;
  let body = running ? "升级会停止环境并重启 Super Cage，完成后自动恢复原来的环境。" : "升级会重启 Super Cage。";
  body += items.length ? "以下内容会被中断：" : running ? "现在没有正在进行的任务。" : "";
  return {
    title,
    body,
    note: reopen.length ? `这些终端会关闭，升级后可以重新打开：${reopen.join("、")}` : "",
    items,
    mustWait,
    reopen,
    confirmed: interruptions.confirm || [],
    canContinue: mustWait.length === 0,
    continueLabel: "继续升级",
    cancelLabel: "稍后",
    waitMessage: waitMessage(mustWait),
  };
}

// "请等待「更新环境组件」完成后再升级" for jobs; other must-wait states are named.
function waitMessage(labels) {
  if (!labels.length) return "";
  if (labels.every((label) => label.endsWith("正在进行"))) {
    return `请等待${labels.map((label) => label.slice(0, -"正在进行".length)).join("、")}完成后再升级`;
  }
  return `以下情况处理完成后才能升级：${labels.join("、")}`;
}

const modeOptions = [
  { value: "manual", label: "手动更新（默认）", description: "有新版本时显示升级按钮，只有点击后才会下载和安装。" },
  { value: "silent", label: "空闲时自动静默更新", description: "电脑空闲、没有任何任务时自动完成升级，并恢复原来的环境。" },
];

function lastAttemptText(attempt, silent, timeZone) {
  if (!attempt) return "";
  const when = formatWhen(attempt.finished_at || attempt.started_at, timeZone);
  if (attempt.result === "ok") return `上次升级：已升级到 ${attempt.version}${when ? `（${when}）` : ""}`;
  const reason = attempt.reason || "升级没有完成";
  return `上次升级未完成：${reason}${silent ? "（不会自动重试同一版本）" : ""}`;
}

/**
 * The 应用更新 rows of 设置 → 关于.
 * @param {object} input
 * @param {object|null} input.status  GET /api/app-update body
 * @param {object|null} [input.error] failed GET
 * @param {string} [input.timeZone]   for tests; the local zone otherwise
 * @returns {{mode, options, rows: {key, text}[], editable: boolean}}
 */
export function appUpdateSettings({ status = null, error = null, timeZone } = {}) {
  const failure = readStatusFailure(error);
  if (failure || !status) {
    const options = modeOptions.map((option) => ({ ...option, disabled: true, reason: failure?.message || "" }));
    return { mode: "manual", options, editable: false, rows: [{ key: "unavailable", text: failure?.message || "正在读取升级设置…" }] };
  }
  const silent = status.mode === "silent";
  const options = modeOptions.map((option) => ({
    ...option,
    disabled: option.value === "silent" && !status.silent_available && !silent,
    reason: option.value === "silent" && !status.silent_available ? status.silent_unavailable_reason || "" : "",
  }));
  const rows = [];
  if (status.mode_error) rows.push({ key: "mode_error", text: status.mode_error });
  rows.push({
    key: "explain",
    text: silent
      ? "只在没有运行中的任务、终端命令、下载、手机或远程连接，且电脑 30 分钟无人操作时升级；失败会恢复原版本，并且不会自动重试同一版本。"
      : "不会自动更新。Super Cage 只会检查是否有新版本，不会自行下载或安装。",
  });
  if (!status.silent_available) rows.push({ key: "unavailable", text: `此安装方式不支持静默更新：${status.silent_unavailable_reason || "原因未知"}` });
  if (silent && status.pending) {
    const waiting = status.waiting;
    const text = !status.pending.installable
      ? `新版本 ${status.pending.version} 需要手动安装：${status.pending.reason || ""}`
      : status.pending.requested
        ? `新版本 ${status.pending.version} 已就绪，电脑空闲，即将自动升级`
        : waiting?.blockers?.length
          ? `新版本 ${status.pending.version} 已就绪，等待空闲：${waiting.blockers.map((b) => b.label).join("、")}`
          : `新版本 ${status.pending.version} 已就绪，${waiting?.message || "等待空闲"}`;
    rows.push({ key: "waiting", text });
  }
  const last = lastAttemptText(status.last_attempt, silent, timeZone);
  if (last) rows.push({ key: "last", text: last });
  return { mode: silent ? "silent" : "manual", options, editable: true, rows };
}
