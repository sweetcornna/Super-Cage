// The pane's view of Claude Science: one state at a time, with its copy and
// actions. Maps GET /api/claude-science (contract: the comment block in
// internal/workbench/claude_science.go) and the workbench job status to a plain
// description; claude-science.jsx only draws it. No React and no network here.

const PREFIX = "claude-science-";
const ACTIONS = ["install", "update", "start", "stop"];

// Why a start failed (failure_reason), in plain Chinese.
const failureReasons = {
  port_in_use: "Claude Science 需要的本机端口已被其他程序占用，未能启动。请关闭占用端口的程序，或重启环境后重试。",
  "oom-kill": "环境内存不足，Claude Science 已被系统关闭。请在设置中检查计算资源后重试。",
  exit: "Claude Science 启动后意外退出。请重试；仍然失败时，在操作详情中查看日志。",
};
const unknownFailure = "Claude Science 未能启动。请重试；仍然失败时，在操作详情中查看日志。";

const jobFailureLabels = { install: "安装未完成", update: "更新未完成", start: "启动未完成", stop: "停止未完成" };
const progressFallbacks = {
  install: "正在下载并安装 Claude Science…",
  update: "正在更新 Claude Science…",
  start: "正在启动 Claude Science…",
  stop: "正在停止 Claude Science…",
};

export const signInNote = "登录在打开的页面中完成，整个过程发生在隔离浏览器内，与 Claude Desktop 的登录方式相同。";

// 11264 -> "11 GB", 1536 -> "1.5 GB", 512 -> "512 MB".
export function formatMB(mb) {
  if (!(mb > 0)) return "";
  if (mb < 1024) return Math.round(mb) + " MB";
  return Number((mb / 1024).toFixed(1)) + " GB";
}

const lines = (text) => String(text || "").split(/\r?\n/).map((line) => line.trim()).filter(Boolean);

// The server's refusal reason is the last line of the job log. When that line
// is not Chinese prose (a raw tool message), the job's own error reads better.
export function jobFailureReason(job) {
  const last = lines(job?.log).at(-1) || "";
  if (/\p{Script=Han}/u.test(last)) return last;
  return job?.error || last || "操作没有完成。请在操作详情中查看日志。";
}

export const ownJob = (job) => typeof job?.kind === "string" && job.kind.startsWith(PREFIX);
const jobAction = (job) => (ownJob(job) ? job.kind.slice(PREFIX.length) : "");

// Maps a failed GET (an Error carrying .status and the server's Chinese text).
export function readFailure(error) {
  if (!error) return null;
  const message = error.message || "读取 Claude Science 状态失败。";
  if (error.status === 409) return { kind: "environment", message };
  if (error.status === 423) return { kind: "locked", message };
  return { kind: "unreachable", message };
}

/**
 * @param {object} input
 * @param {object|null} input.data       last GET /api/claude-science body
 * @param {{status?: number, message: string}|null} input.error  last failed GET
 * @param {string} input.actionError     message of the last refused action
 * @param {object} [input.job]           status.job of the workbench
 * @param {boolean} input.running        the environment (VM) is running
 * @param {boolean} input.ready          the guest browser is ready
 * @param {string} [input.pending]       action this pane has submitted and not seen finish
 */
