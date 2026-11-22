// Pure helpers for the Codex account panel, terminal menu and tab labels.

const NATIVE_GUESTS = new Set(["macos", "windows-11", "windows-remote"]);

// Codex accounts use a private CODEX_HOME in the Linux guest only.
export function codexTerminalSupported(system) {
  return !NATIVE_GUESTS.has(system || "");
}

// Codex surfaces follow presence (/status "clients" and "clients_unknown",
// design 2.3, choice C): shown when a codex program was found, and as before
// while nothing is known. "pending" is the window before its first /status:
// it shows nothing and asks for nothing yet.
export function codexPresence(status) {
  if (!status) return "pending";
  if (status.clients_unknown === true) return "unknown";
  return Array.isArray(status.clients) && status.clients.includes("codex") ? "present" : "absent";
}

// menu: the 新建 row; link: 账号管理 in the sidebar; accounts: the
// /codex-accounts read; providers: what the session list asks for. A macOS
// or Windows guest is never read: unknown, so as before (and no 新建 row).
export function codexSurfaces(status, system) {
  const presence = status && !codexTerminalSupported(system) ? "unknown" : codexPresence(status);
  const shown = presence === "present" || presence === "unknown";
  return { presence, menu: shown && codexTerminalSupported(system), link: shown, accounts: shown,
    providers: presence === "absent" ? "claude" : "claude,codex" };
}

// Codex reports each window's length; label it like the Claude panel's 5h/7d.
export function codexWindowLabel(minutes, slot = "primary") {
  if (!Number.isInteger(minutes) || minutes <= 0) return slot === "secondary" ? "长周期" : "短周期";
  if (minutes % 1440 === 0) return `${minutes / 1440}d`;
  if (minutes % 60 === 0) return `${minutes / 60}h`;
  return `${minutes}m`;
}

// The shared quota components read five_hour/seven_day snapshots. Keep the
// Codex window lengths alongside for their labels.
export function codexQuotaView(profile) {
  const quota = profile?.quota;
  return { ...profile, quota: quota ? { five_hour: quota.primary, seven_day: quota.secondary, observed_at: quota.observed_at } : undefined };
}

export function codexAccountName(account) {
  return account?.label || account?.email || "";
}

const isCodexTab = (tab) => tab?.terminalKind === "codex" || tab?.terminalKind === "codex-login";

// Label shown on a Codex terminal's tab and header. Default terminals use the
// environment's own ~/.codex login.
export function codexTerminalLabel(tab, accounts) {
  if (!isCodexTab(tab)) return "";
  const account = accounts?.find((item) => item.id === tab.codex_account_id);
  if (tab.terminalKind === "codex-login") return codexAccountName(account) || "新账号登录";
  if (!tab.codex_account_id) return "默认登录";
  return codexAccountName(account) || "Codex 账号";
}

export function codexTerminalDetail(tab, accounts) {
  if (!isCodexTab(tab)) return "";
  const label = codexTerminalLabel(tab, accounts);
  if (tab.terminalKind === "codex-login") return `Codex 登录 · ${label}`;
  return tab.codex_account_id ? `Codex · ${label}` : "Codex · 环境默认登录（~/.codex）";
}

// Count live Codex terminals per account for the panel's notes.
export function codexActiveCounts(tabs) {
  const counts = new Map();
  for (const tab of tabs || []) {
    if (!tab.codex_account_id || tab.closed) continue;
    counts.set(tab.codex_account_id, (counts.get(tab.codex_account_id) || 0) + 1);
  }
  return counts;
}
