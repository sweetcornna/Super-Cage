// Match printed links in xterm's wrapped buffer. OSC 8 links also use this
// exact destination check before opening the guest's private login tab.
export const claudeLoginLink = /https:\/\/(?:claude\.ai|claude\.com|console\.anthropic\.com|platform\.claude\.com|platform\.anthropic\.com)\/(?:cai\/)?oauth\/authorize\/?\?[^\s"'<>\\\x1b]+/i;

export function isClaudeLoginLink(uri) {
  const match = claudeLoginLink.exec(uri);
  return uri.length <= 8192 && match?.[0] === uri;
}

// Codex device login prints a fixed page; the one-time code is entered there.
// Opening it in the user's own browser carries no token or callback.
export const codexDeviceLink = /https:\/\/auth\.openai\.com\/codex\/device(?![\w/?#.-])/;

export function isCodexDeviceLink(uri) {
  return uri === "https://auth.openai.com/codex/device";
}

export const BUILTIN_AGENT_LOGIN_HOSTS = [
  "auth.openai.com",
  "chatgpt.com",
  "platform.openai.com",
  "accounts.google.com",
  "oauth2.googleapis.com",
  "aistudio.google.com",
  "opencode.ai",
  "openrouter.ai",
  "github.com",
  "auth.githubcopilot.com",
  "chat.qwen.ai",
  "qwen.ai",
  "account.aliyun.com",
  "dashscope.console.aliyun.com",
  "ampcode.com",
  "sourcegraph.com",
  "app.cline.bot",
  "cline.bot",
  "auth.cline.bot",
  "cursor.com",
  "authenticator.cursor.sh",
  "console.mistral.ai",
  "auth.mistral.ai",
  "mistral.ai",
  "code.kimi.com",
  "kimi.com",
  "platform.moonshot.ai",
  "platform.moonshot.cn",
  "app.factory.ai",
  "auth.factory.ai",
  "factory.ai",
  "pi.dev",
];

const CLAUDE_HOSTS = new Set([
  "claude.ai",
  "claude.com",
  "console.anthropic.com",
  "platform.claude.com",
  "platform.anthropic.com",
]);

const LOGIN_PATH_TOKENS = [
  "oauth",
  "auth",
  "login",
  "device",
  "signin",
  "sign-in",
  "authorize",
  "verify",
  "cli",
];

function normalizedHosts(extraHosts = []) {
  const set = new Set(BUILTIN_AGENT_LOGIN_HOSTS);
  for (const host of extraHosts || []) {
    const clean = String(host || "").trim().toLowerCase();
    if (clean && /^[a-z0-9.-]+$/.test(clean) && !clean.includes("..")) {
      set.add(clean);
    }
  }
  return [...set];
}

function escapeRegex(value) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function hasStrictHTTPSAuthority(uri) {
  if (typeof uri !== "string" || uri.length === 0 || uri.length > 8192) return false;
  if (uri !== uri.trim() || !/^https:\/\//i.test(uri)) return false;
  for (let i = 0; i < uri.length; i += 1) {
    const code = uri.charCodeAt(i);
    if (code <= 0x20 || code === 0x5c || code === 0x7f) return false;
  }
  const authority = uri.slice("https://".length).split(/[/?#]/, 1)[0];
  return Boolean(authority) && !authority.includes(":") && !authority.includes("@");
}

export function isAgentLoginLink(uri, extraHosts = []) {
  if (!hasStrictHTTPSAuthority(uri)) return false;
  if (isClaudeLoginLink(uri)) return true;
  let parsed;
  try {
    parsed = new URL(uri);
  } catch {
    return false;
  }
  if (parsed.protocol !== "https:" || parsed.username || parsed.password || parsed.port) return false;
  const host = parsed.hostname.toLowerCase();
  if (CLAUDE_HOSTS.has(host)) return false;
  const allowed = new Set(normalizedHosts(extraHosts));
  if (!allowed.has(host)) return false;
  const lowerPath = parsed.pathname.toLowerCase().replace(/\/+$/, "");
  return LOGIN_PATH_TOKENS.some((token) => lowerPath.includes(token));
}

export function agentLoginLinkRegex(extraHosts = []) {
  const hosts = [...CLAUDE_HOSTS, ...normalizedHosts(extraHosts)].map(escapeRegex);
  return new RegExp(`https:\\/\\/(?:${hosts.join("|")})\\/[^\\s"'<>\\\\\\x1b]+`, "i");
}