export function scienceView({ data = null, error = null, actionError = "", job = null, running = false, ready = false, pending = "" }) {
  const view = {
    state: "",
    tone: "",
    drawing: "",
    title: "",
    lead: "",
    note: "",
    chips: [],
    facts: [],
    progress: null,
    log: [],
    primary: null,
    secondary: [],
    hint: "",
    notices: [],
    busy: false,
  };
  const own = ownJob(job);
  const jobRunning = Boolean(job?.running);
  const elsewhere = jobRunning && !own;
  const operation = (jobRunning && own ? jobAction(job) : "") || (data?.operation && ACTIONS.includes(data.operation) ? data.operation : "") || (ACTIONS.includes(pending) ? pending : "");
  view.busy = Boolean(pending) || jobRunning || Boolean(operation);
  const idle = !view.busy;
  const action = (name, label, extra = {}) => ({ action: name, label, disabled: !idle, ...extra });
  const failure = readFailure(error);

  // Errors that do not replace the pane: a refused action, a failed re-read
  // while an earlier answer is still on screen, a failed job.
  if (actionError) view.notices.push({ tone: "error", text: actionError });
  if (failure && data) view.notices.push({ tone: "error", text: failure.message });
  if (!pending && !jobRunning && own && job.error) {
    view.notices.push({ tone: "error", label: jobFailureLabels[jobAction(job)] || "操作未完成", text: jobFailureReason(job), details: true });
  }

  if (data && data.supported === false) {
    // Needs no running environment: the server answers from host facts.
    view.state = "unsupported";
    view.drawing = "plug";
    view.title = "这个环境暂不能使用 Claude Science";
    view.lead = data.unsupported_reason || "当前环境暂不支持 Claude Science。";
    return view;
  }
  if (!running) {
    view.state = "environment-stopped";
    view.drawing = "server";
    view.title = "先启动 Super Cage 环境";
    view.lead = "Claude Science 运行在环境内。请在工作台启动环境，再回到这里安装或打开。";
    return view;
  }
  if (!data) {
    if (failure?.kind === "locked") {
      view.state = "locked";
      view.title = "Super Cage 已锁定";
      view.lead = failure.message;
      view.notices = view.notices.filter((notice) => notice.text !== failure.message);
      view.primary = { action: "refresh", label: "重新读取状态", disabled: false };
    } else if (failure?.kind === "environment") {
      view.state = "environment-stopped";
      view.drawing = "server";
      view.title = "先启动 Super Cage 环境";
      view.lead = failure.message;
      view.primary = { action: "refresh", label: "重新读取状态", disabled: false };
    } else if (failure) {
      view.state = "unreachable";
      view.title = "暂时读不到 Claude Science 的状态";
      view.lead = failure.message;
      view.primary = { action: "refresh", label: "重新读取状态", disabled: false };
    } else {
      view.state = "loading";
      view.title = "正在读取 Claude Science 状态…";
    }
    return view;
  }

  const version = data.version ? "v" + data.version : "";
  const versionChip = version ? [{ text: version, tone: "" }] : [];
  const updateTo = data.pinned_version ? "更新到 " + data.pinned_version : "更新";
  const updateChips = [];
  if (data.update_available) updateChips.push({ text: "有新版本", tone: "busy" });
  if (data.update_required === true) updateChips.push({ text: "需要更新", tone: "warn" });
  if (data.update_required === true) {
    view.notices.push({ tone: "warn", text: data.update_hint || "Anthropic 要求更新 Claude Science。" + (data.update_via === "super-cage" ? "请先更新 Super Cage。" : "") });
  }
  // The pinned build can fix a required update only when it is newer than what is installed.
  const updatable = data.installed && (data.update_available || (data.update_required === true && data.update_via === "claude-science"));
  const updateRequiredNow = data.update_required === true && data.update_via === "claude-science";
  if (elsewhere) view.progress = { own: false, message: "环境正在进行另一项操作：" + (job.message || "请稍候") + "。完成后可继续。" };

  if (operation) {
    view.state = { install: "installing", update: "updating", start: "starting", stop: "stopping" }[operation];
    view.tone = "busy";
    view.title = { install: "正在安装 Claude Science", update: "正在更新 Claude Science", start: "正在启动 Claude Science", stop: "正在停止 Claude Science" }[operation];
    view.lead = {
      install: "正在从 Anthropic 官方源下载并校验，完成后回到这里启动。",
      update: "会先停止 Claude Science，再安装新版本。项目和登录状态保留。",
      start: "首次启动会准备 Python 与 R 环境，可能需要几分钟。",
      stop: "正在停止，稍候即可。",
    }[operation];
    view.chips = [...versionChip];
    view.progress = {
      own: true,
      message: (own && jobRunning && job.message) || progressFallbacks[operation],
    };
    if (own && jobRunning) view.log = lines(job.log).slice(-6);
    return view;
  }

  if (!data.installed) {
    view.state = "not-installed";
    view.drawing = "tray";
    view.title = "在 Super Cage 中使用 Claude Science";
    view.lead = "Anthropic 的科研与数据分析应用，在环境内运行，页面在隔离浏览器中打开。";
    const required = data.disk_required_mb;
    const free = data.disk_free_mb;
    const short = required > 0 && free > 0 && free < required;
    if (required > 0) view.facts.push({ label: "需要空间", value: "至少 " + formatMB(required) });
    if (free > 0) view.facts.push({ label: "当前可用", value: formatMB(free), tone: short ? "warn" : "" });
    view.note = "程序约 170 MB，从官方源下载并校验；首次启动再准备约 10 GB 的运行环境。";
    if (short) view.hint = "可用空间不足，请先清理环境内的文件，再安装。";
    view.primary = action("install", "安装 Claude Science", { disabled: !idle || short });
    return view;
  }

  if (data.running && data.ready) {
    view.state = "running";
    view.tone = "ok";
    view.title = "Claude Science 正在运行";
    view.lead = "页面会在浏览器窗格的新标签页中打开。";
    view.note = signInNote;
    view.chips = [...versionChip, ...updateChips];
    if (data.signed_in === true) view.chips.splice(1, 0, { text: "已登录", tone: "ok" });
    if (updateRequiredNow) {
      view.primary = action("update", updateTo, { confirm: true });
      view.secondary.push(action("open", "打开 Claude Science", { disabled: !idle || !ready }));
    } else {
      view.primary = action("open", "打开 Claude Science", { disabled: !idle || !ready });
      if (!ready) view.hint = "浏览器正在准备，稍后即可打开。";
      if (updatable) view.secondary.push(action("update", updateTo, { confirm: true }));
    }
    view.secondary.push(action("stop", "停止", { confirm: true }));
    return view;
  }

  if (data.running) {
    view.state = "starting";
    view.tone = "busy";
    view.title = "正在启动 Claude Science";
    view.lead = "服务已运行，正在等待它响应。首次启动需要准备 Python 与 R 环境，可能要几分钟。";
    view.chips = [...versionChip];
    view.progress = { own: true, message: progressFallbacks.start };
    view.secondary.push(action("stop", "停止", { confirm: true }));
    return view;
  }

  if (data.failed) {
    view.state = "failed";
    view.tone = "error";
    view.title = "Claude Science 未能启动";
    view.lead = failureReasons[data.failure_reason] || unknownFailure;
    view.chips = [...versionChip, ...updateChips];
    view.primary = action("start", "重试");
    if (updatable) view.secondary.push(action("update", updateTo, { confirm: false }));
    return view;
  }

  view.state = "stopped";
  view.title = "Claude Science 已安装";
  view.lead = "启动后在浏览器窗格中打开。首次启动会准备 Python 与 R 环境，可能需要几分钟。";
  view.tone = "idle";
  view.chips = [...versionChip, ...updateChips];
  view.primary = updateRequiredNow ? action("update", updateTo) : action("start", "启动");
  if (updateRequiredNow) view.secondary.push(action("start", "启动"));
  else if (updatable) view.secondary.push(action("update", updateTo, { confirm: false }));
  return view;
}

