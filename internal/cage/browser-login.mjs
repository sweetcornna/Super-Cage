import { createHash } from "node:crypto";

// Official HTTPS OAuth/login hosts for Cage's built-in mainstream Agent CLIs.
export const BUILTIN_LOGIN_HOSTS = new Set([
  "claude.ai", "claude.com", "console.anthropic.com", "platform.claude.com", "platform.anthropic.com",
  "auth.openai.com", "chatgpt.com", "platform.openai.com",
  "accounts.google.com", "oauth2.googleapis.com", "antigravity.google", "aistudio.google.com",
  "github.com", "auth.githubcopilot.com",
  "opencode.ai", "openrouter.ai",
  "chat.qwen.ai", "qwen.ai", "account.aliyun.com", "dashscope.console.aliyun.com",
  "ampcode.com", "sourcegraph.com",
  "app.cline.bot", "cline.bot", "auth.cline.bot",
  "cursor.com", "authenticator.cursor.sh",
  "console.mistral.ai", "mistral.ai",
  "code.kimi.com", "kimi.com", "platform.moonshot.cn", "platform.moonshot.ai",
  "app.factory.ai", "auth.factory.ai", "factory.ai",
  "pi.dev",
]);

const VALID_HOST = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$/;
const IPV4_HOST = /^\d+(?:\.\d+){3}$/;

export function isAllowedLoginHost(hostname, extraHosts) {
  const host = String(hostname || "").toLowerCase().trim();
  if (!host || host.length > 253) return false;
  if (BUILTIN_LOGIN_HOSTS.has(host)) return true;
  if (!Array.isArray(extraHosts)) return false;
  for (const item of extraHosts) {
    if (typeof item !== "string") continue;
    const candidate = item.toLowerCase().trim();
    if (
      candidate === host &&
      candidate.length <= 253 &&
      VALID_HOST.test(candidate) &&
      !IPV4_HOST.test(candidate) &&
      candidate !== "localhost" &&
      !candidate.endsWith(".local") &&
      !candidate.endsWith(".internal")
    ) {
      return true;
    }
  }
  return false;
}

// The browser owns these ephemeral cookies. Only the origin of a login page
// may enter the workbench tab list, including for popups and redirects.
export class LoginBrowser {
  constructor(cdp) {
    this.cdp = cdp;
    this.contexts = new Map();
  }

  tab(raw, previous) {
    if (!previous?.login && !this.contexts.has(raw.browserContextId)) return raw;
    let origin = "about:blank";
    try {
      const url = new URL(raw.url);
      if (["https:", "http:"].includes(url.protocol)) origin = url.origin;
    } catch {}
    return { ...raw, login: true, url: origin, title: "账号授权" };
  }

  async open(login, address, activate, extraHosts, activeId) {
    if (typeof login !== "string" || !/^[a-f0-9-]{16,64}$/.test(login)) throw new Error("登录任务无效");
    if (typeof address !== "string" || address !== address.trim() || /[\x00-\x20\x7f\\]/.test(address) || !/^https:\/\//i.test(address))
      throw new Error("仅可打开官方登录地址");
    const rawAuthority = address.slice("https://".length).split(/[/?#]/, 1)[0];
    if (!rawAuthority || rawAuthority.includes(":") || rawAuthority.includes("@"))
      throw new Error("仅可打开官方登录地址");
    const url = new URL(address);
    if (url.protocol !== "https:" || url.port || url.username || url.password || !isAllowedLoginHost(url.hostname, extraHosts))
      throw new Error("仅可打开官方登录地址");
    // The CLI opener and the terminal link can deliver the same request.
    // Keep its browser state without retaining the address or its OAuth parameters.
    const requestKey = createHash("sha256").update(url.href).digest("hex");
    for (const [id, entry] of this.contexts) {
      if (entry.login !== login || entry.requestKey !== requestKey) continue;
      const { targetInfos } = await this.cdp("Target.getTargets");
      const pages = targetInfos.filter(t => t.type === "page" && t.browserContextId === id);
      const page = pages.find(t => t.targetId === activeId) ?? pages.find(t => t.targetId === entry.targetId) ?? pages[0];
      if (page) {
        await activate(page);
        return { id: page.targetId };
      }
    }
    await this.finish(login);
    const { browserContextId } = await this.cdp("Target.createBrowserContext", { disposeOnDetach: true });
    const expiry = setTimeout(() => this.close(browserContextId).catch(() => {}), 15 * 60_000);
    expiry.unref();
    const entry = { login, requestKey, expiry, targetId: "" };
    this.contexts.set(browserContextId, entry);
    try {
      const { targetId } = await this.cdp("Target.createTarget", { browserContextId, url: "about:blank", newWindow: true });
      entry.targetId = targetId;
      const sessionId = await activate({ targetId, browserContextId, type: "page", url: url.href });
      const navigation = await this.cdp("Page.navigate", { url: url.href }, sessionId);
      // A replacement navigation can cancel the initial load while the tab is still in use.
      if (navigation?.errorText && navigation.errorText !== "net::ERR_ABORTED") throw new Error("授权页导航失败");
      return { id: targetId };
    } catch {
      await this.close(browserContextId);
      throw new Error("临时登录页面未能打开");
    }
  }

  async finish(login) {
    for (const [id, entry] of this.contexts) {
      if (entry.login === login) await this.close(id);
    }
  }

  async close(id) {
    const entry = this.contexts.get(id);
    if (!entry) return;
    await this.cdp("Target.disposeBrowserContext", { browserContextId: id });
    clearTimeout(entry.expiry);
    this.contexts.delete(id);
  }
}
