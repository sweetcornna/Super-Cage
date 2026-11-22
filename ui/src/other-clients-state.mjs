// Settings → 其他客户端: what GET /api/agents and the polled job say, as rows
// and dialogs. Pure: no React, no fetch. The routes and the job are the
// host's (design 4.1, 4.4); each shape assumed beyond that is read in one
// place below (normalizeEntry, agentJob, failureView).

import { AGENTS_PATH, entriesOf, isListed } from "./other-clients-nav.mjs";

export { AGENTS_PATH };
const JOB_KIND = /^agent-(install|update|remove):([a-z0-9][a-z0-9-]*)$/;

export const LEAD = "Claude Code 随环境提供。需要时，可以在这个环境中添加其他编程客户端。";
export const NOTE = "添加和更新通过所选代理下载，不会放行业务网络。安装期间不能新建终端，通常一分钟左右。";
export const STOPPED_LINE = "启动环境后可以添加。";
export const BUSY_LINE = "另一个操作仍在进行，完成后可以继续。";
export const EMPTY_LINE = "暂时没有可添加的客户端。";
export const ADDED_LINE = "已添加。可以在 新建 中打开。";
export const NOT_ALLOWED = "这个服务器的管理员未开放";
export const OWN_NOTE = "Super Cage 不会修改它。";

const isObject = (value) => value !== null && typeof value === "object" && !Array.isArray(value);
const text = (value) => (typeof value === "string" ? value.trim() : "");
const list = (value) => (Array.isArray(value) ? value.filter((item) => typeof item === "string" && item.trim()).map((item) => item.trim()) : []);
const count = (value) => (Number.isFinite(value) && value > 0 ? value : 0);

/** The entries and the environment of a GET /api/agents answer, or null when it is not one. */
export function normalizeAgents(payload) {
  const source = entriesOf(payload);
  if (!source) return null;
  const environment = isObject(payload?.environment) ? payload.environment : {};
  const entries = source.filter(isListed).map(normalizeEntry);
  return {
    entries,
    environment: {
      running: environment.running === true,
      arch: text(environment.arch),
      guestSystem: text(environment.guest_system),
      diskFreeMb: count(environment.disk_free_mb),
      // False while presence has never been read: an entry without `present` is then unknown, not absent.
      clientsKnown: environment.clients_known !== false,
    },
  };
}

function normalizeEntry(raw) {
  const present = isObject(raw.present) ? { by: raw.present.by === "own" ? "own" : "cage", version: text(raw.present.version), state: ["drifted", "unknown"].includes(raw.present.state) ? raw.present.state : "ok" } : null;
  const hosts = isObject(raw.hosts) ? raw.hosts : {};
  return {
    id: text(raw.id),
    name: text(raw.name) || text(raw.id),
    vendor: text(raw.vendor),
    kind: raw.kind === "app" ? "app" : "terminal",
    status: ["hidden", "withdrawn"].includes(raw.status) ? raw.status : "available",
    withEnvironment: raw.with_environment === true,
    personOnly: raw.person_only === true,
    pinnedVersion: text(raw.pinned_version),
    present,
    updateAvailable: raw.update_available === true,
    supported: raw.supported !== false,
    unsupportedCode: text(raw.unsupported_code),
    unsupportedReason: text(raw.unsupported_reason),
    allowed: raw.allowed !== false,
    operation: ["install", "update", "remove"].includes(raw.operation) ? raw.operation : "",
    lastFailure: isObject(raw.last_failure) ? { code: text(raw.last_failure.code) || "failed", message: text(raw.last_failure.message) } : null,
    downloadMb: count(raw.download_mb),
    diskMb: count(raw.disk_mb),
    hosts: { install: list(hosts.install), signin: list(hosts.signin), run: list(hosts.run), telemetry: list(hosts.telemetry) },
    telemetryNote: text(raw.telemetry_note),
    regionNote: text(raw.region_note),
    dataDirs: Array.isArray(raw.data_dirs) ? raw.data_dirs : [],
    mark: isObject(raw.mark) ? { builtin: text(raw.mark.builtin), monogram: text(raw.mark.monogram) } : { builtin: "", monogram: "" },
  };
}