// Host rendering (phase 2, internal/workbench/science_host.go): the Windows
// window shows the guest's Claude Science page in a frame, with the guest's
// time zone, language and browser identity. Off by default.
export const hostRenderLabel = "本机渲染（实验性）";
export const hostRenderExplanation = "在这个窗口中直接显示 Claude Science，页面看到的仍是环境的时区、语言和浏览器标识。";
// The scienceFrame bridge version this pane needs (science_host.go
// scienceBridgeContract): 2 gives the guest's identity to the frame's own
// targets only. The window answers op "capabilities".
export const hostRenderContract = 2;
export const hostRenderUpdateNotice = "请更新 Windows 应用后再使用本机渲染：这个窗口还不能把环境的身份只交给 Claude Science 页面。";
export const hostRenderGuide = "https://github.com/sweetcornna/Super-Cage/blob/main/docs/guide/claude-science.md#本机渲染实验性";

/**
 * The switch and whether the pane is the frame.
 * @param {object} input
 * @param {object|null} input.data       last GET /api/claude-science body (host_render inside)
 * @param {object} [input.client]        client context (desktop_gateway, mode, client_platform)
 * @param {boolean} input.bridge         the window's native bridge is present
 * @param {?number} input.contract       what the bridge's scienceFrame
 *                                       capabilities op answered (null: not
 *                                       yet; 0: no such op)
 * @param {boolean} input.running        the environment runs
 * @param {boolean} input.locked         the last read answered 423
 * @param {boolean} input.busy           a Claude Science job runs or is pending
 * @param {string} [input.fallback]      why this pane went back to the browser pane
 */
