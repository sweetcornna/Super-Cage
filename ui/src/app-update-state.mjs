// What the application-update button says. Inputs are the native shell's
// updater state (native "appUpdate") and GET /api/app-update
// (internal/appupdate/policy/status.go). No React and no network here;
// app-updates.jsx draws it. The confirmation and the 设置 → 关于 rows are in
// app-update-texts.mjs, which only lazy chunks load.

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
const idleWaits = new Set(["busy", "not_held", "changed", "user_active", "user_unknown", "too_new", "retry_later",
  "host_unknown", "dark_wake", "lid_closed", "low_battery", "display_held"]);

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
 * @param {object} [input.environment] {running, networkOk, componentsCurrent}; defaults to the
 *   status's own "environment" (sent while a chain has failed); unknown fields keep a failure visible
 * @param {number} [input.now]
 * @returns {{visible, label, hint, disabled, busy, tone, percent, action}}
 *   action: "confirm" opens the confirmation; "releases" opens the release
 *   page; the rest are the existing environment actions.
 */
export function appUpdateButton({ shell = {}, status = null, environment, now = Date.now() } = {}) {
  const chain = status?.chain;
  const seen = status?.environment;
  environment ??= seen ? { running: seen.running, networkOk: seen.network_ok, componentsCurrent: seen.components_current } : {};
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
  // 2. The shell is downloading, verifying or installing. A background check
  // (no newer version known yet) shows nothing.
  if (activeShellPhases.has(shell.phase) && version) {
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
    return show({ label: "查看新版本", action: "releases", hint: `新版本 ${version} 已发布。${shell.message || ""}点击打开发布页面。` });
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

// GET /api/app-update changes by itself only while silent mode waits for an
// idle computer or a post-update chain runs. Otherwise the window looks again
// on focus, visibility and update events only, and polls nothing.
export function statusNeedsWatch(status) {
  const step = status?.chain?.step;
  return status?.mode === "silent" || Boolean(step && !["done", "failed", "attention"].includes(step));
}
