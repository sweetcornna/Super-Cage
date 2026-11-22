// 设备: the plain values and rules behind Settings → 设备 (devices-page.jsx and its two dialogs), so they can be
// tested without a page or a clock. The routes are the interface contract of the host (design §3.8):
//   GET /api/devices                         the list, the limit, keep-awake and the code on show
//   POST /api/devices/invite                 a code: image, manual (long), short (8 digits, when the host has them)
//   GET /api/devices/invite?wait=25          held read: waiting | request | joined | failed | expired
//   POST /api/devices/invite/approve|reject, DELETE /api/devices/invite, DELETE /api/devices/{id}
//   GET /api/client/devices, POST /api/client/devices/join, GET …/join?wait=25, POST …/{id}/open|close
import { MIN_SPACING_MS, backoffDelay } from "./session-poll.mjs";
import { DEVICE_KEY, DEVICE_NAME_KEY } from "./mobile-cli/device-link.mjs";
export { DEVICE_KEY, DEVICE_NAME_KEY };

// The id inside "device:<id>:<generation>", or "" for any other connection.
export const deviceIdOf = (connection) => /^device:[^:/\s]+:\d+$/.test(String(connection || "")) ? String(connection).split(":")[1] : "";

export const DEVICE_LIMIT = 5;
export const SHORT_LENGTH = 8;
export const HELD_SECONDS = 25;

const PLATFORMS = { darwin: "macOS", macos: "macOS", windows: "Windows", linux: "Linux", ios: "iOS", android: "Android" };
export const platformName = (value) => PLATFORMS[String(value || "").toLowerCase()] || "";

// "48219037" → "4821 9037", also while it is being typed.
export const groupDigits = (digits) => String(digits || "").replace(/\D/g, "").slice(0, SHORT_LENGTH).replace(/^(\d{4})(\d)/, "$1 $2");

// What the person typed or pasted into the joining field: the 8 digits (spaces, hyphens and dots between
// them are ignored) or a long code (CAGE-… or CAGE1:…, pasted whole, possibly broken over lines).
export function readCodeInput(raw) {
  const text = String(raw ?? "").trim();
  if (!text) return { kind: "empty", display: "", ready: false };
  // Also while it is being typed: "CA" is the start of a long code, not a mistake.
  if (/^c(?:a(?:g(?:e(?:[-1:].*)?)?)?)?$/is.test(text)) {
    const code = text.replace(/\s+/g, "");
    return { kind: "long", code, display: code, ready: /^cage[-1:]/i.test(code) && code.length > 8 };
  }
  if (/^[\d\s.\-–]+$/.test(text)) {
    const digits = text.replace(/\D/g, "").slice(0, SHORT_LENGTH);
    return { kind: "short", code: digits, display: groupDigits(digits), ready: digits.length === SHORT_LENGTH };
  }
  return { kind: "invalid", display: text, ready: false };
}