/** The agent-* job in the polled /status, or null for any other job. */
export function agentJob(job) {
  const match = JOB_KIND.exec(job?.kind || "");
  if (!match) return null;
  return {
    operation: match[1],
    id: match[2],
    running: job.running === true,
    error: text(job.error),
    canceled: job.canceled === true,
    progress: isObject(job.setup_progress) ? job.setup_progress : null,
    jobId: text(job.id),
  };
}

/**
 * The running job to show for the section: the polled one, except in the moment
 * between accepting a request and the next /status poll (up to 5 s while nothing
 * runs), when the accepted job (local.jobId) stands in; a refused request
 * (local.error) shows as a failed job. A job that has ended is not a job here: its
 * outcome is the entry's last_failure, read from the host's list.
 */
export function currentJob(raw, local) {
  const polled = agentJob(raw);
  if (local?.error) return { operation: local.operation, id: local.id, running: false, error: local.error, canceled: false, progress: null, jobId: "" };
  if (local && raw?.id !== local.jobId) return { operation: local.operation, id: local.id, running: true, error: "", canceled: false, progress: null, jobId: local.jobId };
  return polled?.running ? polled : null;
}

/** A job of this section has just ended: refresh the list once (never by a timer). */
export const jobEnded = (previous, next) => Boolean(previous?.running && agentJob(previous) && !next?.running);

const megabytes = (bytes) => {
  const value = bytes / 1048576;
  return value >= 10 || Number.isInteger(value) ? String(Math.round(value)) : value.toFixed(1);
};
export function formatMb(mb) {
  if (!(mb > 0)) return "";
  return mb >= 1024 ? (mb / 1024).toFixed(1) + " GB" : Math.round(mb) + " MB";
}
export const sizeLine = (entry) =>
  entry.downloadMb ? "下载约 " + formatMb(entry.downloadMb) + (entry.diskMb ? "，安装后约 " + formatMb(entry.diskMb) : "") : "";

/** What the progress bar and its line say for a running job of this section. */
export function progressView(job) {
  const progress = job.progress || {};
  // The host's progress is {phase: "agent", operation: <the installer's step>, label, received?, total?}.
  const step = progress.phase === "agent" ? text(progress.operation) : text(progress.phase);
  const label = text(progress.label);
  const received = Number.isFinite(progress.received) ? progress.received : null;
  const total = count(progress.total);
  const fraction = received !== null && total ? Math.max(0, Math.min(1, received / total)) : null;
  if (job.operation === "remove") return { fraction: null, text: "正在移除" };
  if (fraction !== null && (step === "download" || label === "下载")) {
    return { fraction, text: "正在下载 " + megabytes(received) + " / " + megabytes(total) + " MB" };
  }
  // 未完成 is the step between the installer's last line and the end of a failed job.
  const phase = step === "failed" ? "未完成" : label ? (label.startsWith("正在") ? label : "正在" + label) : job.operation === "update" ? "正在更新" : "正在准备";
  return { fraction, text: phase };
}

// Lines whose cause a retry cannot change (agent-install.sh, docs/agent-install.md):
// the person's own install, a withdrawn entry, a pinned file the vendor removed,
// another architecture, libc, namespaces or a keyring this environment lacks.
// By code (the host's last_failure): a retry cannot change these.
const NO_RETRY_CODES = new Set(["withdrawn", "arch", "libc", "keyring", "userns", "own-install", "gone", "input"]);
/** A failed job as the host reports it ({code, message}): canceled is quiet, some codes have no retry. */
export const failureOf = ({ code, message }) => ({
  line: message || (code === "canceled" ? "已取消，已下载的部分会在下次继续使用。" : "操作未完成。"),
  tone: code === "canceled" ? "muted" : "error",
  retry: !NO_RETRY_CODES.has(code),
});
// A refused request has no code, only the host's sentence: judge by it.
const NO_RETRY = [/自行安装/, /不再受 Super Cage 支持/, /已不再提供/, /固定版本适用于/, /需要 glibc|需要 musl/, /命名空间/, /密钥环/, /参数无效/];
/** How a failed job's line is shown: canceled is not an error, and some lines have no retry. */
export function failureView(line, canceled = false) {
  const cancelled = canceled || /^已取消/.test(line);
  return { tone: cancelled ? "muted" : "error", retry: !NO_RETRY.some((pattern) => pattern.test(line)) };
}

