// Pure state of the "本机终端" feature on this computer: the chooser of the
// terminal application, the menu item "在本机终端中打开" and the request that
// opens a terminal there. No DOM and no fetch, so node --test covers it.

export const BUILT_IN = "";
export const BUILT_IN_NAME = "Super Cage 内置终端";
export const MENU_LABEL = "在本机终端中打开";
export const MENU_SHORTCUT = "⌘⇧O";
export const MENU_ICON = "square-arrow-out-up-right";
export const SETTINGS_PATH = "/client/local-terminal";
export const APPS_PATH = "/client/local-terminal/apps";
export const OPEN_PATH = "/client/local-terminal/open";

// The names of the applications internal/localterm/apps.go knows, so the
// menu can name the chosen one without detecting anything. The test checks
// this list against the Go table.
export const APP_NAMES = Object.freeze({
  ghostty: "Ghostty",
  iterm2: "iTerm2",
  terminal: "终端",
  warp: "Warp",
  wezterm: "WezTerm",
  kitty: "kitty",
  alacritty: "Alacritty",
  herdr: "Herdr",
});

export const DEFAULT_SETTINGS = Object.freeze({ app: BUILT_IN, image_upload: true, clipboard_write: true });

export const SWITCHES = Object.freeze([
  {
    key: "image_upload",
    title: "上传拖入的图片",
    help: "把图片拖入本机终端，或粘贴的内容只有图片文件的完整路径时，会上传到环境，并粘贴环境中的路径。只处理 PNG、JPEG、WebP 和 GIF 图片；其他文件和输入的文字原样发送。",
  },
  {
    key: "clipboard_write",
    title: "允许环境写入剪贴板",
    help: "环境中的程序（例如 neovim、tmux）复制的内容会写入这台电脑的剪贴板，与网页终端一致。环境始终无法读取这台电脑的剪贴板。",
  },
]);

export function normalizeSettings(raw) {
  const value = raw && typeof raw === "object" ? raw : {};
  return {
    app: typeof value.app === "string" ? value.app : BUILT_IN,
    image_upload: typeof value.image_upload === "boolean" ? value.image_upload : true,
    clipboard_write: typeof value.clipboard_write === "boolean" ? value.clipboard_write : true,
  };
}

export function settingsChanged(saved, draft) {
  const a = normalizeSettings(saved);
  const b = normalizeSettings(draft);
  return a.app !== b.app || a.image_upload !== b.image_upload || a.clipboard_write !== b.clipboard_write;
}

export function appName(id, apps) {
  if (id === BUILT_IN) return BUILT_IN_NAME;
  return (Array.isArray(apps) && apps.find(app => app.id === id)?.name) || APP_NAMES[id] || id;
}

const collator = new Intl.Collator("en", { sensitivity: "base", numeric: true });
const byName = (a, b) => collator.compare(a.name, b.name);

// chooserOptions orders the chooser as design 5.8 and 6.1 ask: applications
// found on this computer (alphabetical), then the built-in terminal, then the
// others greyed out. An installed application that cannot show Cage
// terminals yet (Herdr) sits with the greyed ones and says why.
export function chooserOptions(apps, selected = BUILT_IN) {
  const list = Array.isArray(apps) ? apps : [];
  const ready = list.filter(app => app.installed && app.hosts).sort(byName);
  const waiting = list.filter(app => app.installed && !app.hosts).sort(byName);
  const missing = list.filter(app => !app.installed).sort(byName);
  const option = (app, group) => ({
    id: app.id,
    name: app.name,
    group,
    selectable: group === "ready",
    note: group === "ready"
      ? (app.automation ? "第一次打开时，macOS 会询问是否允许 Super Cage 控制 " + app.name : "")
      : group === "waiting" ? (app.note || "稍后的版本提供") : "",
    chip: group === "missing" ? "未安装" : group === "waiting" ? "稍后提供" : "",
    checked: app.id === selected,
  });
  return [
    ...ready.map(app => option(app, "ready")),
    { id: BUILT_IN, name: BUILT_IN_NAME, group: "built-in", selectable: true, note: "在 Super Cage 的标签页中打开终端，菜单里不显示“" + MENU_LABEL + "”。", chip: "", checked: selected === BUILT_IN },
    ...waiting.map(app => option(app, "waiting")),
    ...missing.map(app => option(app, "missing")),
  ];
}

// missingChoice names the saved application when it is no longer installed,
// so the page can say so instead of silently showing another choice.
export function missingChoice(settings, apps) {
  const chosen = normalizeSettings(settings).app;
  if (chosen === BUILT_IN || !Array.isArray(apps)) return "";
  const app = apps.find(item => item.id === chosen);
  return app && app.installed && app.hosts ? "" : appName(chosen, apps);
}

const terminalID = /^[0-9a-f]{16}$/;
const sessionID = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;
const accountID = /^[A-Za-z0-9_-]{1,128}$/;
const kinds = new Set(["shell", "claude", "codex"]);
// Any control character (C0, DEL, C1) or a lone surrogate, as the desktop
// and cage attach refuse: the path reaches a command line and the clipboard.
const controls = /\p{Cc}|[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/u;

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
// session row. It is hidden while the built-in terminal is chosen and outside
// the desktop app. It never detects applications: the desktop refuses one
// that is gone, and the failure toast says so.
export function menuItem({ settings, apps, target, context, environment } = {}) {
  const chosen = normalizeSettings(settings).app;
  if (chosen === BUILT_IN || !context?.desktop_gateway) return { visible: false };
  const item = {
    visible: true,
    label: MENU_LABEL,
    detail: appName(chosen, apps),
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
  const args = ["cage", "attach", "current"];
  if (body.terminal_id) args.push("--terminal", body.terminal_id);
  if (body.observe) args.push("--observe");
  if (body.session_id) args.push("--resume", body.session_id);
  if (body.project) args.push("--new", body.kind, "--project", body.project);
  if (body.claude_account_id) args.push("--claude-account", body.claude_account_id);
  if (body.codex_account_id) args.push("--codex-account", body.codex_account_id);
  return args.map(shellWord).join(" ");
}

export function launchFailure(name, reason) {
  const why = String(reason || "未知原因").trim().replace(/[。.]+$/, "");
  return "没能打开 " + name + "：" + why + "。可在设置 → 本机终端应用里换一个。";
}

// automationNotice is the dialog before the first launch of an application
// that macOS asks permission to control (design 6.3); null when none is due.
export function automationNotice(app, acknowledged = false) {
  if (!app?.automation || acknowledged) return null;
  return {
    title: "Super Cage 需要控制 " + app.name + " 来打开窗口",
    body: "macOS 接下来会询问是否允许 Super Cage 控制 " + app.name + "。Super Cage 只用它打开新窗口；如果不允许，会改用不需要权限的方式打开。",
    cancel: "改用其他应用",
    confirm: "继续",
  };
}