export const hostName = (relay) => {
  try { return new URL(relay).host; } catch { return String(relay || "").replace(/^https?:\/\//, "").replace(/\/.*$/, ""); }
};

// ── words ────────────────────────────────────────────────────────────────

const pad = (n) => String(n).padStart(2, "0");
const sameDay = (a, b) => a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
// An unset time arrives as the zero time of the host's language or nothing at all.
export const validTime = (value) => {
  const ms = Date.parse(value);
  return Number.isFinite(ms) && new Date(ms).getFullYear() >= 2000 ? ms : 0;
};

// 刚刚 · 12 分钟前 · 3 小时前 · 今天 14:02 · 昨天 14:02 · 11月20日 · 2025年11月20日
export function timeText(value, now = Date.now()) {
  const ms = validTime(value);
  if (!ms) return "";
  const date = new Date(ms), today = new Date(now), age = now - ms;
  if (age < 60000) return "刚刚";
  if (age < 3600000) return Math.floor(age / 60000) + " 分钟前";
  if (sameDay(date, today) && age < 6 * 3600000) return Math.floor(age / 3600000) + " 小时前";
  const clock = pad(date.getHours()) + ":" + pad(date.getMinutes());
  if (sameDay(date, today)) return "今天 " + clock;
  if (sameDay(date, new Date(now - 86400000))) return "昨天 " + clock;
  return (date.getFullYear() === today.getFullYear() ? "" : date.getFullYear() + "年") + (date.getMonth() + 1) + "月" + date.getDate() + "日";
}

// A phone without a model name is a host that predates the report (design §3.5).
export const deviceName = (device) => String(device?.name || "").trim() || (device?.kind === "computer" ? "未命名的电脑" : "型号待获取");

// 手机 · iOS
export const kindLine = (device) => [device?.kind === "computer" ? "电脑" : "手机", platformName(device?.platform)].filter(Boolean).join(" · ");

export function deviceMeta(device, now) {
  const seen = timeText(device?.last_seen, now);
  return kindLine(device) + " · " + (device?.connected ? "已连接" : seen ? "上次连接 " + seen : "还没有连接过");
}

export function computerMeta(computer, now) {
  const seen = timeText(computer?.last_open, now);
  return kindLine({ kind: "computer", platform: computer?.platform }) + " · " + (seen ? "上次打开 " + seen : "还没有打开过");
}

export const summaryText = (devices) => {
  const list = devices || [];
  return list.length ? list.length + " 台设备 · " + list.filter((device) => device.connected).length + " 台在线" : "";
};
export const limitOf = (state) => (Number.isInteger(state?.limit) && state.limit > 0 ? state.limit : DEVICE_LIMIT);
export const atLimit = (state) => (state?.devices?.length || 0) >= limitOf(state);
export const limitText = (limit) => "已达到 " + limit + " 台设备，请先移除一台。";

export const keepAwakeText = "有设备配对时，这台电脑保持运行，屏幕可以熄灭。移除全部设备后恢复原来的休眠设置。";

export function removeConfirmation(device) {
  const name = deviceName(device);
  return { title: "移除 " + name + "？", body: "移除 " + name + " 后，它要重新配对才能访问这台电脑。正在进行的操作会立即中断。", confirm: "移除", danger: true };
}
export function forgetConfirmation(computer) {
  const name = String(computer?.name || "").trim() || "这台电脑";
  return { title: "移除「" + name + "」？", body: "只会从这台电脑上忘记它。还需要在那台电脑的“设备”里移除这台电脑。", confirm: "移除", danger: true };
}

// "1:47 后失效"; null while there is no time to count to.
export function countdownText(expires, now = Date.now()) {
  const end = typeof expires === "number" ? expires : Date.parse(expires);
  if (!Number.isFinite(end) || !end) return "";
  const left = Math.max(0, Math.ceil((end - now) / 1000));
  return left ? Math.floor(left / 60) + ":" + pad(left % 60) + " 后失效" : "已过期";
}

export const customRelayLine = (relay) => relay ? "这台电脑使用自建中继 " + hostName(relay) + "，对方也要设置同一个中继。" : "";

// ── the add dialog (this computer shows a code) ──────────────────────────

export const FAILED_TEXT = "配对失败：配对码不对或连接被篡改。请重新生成。";

export const initialInvite = { phase: "minting" };

// phases: minting → code → request → joined, or failed | expired | rejected | limit | error at any step.
// Events: minted(reply, refreshed) · poll(result) · tick(now) · deciding · rejected · error(error) · restart.
export function inviteReducer(state, event) {
  switch (event.type) {
    case "restart": return initialInvite;
    case "minted": {
      const reply = event.reply || {};
      const expires = Date.parse(reply.expires);
      return { phase: "code", image: reply.image || "", manual: reply.manual || "", short: /^\d{8}$/.test(String(reply.short || "")) ? String(reply.short) : "", expires: Number.isFinite(expires) ? expires : 0, mintedAt: event.now ?? Date.now(), refreshed: Boolean(event.refreshed) };
    }
    case "poll": {
      if (!["code", "request"].includes(state.phase)) return state;
      const result = event.result || {};
      if (result.state === "request" && result.request) {
        // The same request again (a held read that timed out) keeps the buttons as they are.
        if (state.phase === "request" && state.request?.check === result.request.check) return state;
        return { ...state, phase: "request", request: { name: String(result.request.name || "").trim(), platform: result.request.platform || "", check: String(result.request.check || "") }, deciding: false };
      }
      if (result.state === "joined") return { phase: "joined", device: { id: result.device?.id || "", name: String(result.device?.name || "").trim() } };
      if (result.state === "failed") return { phase: "failed" };
      // "none": the host has no code any more (it was cancelled or replaced from another window).
      if (result.state === "expired" || result.state === "none") return { phase: "expired" };
      return state;
    }
    case "tick": return state.phase === "code" && state.expires && event.now >= state.expires ? { phase: "expired" } : state;
    case "deciding": return state.phase === "request" ? { ...state, deciding: true } : state;
    case "rejected": return { phase: "rejected", request: state.request };
    case "error": {
      // The host's own sentence for a full list ("最多添加 5 台设备。…") says how many; the page words it as it does at the limit.
      const full = event.error?.status === 409 ? /(\d+)\s*台设备/.exec(event.error?.message || "") : null;
      if (full) return { phase: "limit", limit: Number(full[1]) };
      return { phase: "error", message: event.error?.message || "请求没有完成，请稍后重试。" };
    }
    default: return state;
  }
}

// The words of the approval prompt (design §6.3): who asks, and the number to compare.
export const requestTitle = (request) => (request?.name || "一台设备") + (platformName(request?.platform) ? "（" + platformName(request.platform) + "）" : "") + "想加入这台电脑";
export const requestHintParts = (check) => ["核对数字 ", String(check), "，与那台电脑上显示的一致再允许。"];
export const requestHint = (check) => requestHintParts(check).join("");

// ── the join dialog (this computer enters a code) ────────────────────────

export const NOT_FOUND_TEXT = "没有找到这个配对码：请核对数字，并确认两台电脑使用同一个中继。";
export const USED_TEXT = "配对码已被使用，请在那台电脑上重新生成。";
export const DENIED_TEXT = "那台电脑没有允许加入。";
export const EXPIRED_TEXT = "配对码已过期，请在那台电脑上重新生成。";

// The sentence for a refused join. The gateway answers in Chinese when it has something specific to say (a 409 that
// names the relay the code uses and the one this computer uses; a locked or unreachable computer), and api.js
// composes a generic "(HTTP n)" sentence when it does not: the specific one is shown, the generic one is replaced by
// the sentence that fits the status.
export function joinFailure(error) {
  const message = String(error?.message || "");
  const specific = message && !/HTTP \d{3}/.test(message) ? message : "";
  if (error?.state === "denied") return DENIED_TEXT;
  if (error?.state === "expired") return EXPIRED_TEXT;
  if (error?.status === 404) return NOT_FOUND_TEXT;
  if (error?.status === 409 || error?.status === 410) return /中继/.test(specific) ? specific : USED_TEXT;
  return specific || "加入没有完成，请稍后重试。";
}

// The optional name of the computer being joined (the gateway keeps up to 40 characters and no control characters;
// the other computer does not tell its name, and without one the gateway says 「另一台电脑」).
export const NAME_LIMIT = 40;
export const readJoinName = (raw) => String(raw ?? "").replace(/[\u0000-\u001f\u007f-\u009f]/g, "").slice(0, NAME_LIMIT);

export const approvalText = (check) => "在那台电脑上点“允许”。核对数字 " + check;
// After the other computer allows, this one still has to agree: the same four digits, shown on both screens. Only 确认 saves
// the computer (so a code someone else typed, or a number that does not match, saves nothing).
export const confirmTitle = "那台电脑已允许加入";
export const confirmHintParts = (check) => ["核对数字 ", String(check), "，与那台电脑上显示的数字一致再确认。"];
export const confirmHint = (check) => confirmHintParts(check).join("");
// 确认 that finds nothing waiting any more (the other computer gave up, the time ran out, a lock): the join starts over.
export const CONFIRM_LOST_TEXT = "这次加入已失效，请在那台电脑上重新生成配对码。";
export const joinedText = (name) => "已加入「" + (String(name || "").trim() || "那台电脑") + "」";

// phases: input → searching → approval → confirm → saving → joined, or input again with an error.
export const initialJoin = { phase: "input", error: "" };
export function joinReducer(state, event) {
  switch (event.type) {
    case "edit": return state.phase === "input" ? { phase: "input", error: "" } : state;
    case "submit": return { phase: "searching" };
    case "reply": {
      const reply = event.reply || {};
      // The other computer has allowed: the number is shown again, and the computer is saved only when 确认 is pressed.
      if (reply.state === "confirm") return (state.phase === "saving" || state.phase === "confirm") && state.check === String(reply.check || "") && state.id === String(reply.id || "") ? state : { phase: "confirm", check: String(reply.check || ""), id: String(reply.id || "") };
      if (reply.state === "approval") {
        if (state.phase === "confirm" || state.phase === "saving") return state;
        return state.phase === "approval" && state.check === String(reply.check || "") ? state : { phase: "approval", check: String(reply.check || "") };
      }
      if (reply.state === "joined" || reply.id) return { phase: "joined", computer: { id: reply.id || "", name: String(reply.name || "").trim() } };
      if (reply.state === "failed" || reply.state === "denied" || reply.state === "expired") return { phase: "input", error: joinFailure({ state: reply.state, message: reply.error }) };
      return state;
    }
    case "confirming": return state.phase === "confirm" ? { ...state, phase: "saving" } : state;
    case "error": return { phase: "input", error: event.confirming && [404, 409, 410].includes(event.error?.status) ? CONFIRM_LOST_TEXT : joinFailure(event.error) };
    default: return state;
  }
}

// ── held reads ───────────────────────────────────────────────────────────

const abortableSleep = (ms, signal) => new Promise((resolve) => {
  const timer = setTimeout(resolve, ms);
  signal?.addEventListener("abort", () => { clearTimeout(timer); resolve(); }, { once: true });
});

// Reads until the signal ends or onResult returns true. A read is held by the host (wait seconds) and answers
// when something changes, so nothing ticks here: a read that came back sooner than MIN_SPACING_MS (a host that
// does not hold, a change) is followed by the rest of that second, a failed one by the session-poll backoff,
// and onError may end the loop (it returns true to stop).
export async function holdReads({ read, onResult, onError, signal, wait = HELD_SECONDS, now = Date.now, sleep = abortableSleep }) {
  let failures = 0;
  while (!signal.aborted) {
    const started = now();
    let result;
    try {
      result = await read(wait, signal);
      failures = 0;
    } catch (error) {
      if (signal.aborted) return;
      if (onError?.(error, ++failures)) return;
      await sleep(backoffDelay(failures), signal);
      continue;
    }
    if (signal.aborted || onResult(result)) return;
    const spent = now() - started;
    if (spent < MIN_SPACING_MS) await sleep(MIN_SPACING_MS - spent, signal);
  }
}

// ── opening a joined computer ────────────────────────────────────────────

// What a joined computer's window remembers for its page (the connection id and the name of the computer).
export function rememberDevice(connection, name, storage = globalThis.sessionStorage) {
  storage.setItem(DEVICE_KEY, connection);
  storage.setItem(DEVICE_NAME_KEY, name || "");
}