const monogram = (entry) => {
  if (entry.mark.monogram) return entry.mark.monogram;
  const letters = entry.name.replace(/[^A-Za-z]/g, "");
  return letters ? letters[0].toUpperCase() + (letters[1] || "").toLowerCase() : entry.name.slice(0, 1);
};
const markOf = (entry) => {
  const builtin = entry.mark.builtin || (["claude", "codex"].includes(entry.id) ? entry.id : "");
  return builtin ? { builtin } : { monogram: monogram(entry) };
};

/** What a retry repeats when the operation that failed is not known (a failure from before this visit). */
const fallbackOperation = (entry) => (!entry.present || entry.present.state === "drifted" ? "install" : entry.updateAvailable ? "update" : "remove");

/**
 * One row. context: { environment, job (currentJob, any entry), anyJobRunning,
 * added (the id just added, for the one-time line), operations (id -> the
 * operation asked for in this visit, for 重试) }.
 * actions: { id, label, kind: "secondary" | "ghost", disabled }; menu lists the
 * quiet actions behind ⋯.
 */
export function rowFor(entry, { environment, job = null, anyJobRunning = false, added = "", operations = {} }) {
  const base = { id: entry.id, name: entry.name, mark: markOf(entry), muted: false, bar: null, error: null, actions: [], menu: [], parts: [entry.vendor, entry.kind === "app" ? "应用" : "终端"].filter(Boolean), meta: "" };
  const mine = job && job.id === entry.id ? job : null;
  const idle = environment.running && !anyJobRunning;
  const act = (id, label, kind = "secondary") => ({ id, label, kind, disabled: !idle });
  const version = entry.present?.version || entry.pinnedVersion;
  const quiet = entry.present?.by === "cage" ? [act("remove", "移除")] : [];

  // Running: this visit's job, or the host's word that one of this entry runs.
  const operation = mine?.running ? mine.operation : entry.operation;
  if (operation) {
    const view = mine?.running ? progressView(mine) : { fraction: null, text: operation === "remove" ? "正在移除" : "正在准备" };
    return {
      ...base, state: operation === "remove" ? "removing" : "installing", meta: view.text, bar: operation === "remove" ? null : { fraction: view.fraction },
      parts: entry.present ? [version] : base.parts,
      actions: operation === "remove" ? [] : [{ id: "cancel", label: "取消", kind: "ghost", disabled: false }],
    };
  }
  // Failed: a refused request of this visit, or the host's last_failure. The person's own
  // install is no failure: the list already says so, and the row shows it.
  const failure = mine?.error ? { ...failureView(mine.error), line: mine.error, operation: mine.operation } : entry.lastFailure ? { ...failureOf(entry.lastFailure), operation: operations[entry.id] || fallbackOperation(entry) } : null;
  if (failure && entry.present?.by !== "own") {
    return {
      ...base, state: "failed", parts: entry.present ? [version] : base.parts, error: { line: failure.line, tone: failure.tone },
      actions: failure.retry ? [act("retry:" + failure.operation, "重试")] : [], menu: quiet,
    };
  }
  if (entry.status === "withdrawn") {
    return { ...base, state: "withdrawn", parts: [version], meta: "不再受支持", actions: [], menu: [act("remove", "移除")] };
  }
  if (!entry.allowed) return { ...base, state: "not-allowed", muted: true, meta: NOT_ALLOWED };
  if (!entry.supported) return { ...base, state: "unsupported", muted: true, meta: entry.unsupportedReason || "这个环境暂不支持" };
  if (entry.present?.by === "own") {
    return { ...base, state: "own", parts: ["已安装（自行安装）", version].filter(Boolean), meta: OWN_NOTE };
  }
  if (entry.present) {
    const drifted = entry.present.state === "drifted";
    const update = entry.updateAvailable && !drifted;
    return {
      ...base, state: drifted ? "drifted" : update ? "update" : "installed", parts: [version],
      meta: drifted ? "程序文件与安装时不同" : update ? "有新版本 " + entry.pinnedVersion : added === entry.id ? ADDED_LINE : "",
      actions: drifted ? [act("repair", "修复")] : update ? [act("update", "更新")] : [],
      menu: [act("remove", "移除")],
    };
  }
  return { ...base, state: "available", meta: sizeLine(entry), actions: [act("add", "添加")] };
}

