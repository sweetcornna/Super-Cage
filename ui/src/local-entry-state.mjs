// Pure state for the "本机终端" page of one environment (design 6.2). The
// page reads GET /api/entry; everything shown is derived here so it can be
// tested without a browser.

const MINUTE = 60_000;

// The one status line under the title: a dot tone and plain Chinese text.
// Never a code or an English message.
export function entryStateLine(view, { locked = false } = {}) {
  if (locked) return { tone: "warn", text: "Super Cage 已锁定 · 解锁后可连接" };
  if (!view) return { tone: "muted", text: "正在读取本机终端的状态…" };
  if (view.revoked && view.enabled) return { tone: "warn", text: "这台电脑的钥匙已撤销 · 重新开启即可生成新钥匙" };
  switch (view.state) {
    case "unsupported":
      return { tone: "muted", text: "这个环境的系统暂不支持本机终端" };
    case "off":
      return { tone: "muted", text: "未开启" };
    case "stopped":
      return { tone: "muted", text: "环境已停止 · 启动环境后可连接" };
    case "starting":
      return { tone: "busy", text: "环境正在启动 · 启动完成后可连接" };
    case "paused":
      return { tone: "warn", text: "网络已暂停 · ssh 已断开，恢复网络后可重新连接" };
    case "in_use":
      return { tone: "ok", text: "已开启 · " + connectionSummary(view) };
    case "idle":
      return { tone: "ok", text: "已开启 · 没有连接" };
    default:
      return { tone: "muted", text: "状态未知" };
  }
}

// "2 个连接", with a split once Herdr lines exist next to ssh ones.
export function connectionSummary(view) {
  const connections = view?.connections || {};
  const total = connections.total ?? 0;
  const herdr = connections.herdr ?? 0;
  if (!total) return "没有连接";
  if (view?.herdr && herdr) return `${total} 个连接（ssh ${total - herdr}，Herdr ${herdr}）`;
  return `${total} 个连接`;
}

// Herdr's row: whether this computer's Herdr added the environment (the
// server knows from the machine link) and is connected now.
const HERDR_STATES = { not_set_up: "未添加", ready: "已添加", connected: "已连接" };

// The command rows. "ssh cage-x" only works once ~/.ssh/config includes
// Super Cage's file; until then the first row is the cage ssh form, which
// needs no Include. The attach row shows when the server offers cage attach
// for this environment (commands.attach); Herdr's when it sends the
// command that adds the environment to Herdr (view.herdr).
export function commandRows(view) {
  const commands = view?.commands;
  if (!view?.enabled || !commands) return [];
  const included = Boolean(view.include?.present);
  const rows = [
    included
      ? { id: "ssh", command: commands.ssh, caption: "任何终端：直接登录" }
      : { id: "ssh", command: commands.cage_ssh, caption: "任何终端：直接登录（不改动 ~/.ssh/config）" },
  ];
  if (commands.attach) {
    rows.push({ id: "attach", command: commands.attach, caption: "任何终端：新开一个这个环境的终端，也会出现在 Super Cage 里" });
  }
  const herdr = view.herdr;
  if (herdr?.command) {
    rows.push({
      id: "herdr",
      command: herdr.command,
      status: HERDR_STATES[herdr.state] || "",
      connected: herdr.state === "connected",
      caption: "Herdr：运行一次，把环境加为一台机器。它会在环境里装上 herdr-core，空闲 10 分钟后自动停止",
    });
  }
  return rows;
}

// The ssh config row while the entry is on: whether ssh cage-x works from
// the owner's own config, and the one action that changes it.
export function includeRow(view) {
  const include = view?.include;
  if (!view?.enabled || !include) return null;
  if (include.error) {
    // Read errors arrive with or without their prefix; a read-only file or
    // a host name the owner already uses arrive as whole sentences.
    const read = /^无法读取 ~\/\.ssh\/config：/.test(include.error) || !include.error.startsWith("~/.ssh/config");
    const text = read ? "无法读取 ~/.ssh/config：" + include.error.replace(/^无法读取 ~\/\.ssh\/config：/, "") : include.error;
    return { state: "error", text: text + "。你仍可以用 " + (view.commands?.cage_ssh || "cage ssh") + " 连接。" };
  }
  if (include.present) return { state: "present", text: "~/.ssh/config 已包含 Super Cage 的配置，可以直接 ssh " + (view.host_alias || "") };
  return { state: "absent", text: "~/.ssh/config 还没有包含 Super Cage 的配置，现在用 cage ssh 连接。" };
}