// hostRenderHere: the Windows window on this computer's environment, the
// only place the switch can show (and the bridge is asked for its contract).
export const hostRenderHere = (client = {}, bridge = false) =>
  Boolean(client.desktop_gateway) && client.mode === "local" && client.client_platform === "windows" && Boolean(bridge);

export function hostRenderView({ data = null, client = {}, bridge = false, contract = 0, running = false, locked = false, busy = false, fallback = "" }) {
  const view = { toggle: null, frame: false, fallback: "", notice: "" };
  const render = data?.host_render;
  if (!render || !data.installed) return view;
  const here = hostRenderHere(client, bridge);
  // Only where it can be used: elsewhere (macOS, Linux, a browser, the
  // phone, a remote host) the pane stays as it was and the server refuses
  // the switch anyway.
  if (!render.available || !here || contract === null) return view;
  // F8: a window whose bridge predates the frame contract is not offered
  // the switch; one that is already on stays so it can be turned off.
  const capable = Number(contract) >= hostRenderContract;
  if (!capable && !render.enabled) return view;
  view.toggle = {
    label: hostRenderLabel,
    checked: Boolean(render.enabled),
    disabled: busy,
    explanation: hostRenderExplanation,
  };
  if (render.error) view.notice = render.error;
  if (!capable) {
    view.notice = hostRenderUpdateNotice;
    return view;
  }
  const daemonReady = Boolean(data.installed && data.running && data.ready);
  view.frame = Boolean(render.enabled && render.running) && running && !locked && !busy && daemonReady && !fallback;
  if (render.enabled && fallback) view.fallback = fallback;
  return view;
}

/**
 * The frame's toolbar: version, sign-in state and one line of status.
 * @param {object} data   GET /api/claude-science body
 * @param {string} phase  "preparing" | "loading" | "ready"
 */
export function hostFrameStatus(data, phase) {
  const render = data?.host_render || {};
  const chips = [];
  if (data?.version) chips.push({ text: "v" + data.version, tone: "" });
  if (data?.signed_in === true) chips.push({ text: "已登录", tone: "ok" });
  if (data?.update_available) chips.push({ text: "有新版本", tone: "busy" });
  let text = "时区、语言与浏览器标识来自环境";
  if (phase === "preparing") text = "正在核对页面的时区与语言…";
  else if (phase === "loading") text = "正在打开…";
  else if (render.blocked) text = "已拦截一次带有本机时区或语言的请求";
  else if (render.audit_note) text = render.audit_note;
  return { chips, text, tone: render.blocked ? "warn" : phase === "ready" ? "ok" : "busy" };
}