/** The line under the list that explains disabled buttons, if any. */
export const waitLine = (environment, anyJobRunning, mineRunning) => (!environment.running ? STOPPED_LINE : anyJobRunning && !mineRunning ? BUSY_LINE : "");

const where = (host) => (/(^|\.)github\.com$/.test(host) ? "GitHub" : host);
const names = (hosts, limit) => (hosts.length > limit ? hosts.slice(0, limit).join("、") + " 等" : hosts.join("、"));

/** The add dialog's rows (design 8.3). */
export function addDialog(entry, environment) {
  const host = entry.hosts.install[0];
  const rows = [];
  if (entry.pinnedVersion) rows.push({ label: "来源", text: (entry.vendor || entry.name) + (host ? " 在 " + where(host) + " 发布的 " : " 发布的 ") + entry.pinnedVersion + "，已核对校验值" });
  const size = sizeLine(entry);
  if (size) rows.push({ label: "占用", text: size + (environment.diskFreeMb ? "（客机剩余 " + formatMb(environment.diskFreeMb) + "）" : "") });
  const groups = [["下载", entry.hosts.install], ["登录", entry.hosts.signin], ["使用", entry.hosts.run], ["遥测上报", entry.hosts.telemetry]].filter(([, hosts]) => hosts.length);
  if (groups.length) {
    const summary = groups.filter(([label]) => label !== "遥测上报").map(([label, hosts]) => label + " " + names(hosts, 2)).join(" · ");
    // The full lists open only when the line above leaves something out.
    const more = groups.some(([, hosts]) => hosts.length > 2) || entry.hosts.telemetry.length > 0;
    rows.push({ label: "联网", text: summary, ...(more ? { details: groups.map(([label, hosts]) => ({ label, hosts })) } : {}) });
  }
  if (entry.telemetryNote) rows.push({ label: "遥测", text: entry.telemetryNote });
  if (entry.personOnly) rows.push({ label: "条款", text: (entry.vendor || entry.name) + " 的条款不允许由其他程序代为运行它，所以它只在你自己打开的终端中使用。" });
  if (entry.regionNote) rows.push({ label: "地区", text: entry.regionNote });
  rows.push({ label: "安装期间", text: "不能新建终端（包括 Claude 终端），约一分钟。" });
  return { title: "添加 " + entry.name + "？", rows, confirm: "添加" };
}

/** The remove dialog (design 8.3): data is kept unless the box is ticked. */
export const removeDialog = (entry) => ({
  title: "移除 " + entry.name + "？",
  body: "程序文件会被删除。登录信息、账号和会话记录默认保留，下次添加后可以继续使用。",
  checkbox: entry.dataDirs.length ? "同时删除登录信息和设置" : "",
  confirm: "移除",
});

/** The request an action makes: [path, body]. */
export function requestFor(action, id, { removeData = false, job = "" } = {}) {
  const [name, operation] = action.split(":");
  const kind = name === "retry" ? operation : name === "add" || name === "repair" ? "install" : name;
  if (kind === "cancel") return [AGENTS_PATH + "/cancel", { job }];
  return [AGENTS_PATH + "/" + encodeURIComponent(id) + "/" + kind, kind === "remove" && removeData ? { remove_data: true } : {}];
}
