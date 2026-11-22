// Pure state of the "本机终端" feature on this computer: the line that says
// which application opens, the two switches, the menu item "在本机终端中打开"
// and the request that opens a terminal there. There is no application to
// choose: Ghostty opens it when it is installed, the system Terminal
// otherwise (internal/localterm). No DOM and no fetch, so node --test covers it.

export const MENU_LABEL = "在本机终端中打开";
export const MENU_SHORTCUT = "⌘⇧O";
export const MENU_ICON = "square-arrow-out-up-right";
export const SETTINGS_PATH = "/client/local-terminal";
export const APP_PATH = "/client/local-terminal/app";
export const OPEN_PATH = "/client/local-terminal/open";

export const DEFAULT_SETTINGS = Object.freeze({ image_upload: true, clipboard_write: true });

export const SWITCHES = Object.freeze([
  {
    key: "image_upload",
    title: "上传拖入的图片",
    help: "把图片拖入本机终端应用的窗口，或粘贴的内容只有图片文件的完整路径时，会上传到环境，并粘贴环境中的路径。只处理 PNG、JPEG、WebP 和 GIF 图片；其他文件和输入的文字原样发送。从你自己的终端 ssh 连进来时不上传，按文字粘贴。",
  },
  {
    key: "clipboard_write",
    title: "允许环境写入剪贴板",
    help: "环境中的程序（例如 neovim、tmux）复制的内容会写入这台电脑的剪贴板，与网页终端一致；从你自己的终端 ssh 连进来时也一样。环境始终无法读取这台电脑的剪贴板。",
  },
]);

// An older build also kept the application that was chosen; it is not read.
export function normalizeSettings(raw) {
  const value = raw && typeof raw === "object" ? raw : {};
  return {
    image_upload: typeof value.image_upload === "boolean" ? value.image_upload : true,
    clipboard_write: typeof value.clipboard_write === "boolean" ? value.clipboard_write : true,
  };
}

export function settingsChanged(saved, draft) {
  const a = normalizeSettings(saved);
  const b = normalizeSettings(draft);
  return a.image_upload !== b.image_upload || a.clipboard_write !== b.clipboard_write;
}

// openerLine is the one line of the settings section about the application:
// app is what GET /client/local-terminal/app found ({id, name}), or null.
export function openerLine(app, supported = true) {
  if (!supported) return "这台电脑的系统暂不支持在本机终端中打开，终端会在 Super Cage 的标签页中打开";
  if (!app) return "这台电脑上找不到 Ghostty 或系统终端，暂时不能在本机终端中打开";
  if (app.id === "ghostty") return "用 Ghostty 打开";
  return "用系统终端打开；安装 Ghostty 后自动改用 Ghostty";
}

const terminalID = /^[0-9a-f]{16}$/;
const sessionID = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;
const accountID = /^[A-Za-z0-9_-]{1,128}$/;
const kinds = new Set(["shell", "claude", "codex"]);
// Any control character (C0, DEL, C1) or a lone surrogate, as the desktop
// and cage attach refuse: the path reaches a command line and the clipboard.
const controls = /[\p{Cc}\p{Cs}]/u;

// targetProblem says why a terminal, project or session cannot be opened.
function targetProblem(target) {
  switch (target?.kind) {
    case "terminal":
      if (!terminalID.test(target.id || "")) return "无法识别这个终端";
      if (target.closed) return "终端已结束";
      return "";
    case "project":
      if (typeof target.path !== "string" || !target.path.startsWith("/") || controls.test(target.path)) return "无法识别这个项目";
      if (target.run && !kinds.has(target.run)) return "无法识别运行内容";
      return "";
    case "session":
      return sessionID.test(target.id || "") ? "" : "无法识别这个会话";
  }
  return "无法识别要打开的内容";
}

// menuItem is "在本机终端中打开" for a terminal tab, a project row or a
// session row. It is hidden outside the desktop app and where the desktop
// cannot open one (supported is false). It never detects an application:
// the desktop finds the one to use when the item is chosen.
export function menuItem({ supported = true, target, context, environment } = {}) {
  if (!supported || !context?.desktop_gateway) return { visible: false };
  const item = {
    visible: true,
    label: MENU_LABEL,
    icon: MENU_ICON,
    shortcut: target?.kind === "terminal" ? MENU_SHORTCUT : "",
    enabled: false,
    reason: "",
  };
  if (!context.connection_id || context.mode === "none") return { ...item, reason: "先选择一个工作环境" };
  if (environment?.locked) return { ...item, reason: "Super Cage 已锁定" };
  if (environment && environment.running === false) return { ...item, reason: "环境已停止" };
  const problem = targetProblem(target);
  if (problem) return { ...item, reason: problem };
  return { ...item, enabled: true };
}

// openRequest is the call the menu item makes: api(path, body, undefined,
// headers). /client/ calls carry no connection by default (client-context.mjs),
// so the desktop's current connection is named here; the desktop refuses a
// view of an earlier connection.
export function openRequest(target, context) {
  const problem = targetProblem(target);
  if (problem) throw new Error(problem);
  let body;
  if (target.kind === "terminal") {
    body = { terminal_id: target.id };
    if (target.observe) body.observe = true;
  } else if (target.kind === "session") {
    body = { session_id: target.id };
  } else {
    body = { project: target.path, kind: target.run || "shell" };
    if (target.run === "claude" && accountID.test(target.claudeAccount || "")) body.claude_account_id = target.claudeAccount;
    if (target.run === "codex" && accountID.test(target.codexAccount || "")) body.codex_account_id = target.codexAccount;
  }
  return { path: OPEN_PATH, body, headers: { "X-Cage-Connection": context?.connection_id || "disconnected" } };
}

const shellWord = word => /^[A-Za-z0-9_./=:,@+%-]+$/.test(word) ? word : "'" + word.replaceAll("'", "'\\''") + "'";

// copyCommand is the "复制命令" action of the failure toast: the same window
// opened by hand in any terminal, on the desktop's current connection.
export function copyCommand(target) {
  const { body } = openRequest(target, {});
  // The environment operand goes last: cage attach stops reading flags at it.
  const args = ["cage", "attach"];
  if (body.terminal_id) args.push("--terminal", body.terminal_id);
  if (body.observe) args.push("--observe");
  if (body.session_id) args.push("--resume", body.session_id);
  if (body.project) args.push("--new", body.kind, "--project", body.project);
  if (body.claude_account_id) args.push("--claude-account", body.claude_account_id);
  if (body.codex_account_id) args.push("--codex-account", body.codex_account_id);
  args.push("current");
  return args.map(shellWord).join(" ");
}

export function launchFailure(name, reason) {
  const why = String(reason || "未知原因").trim().replace(/[。.]+$/, "");
  return "没能打开 " + name + "：" + why + "。";
}

// automationNotice is the dialog before the first launch of an application
// that macOS asks permission to control (design 6.3); null when none is due.
// app is what GET /client/local-terminal/app found ({id, name, automation}).
export function automationNotice(app, acknowledged = false) {
  if (!app?.automation || acknowledged) return null;
  return {
    title: "Super Cage 需要控制 " + app.name + " 来打开窗口",
    body: "macOS 接下来会询问是否允许 Super Cage 控制 " + app.name + "。Super Cage 只用它打开新窗口；如果不允许，窗口不会打开，之后可以在“系统设置 › 隐私与安全性 › 自动化”里允许。",
    cancel: "取消",
    confirm: "继续",
  };
}