// The dialog that turns the entry on for the first time on this computer:
// the line, the host name it brings in and the whole block behind it.
export function includeDialog(preview) {
  return {
    title: "让 ssh 认识这个环境",
    intro: "Super Cage 会在 " + (preview?.display_path || "~/.ssh/config") + " 的最上面加一行：",
    line: preview?.line || "Include ~/.ssh/super-cage/config",
    alias: preview?.alias || "",
    aliasText: preview?.alias ? "之后用 ssh " + preview.alias + " 连接。这一行引入的配置：" : "",
    block: preview?.block || "",
    warning: preview?.warning || "",
    after: "这一行只引用 Super Cage 自己的文件，原文件会先备份。关闭所有环境的本机终端时会移除它。",
    actions: { cancel: "取消", secondary: "不写入，只用 cage ssh", confirm: "写入并开启" },
  };
}

// What the page says about filtering (design 5.7), exactly as cage entry's
// help does (entryTerminalNote in entry_cli.go).
export function unfilteredNote() {
  return "带终端的会话（ssh、ssh -t）是一个 Super Cage 终端：环境的输出经过和 cage attach 一样的过滤，剪贴板写入按这台电脑的“允许环境写入剪贴板”；拖入的图片不上传，按文字粘贴。" +
    "不带终端的命令（ssh 主机 命令、ssh -T）、scp、sftp 和 rsync 原样传送环境的字节，不经过滤：环境里的程序可以借这些输出向你的终端发送控制字符。带终端的子系统（ssh -t -s）一律不接受。" +
    "Herdr 的终端画面同样经过过滤；环境不能经 Herdr 向这台电脑的终端输入文字或读取它们。";
}

// What the page says when a revocation did not reach a running environment
// in time: its open connections go on until it next hears of the key.
export function unreachedNote(names = []) {
  if (!names.length) return "";
  return "另一个环境暂时没有响应，已撤销的钥匙会在它下次连接时被拒绝（" + names.join("、") + "）";
}

// The kept ssh terminals with 结束 (they have their own limit).
export function keptRows(view) {
  const kept = view?.kept || [];
  return {
    title: "保留的终端",
    help: `来自本机终端、连接断开后仍保留的终端，最多 ${view?.kept_limit || 4} 个。没人查看也没有程序运行 30 分钟后会自动结束。`,
    rows: kept.map((terminal) => ({
      id: terminal.id,
      title: terminal.title,
      detail: "来自 " + (terminal.device || "本机终端") + (terminal.ended ? " · 已结束" : ""),
    })),
  };
}

export function endKeptConfirm(row) {
  return {
    title: `结束「${row.title}」？`,
    body: "终端里还在运行的程序会一起结束。",
    confirm: "结束终端",
    danger: true,
  };
}

// "最近使用 3 分钟前".
export function relativeTime(iso, now = Date.now()) {
  if (!iso) return "从未使用";
  const then = Date.parse(iso);
  if (!Number.isFinite(then)) return "从未使用";
  const minutes = Math.floor((now - then) / MINUTE);
  if (minutes < 1) return "最近使用 刚刚";
  if (minutes < 60) return `最近使用 ${minutes} 分钟前`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `最近使用 ${hours} 小时前`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `最近使用 ${days} 天前`;
  const date = new Date(then);
  return `最近使用 ${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(date.getDate()).padStart(2, "0")}`;
}

// "SHA256:AbCdEf…wxyz": enough to compare with ssh-keygen -lf.
export function shortFingerprint(fingerprint) {
  if (!fingerprint) return "";
  const [kind, value = ""] = fingerprint.includes(":") ? fingerprint.split(/:(.*)/s) : ["", fingerprint];
  if (value.length <= 12) return fingerprint;
  return (kind ? kind + ":" : "") + value.slice(0, 6) + "…" + value.slice(-4);
}

// Mirrors validEntryDeviceName on the server.
export function validDeviceName(name) {
  const length = [...name].length;
  return length >= 1 && length <= 64 && name.trim() === name && !/\p{Cc}/u.test(name);
}

// Devices: this computer first, then by last use; revoked ones last.
export function sortedDevices(devices = []) {
  return [...devices].sort((a, b) =>
    Number(Boolean(a.revoked)) - Number(Boolean(b.revoked)) ||
    Number(Boolean(b.current)) - Number(Boolean(a.current)) ||
    (Date.parse(b.last_used || b.created || 0) || 0) - (Date.parse(a.last_used || a.created || 0) || 0));
}

export function revokeConfirm(device) {
  return {
    title: `撤销「${device.name}」？`,
    body: "这台电脑的所有连接会立即断开，之后需要重新开启才能连接。",
    confirm: "撤销",
    danger: true,
  };
}

export const offConfirm = {
  title: "关闭本机终端？",
  body: "所有本机连接会立即断开；环境里的终端继续运行。",
  confirm: "关闭本机终端",
  danger: true,
};

// What the page shows for a failed GET /api/entry.
export function loadError(error) {
  const status = error?.status;
  if (status === 423) return { locked: true, text: "" };
  if (status === 404) return { text: "需要更新 Super Cage：正在运行的版本还没有本机终端。" };
  if (status === 403) return { text: "本机终端只能在这台电脑上的 Super Cage 里管理。" };
  return { text: error?.message || "无法读取本机终端的状态。" };
}
