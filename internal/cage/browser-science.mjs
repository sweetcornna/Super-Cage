// Claude Science (phase 1): the daemon's own page opens as an ordinary tab of
// this browser. Sign-in uses its single-use link from `claude-science url`,
// accepted only for the loopback port that guest provisioning wrote, never one
// a caller names. The link and the sign-in callback stay out of tab state.
import { readFileSync, statSync } from "node:fs";

export const scienceConfigFile = "/etc/claude-cage/science.json";
export const scienceTitle = "Claude Science";

let cached = { file: "", stamp: "", config: null };

// Read on use, not at start: the agent runs on guests without Claude Science
// and outlives its installation. Absent or invalid means "not installed".
export function readScienceConfig(file = scienceConfigFile) {
  let stamp;
  try {
    const info = statSync(file);
    stamp = `${info.mtimeMs}:${info.size}`;
  } catch {
    cached = { file, stamp: "", config: null };
    return null;
  }
  if (cached.file === file && cached.stamp === stamp) return cached.config;
  let config = null;
  try {
    const { port } = JSON.parse(readFileSync(file, "utf8"));
    if (Number.isInteger(port) && port >= 1024 && port <= 65535) config = { port };
  } catch {}
  cached = { file, stamp, config };
  return config;
}

// The exact text the daemon prints, checked before any URL parsing so that no
// normalization (case, default ports, dot segments, IDNA) can widen it. The
// host is localhost, or 127.0.0.1 when another program holds [::1] there.
const linkPattern = /^http:\/\/(?:localhost|127\.0\.0\.1):([1-9][0-9]{3,4})\/\?nonce=([A-Za-z0-9_-]{16,128})$/;

export function scienceLink(value, port) {
  if (!Number.isInteger(port)) throw new Error("Claude Science 尚未安装或配置");
  const match = typeof value === "string" ? linkPattern.exec(value) : null;
  if (!match || Number(match[1]) !== port) throw new Error("Claude Science 登录链接无效");
  const url = new URL(value);
  if (url.href !== value || url.username || url.password || url.hash || [...url.searchParams.keys()].join() !== "nonce")
    throw new Error("Claude Science 登录链接无效");
  return url.href;
}

// Classification only: the daemon redirects 127.0.0.1 to localhost itself.
export function isScienceURL(value, port) {
  if (!Number.isInteger(port) || typeof value !== "string") return false;
  try {
    const url = new URL(value);
    return url.protocol === "http:" && ["localhost", "127.0.0.1"].includes(url.hostname) &&
      url.port === String(port) && !url.username && !url.password;
  } catch {
    return false;
  }
}

// Sign-in links and OAuth callbacks carry single-use secrets.
const privateParameters = ["nonce", "code", "state"];

export function publicScienceURL(value, port) {
  if (!isScienceURL(value, port)) return value;
  const url = new URL(value);
  if (!privateParameters.some(name => url.searchParams.has(name))) return value;
  for (const name of privateParameters) url.searchParams.delete(name);
  return url.href;
}

// Hosts of the documented sign-in hop; the tab passes through them.
const signInHosts = ["claude.ai", "claude.com", "console.anthropic.com", "platform.claude.com", "platform.anthropic.com"];

// How long after a link was handed over its tab counts as signing in.
export const scienceSignInWindow = 15 * 60 * 1000;

// Whether the tab given the last link can simply be brought forward: it shows
// the app, or is in the middle of signing in (signingIn: within the window).
// A tab left on a link or on the link's form needs a fresh link (single-use,
// ~3 minutes); a tab the user took elsewhere, including claude.ai after the
// window, is left alone.
export function scienceTabReusable(value, port, signingIn = false) {
  if (isScienceURL(value, port)) {
    const url = new URL(value);
    return !url.searchParams.has("nonce") && url.pathname !== "/api/auth/nonce";
  }
  try {
    const url = new URL(value);
    return signingIn === true && url.protocol === "https:" && !url.username && !url.password && signInHosts.includes(url.hostname);
  } catch {
    return false;
  }
}
