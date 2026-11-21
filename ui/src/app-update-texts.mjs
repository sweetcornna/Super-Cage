// What the update confirmation and the 应用更新 rows of 设置 → 关于 say.
// Inputs are GET /api/app-update (internal/appupdate/policy/status.go) and
// GET /api/app-update/interruptions (internal/workbench/app_update.go). Only
// lazy chunks (the confirmation, the settings page) load this module.
import { formatWhen, readStatusFailure } from "./app-update-state.mjs";

/**
 * The confirmation shown after the click, before anything stops.
 * @param {object} input
 * @param {string} input.version
 * @param {object|null} input.interruptions  GET /api/app-update/interruptions body
 * @param {{status?: number, message?: string}|null} [input.error]  failed GET
 * @param {object|null} [input.blocked]  the shell's last refusal ({code, message, blockers}); its
 *   blockers stand in for the list when the workbench cannot be asked directly
 */
export function appUpdateConfirmation({ version, interruptions = null, error = null, blocked = null }) {
  const title = `升级到 ${version}`;
  const failure = readStatusFailure(error);
  if ((failure || !interruptions) && blocked?.blockers?.length) {
    interruptions = {
      blockers: blocked.blockers.filter((b) => b.interruptible),
      must_wait: blocked.blockers.filter((b) => !b.interruptible),
      confirm: blocked.blockers.filter((b) => b.interruptible).map((b) => b.id),
      reopen: [],
      environment_running: true,
    };
  }
  if (!interruptions) {
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
      ? "只在没有运行中的任务、终端命令、下载、手机或远程连接，电脑 30 分钟无人操作、没有通话或视频让屏幕常亮、接着电源或电量不低于一半时升级；失败会恢复原版本，并且不会自动重试同一版本。"
      : "不会自动更新。Super Cage 只会检查是否有新版本，不会自行下载或安装。",
  });
  if (!status.silent_available) rows.push({ key: "unavailable", text: status.silent_unavailable_reason || "此安装方式不支持静默更新" });
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
