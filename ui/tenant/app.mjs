// Cage 多租户门户（无构建依赖，契约见 docs/api-contract.md）。
// 浏览器中由 index.html 以模块加载，并在存在 #app 时自动启动；
// 在 Node 中导入时只导出纯逻辑与控制器，供 node --test 使用。

export const VM_ID_PATTERN = /^[0-9a-f]{32}$/;
export const USERNAME_PATTERN = /^[a-z0-9][a-z0-9._-]{2,31}$/;
export const TICKET_PATTERN = /^[A-Za-z0-9._~+/=-]{1,2048}$/;
export const SYSTEM_ID_PATTERN = /^[a-z0-9-]{1,32}$/;
export const VM_STATES = Object.freeze(["stopped", "starting", "ready", "error"]);
export const POLL_INTERVAL_MS = 2000;
export const WARM_POLL_MS = 350;
export const SESSION_CHECK_GAP_MS = 1000;
export const EXPECTED_USER_HEADER = "X-Cage-Expected-User";
export const SESSION_CHANNEL = "cage-tenant-session";
export const SESSION_PING_KEY = "cage-tenant.session-ping.v1";
export const POLL_MAX_FAILURES = 5;
export const POLL_MAX_BACKOFF_MS = 15000;
export const FRAME_SLOW_MS = 30000;
export const IFRAME_SANDBOX = "allow-scripts allow-same-origin allow-forms allow-downloads";
export const IFRAME_REFERRER_POLICY = "no-referrer";

/** 只把剪贴板读写委托给该 VM 的精确源，不扩大 sandbox。 */
export function iframeAllow(origin) {
  return `clipboard-read ${origin}; clipboard-write ${origin}`;
}
export const ENTER_PATH = "/_tenant/enter";
export const SELECTION_KEY = "cage-tenant.selected-vm.v1";
export const THEME_KEY = "cage-tenant.theme.v1";
export const THEMES = Object.freeze(["system", "light", "dark"]);
const SELECTION_MAX_USERS = 8;
const MESSAGE_MAX = 500;

/* ------------------------------------------------------------------ */
/* 校验与规范化                                                        */
/* ------------------------------------------------------------------ */

export function isValidVmId(value) {
  return typeof value === "string" && VM_ID_PATTERN.test(value);
}

export function normalizeUsername(raw) {
  return String(raw ?? "").trim().toLowerCase();
}

export function validateUsername(raw) {
  const value = normalizeUsername(raw);
  if (!value) return { ok: false, message: "请输入用户名。" };
  if (value.length < 3 || value.length > 32) {
    return { ok: false, message: "用户名需为 3–32 个字符。" };
  }
  if (!USERNAME_PATTERN.test(value)) {
    return {
      ok: false,
      message: "用户名只能使用英文字母、数字和 . _ -，并以字母或数字开头。",
    };
  }
  return { ok: true, value };
}

export function utf8Length(value) {
  return new TextEncoder().encode(value).length;
}

// 密码不改写空白。登录时只检查非空与字节上限，避免规则调整后挡住已有账户。
export function validatePassword(raw, { forLogin = false } = {}) {
  const value = typeof raw === "string" ? raw : "";
  if (!value) return { ok: false, message: "请输入密码。" };
  if (utf8Length(value) > 128) {
    return { ok: false, message: "密码不能超过 128 字节（UTF-8）。" };
  }
  if (!forLogin && Array.from(value).length < 12) {
    return { ok: false, message: "密码至少需要 12 个字符。" };
  }
  return { ok: true, value };
}

export function validateVmName(raw) {
  const value = String(raw ?? "").trim();
  if (!value) return { ok: false, message: "请输入虚拟机名称。" };
  if (/\p{Cc}/u.test(value)) return { ok: false, message: "名称不能包含控制字符。" };
  if (Array.from(value).length > 64) return { ok: false, message: "名称最多 64 个字符。" };
  return { ok: true, value };
}

function cleanText(value, max = MESSAGE_MAX) {
  if (typeof value !== "string") return "";
  const text = value.replace(/\p{Cc}+/gu, " ").trim();
  return Array.from(text).slice(0, max).join("");
}

export function normalizeUser(raw) {
  if (!raw || typeof raw !== "object") return null;
  const id = typeof raw.id === "string" ? raw.id : "";
  const username = cleanText(raw.username, 64);
  // 账户 ID 会作为 X-Cage-Expected-User 请求头发送，只接受可见 ASCII。
  if (!/^[\x21-\x7e]{1,200}$/.test(id) || !username) return null;
  return { id, username };
}

export function isValidSystemId(value) {
  return typeof value === "string" && SYSTEM_ID_PATTERN.test(value);
}

export function normalizeSystem(raw) {
  if (!raw || typeof raw !== "object" || !isValidSystemId(raw.id)) return null;
  return { id: raw.id, label: cleanText(raw.label, 64) || raw.id };
}

export function normalizeSystemList(raw) {
  const seen = new Set();
  const systems = [];
  for (const item of Array.isArray(raw) ? raw : []) {
    const system = normalizeSystem(item);
    if (!system || seen.has(system.id)) continue;
    seen.add(system.id);
    systems.push(system);
  }
  return systems;
}

export function normalizeVm(raw) {
  if (!raw || typeof raw !== "object" || !isValidVmId(raw.id)) return null;
  const state = VM_STATES.includes(raw.state) ? raw.state : "unknown";
  const systemId = isValidSystemId(raw.system_id) ? raw.system_id : "";
  const vm = {
    id: raw.id,
    name: cleanText(raw.name, 64),
    system_id: systemId,
    system_label: cleanText(raw.system_label, 64) || systemId,
    state,
    created_at: typeof raw.created_at === "string" ? raw.created_at : "",
  };
  if (state === "error") {
    const failure = describeVmFailure(raw.message);
    if (failure) vm.message = failure.message;
    if (failure?.detail) vm.message_detail = failure.detail;
  }
  return vm;
}

export function normalizeVmList(raw) {
  const seen = new Set();
  const vms = [];
  for (const item of Array.isArray(raw) ? raw : []) {
    const vm = normalizeVm(item);
    if (!vm || seen.has(vm.id)) continue;
    seen.add(vm.id);
    vms.push(vm);
  }
  return vms;
}

function normalizeLimit(raw) {
  return Number.isInteger(raw) && raw >= 0 ? raw : null;
}

function isLoopbackDevHost(hostname) {
  return hostname === "localhost" || hostname.endsWith(".localhost");
}

/**
 * 校验 open 接口返回的 workspace_url 与约定完全一致：
 * 同门户 scheme 和 port、主机严格为 vm-<id>.<门户主机>、路径为 /_tenant/enter、
 * 无查询与凭据、票据只出现在 fragment，且字符串已是规范形式。
 * HTTP 只允许 *.localhost 的显式本地开发。
 */
export function validateWorkspaceUrl(raw, vmId, portal) {
  const refuse = (message) => ({ ok: false, message });
  const generic = "服务器返回的工作台地址未通过校验，已拒绝加载。";
  if (typeof raw !== "string" || !raw || raw.length > 4096) return refuse(generic);
  if (!isValidVmId(vmId)) return refuse(generic);
  const protocol = portal?.protocol;
  const hostname = typeof portal?.hostname === "string" ? portal.hostname : "";
  const port = typeof portal?.port === "string" ? portal.port : "";
  if (!hostname) return refuse(generic);
  if (protocol !== "https:" && !(protocol === "http:" && isLoopbackDevHost(hostname))) {
    return refuse("当前门户地址不支持打开工作台，请使用 HTTPS 门户。");
  }
  let url;
  try {
    url = new URL(raw);
  } catch {
    return refuse(generic);
  }
  if (url.protocol !== protocol) return refuse(generic);
  if (url.username || url.password) return refuse(generic);
  if (url.hostname !== `vm-${vmId}.${hostname}`) return refuse(generic);
  if (url.port !== port) return refuse(generic);
  if (url.pathname !== ENTER_PATH) return refuse(generic);
  if (url.search !== "") return refuse(generic);
  if (!TICKET_PATTERN.test(url.hash.slice(1))) return refuse(generic);
  if (url.href !== raw) return refuse(generic);
  return { ok: true, url: url.href, origin: url.origin };
}

/* ------------------------------------------------------------------ */
/* 门户 API 客户端                                                     */
/* ------------------------------------------------------------------ */

const FALLBACK_MESSAGES = Object.freeze({
  unauthenticated: "登录已失效，请重新登录。",
  session_changed: "登录账户已在其他标签页变化。",
  registration_closed: "此门户未开放自助注册，请联系管理员获取账户。",
  invalid_credentials: "用户名或密码不正确。",
  username_taken: "该用户名已被使用。",
  vm_not_found: "虚拟机不存在或无权访问。",
  not_found: "请求的内容不存在。",
  forbidden: "没有权限执行此操作。",
  invalid_input: "输入内容无效，请检查后重试。",
  invalid_content_type: "请求格式不受支持。",
  vm_limit: "已达到虚拟机数量上限。",
  origin_refused: "请求来源被拒绝，请从门户地址重新打开页面。",
  rate_limited: "操作过于频繁，请稍后再试。",
  runtime_unavailable: "虚拟机运行时暂时不可用，请稍后重试。",
  network_error: "无法连接门户服务，请检查网络后重试。",
  bad_response: "门户服务返回了无法识别的响应。",
  server_error: "门户服务暂时出错，请稍后重试。",
  aborted: "请求已取消。",
});

function codeForStatus(status) {
  if (status === 401) return "unauthenticated";
  if (status === 403) return "forbidden";
  if (status === 404) return "not_found";
  if (status === 415) return "invalid_content_type";
  if (status === 429) return "rate_limited";
  if (status === 503) return "runtime_unavailable";
  if (status >= 500) return "server_error";
  return "bad_response";
}

export function fallbackMessage(code, status = 0) {
  // Own keys only: a code such as "constructor" must not pick up an Object.prototype member.
  return (
    (Object.hasOwn(FALLBACK_MESSAGES, code) && FALLBACK_MESSAGES[code]) ||
    (status ? `请求失败（HTTP ${status}），请稍后重试。` : "请求失败，请稍后重试。")
  );
}

// The gateway answers in English and the portal speaks Chinese. Text that is already Chinese (a localized
// server, or the mock) is shown as sent. English text is replaced by the sentence for its error code; with
// an unknown code the user gets a generic sentence and the server's own words stay as secondary detail.
const HAN = /\p{Script=Han}/u;

export function describeProblem(code, message, status = 0) {
  const text = cleanText(message);
  if (text && HAN.test(text)) return { message: text };
  if (Object.hasOwn(FALLBACK_MESSAGES, code)) return { message: FALLBACK_MESSAGES[code] };
  const generic = fallbackMessage(code, status);
  return text ? { message: generic, detail: text } : { message: generic };
}

// A failed VM's message carries no code; these are the gateway's fixed sentences for it.
const VM_FAILURE_MESSAGES = Object.freeze({
  "The workspace is unavailable. Open it again to retry.": "虚拟机暂时无法启动，可以重新打开再试。",
});

export function describeVmFailure(message) {
  const text = cleanText(message);
  if (!text) return null;
  if (HAN.test(text)) return { message: text };
  if (Object.hasOwn(VM_FAILURE_MESSAGES, text)) return { message: VM_FAILURE_MESSAGES[text] };
  return { message: "虚拟机启动失败，可以重试。", detail: text };
}

export class ApiError extends Error {
  constructor({ status = 0, code = "bad_response", message = "", aborted = false } = {}) {
    super(cleanText(message) || fallbackMessage(code, status));
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.aborted = aborted;
  }
}

function transportError(error, signal) {
  if (signal?.aborted || error?.name === "AbortError") {
    return new ApiError({ code: "aborted", aborted: true });
  }
  return new ApiError({ code: "network_error" });
}

/**
 * 只使用同源会话 Cookie。注意保持 fetch 默认的 mode（"cors"）：
 * 若改成 "same-origin"，在 no-referrer 策略下浏览器会把 POST 的 Origin 置为 null。
 */
export function createApi({ fetch: fetchImpl = globalThis.fetch?.bind(globalThis), base = "" } = {}) {
  async function call(method, path, { body, signal, expectedUser } = {}) {
    const headers = { Accept: "application/json" };
    if (typeof expectedUser === "string" && expectedUser) headers[EXPECTED_USER_HEADER] = expectedUser;
    const init = {
      method,
      headers,
      credentials: "same-origin",
      cache: "no-store",
      redirect: "error",
      signal,
    };
    if (method !== "GET") {
      headers["Content-Type"] = "application/json";
      init.body = JSON.stringify(body ?? {});
    }
    let response;
    try {
      response = await fetchImpl(base + path, init);
    } catch (error) {
      throw transportError(error, signal);
    }
    let text = "";
    try {
      text = response.status === 204 ? "" : await response.text();
    } catch (error) {
      throw transportError(error, signal);
    }
    let data = null;
    if (text) {
      try {
        data = JSON.parse(text);
      } catch {
        data = null;
      }
    }
    if (!response.ok) {
      const detail =
        data && typeof data === "object" && data.error && typeof data.error === "object"
          ? data.error
          : {};
      const code =
        typeof detail.code === "string" && /^[a-z0-9_]{1,64}$/.test(detail.code)
          ? detail.code
          : codeForStatus(response.status);
      throw new ApiError({ status: response.status, code, message: detail.message });
    }
    if (response.status !== 204 && (!data || typeof data !== "object" || Array.isArray(data))) {
      throw new ApiError({ status: response.status, code: "bad_response" });
    }
    return { status: response.status, data };
  }
  return {
    session: (options) => call("GET", "/api/tenant/session", options),
    login: (username, password, options) =>
      call("POST", "/api/tenant/login", { ...options, body: { username, password } }),
    register: (username, password, options) =>
      call("POST", "/api/tenant/register", { ...options, body: { username, password } }),
    logout: (options) => call("POST", "/api/tenant/logout", { ...options, body: {} }),
    listVms: (options) => call("GET", "/api/tenant/vms", options),
    createVm: (name, options) =>
      call("POST", "/api/tenant/vms", { ...options, body: { name } }),
    openVm: (id, options) =>
      isValidVmId(id)
        ? call("POST", `/api/tenant/vms/${id}/open`, { ...options, body: {} })
        : Promise.reject(new ApiError({ status: 404, code: "vm_not_found" })),
  };
}

/* ------------------------------------------------------------------ */
/* 本地记忆：只保存“账户 ID → 所选 VM ID”，不保存票据、URL、用户名或密码   */
/* ------------------------------------------------------------------ */

export function safeStorage(name) {
  try {
    const storage = globalThis[name];
    return storage && typeof storage.getItem === "function" ? storage : null;
  } catch {
    return null;
  }
}

export function createSelectionStore({
  local = safeStorage("localStorage"),
  session = safeStorage("sessionStorage"),
} = {}) {
  const stores = [session, local].filter(Boolean);
  const validUser = (userId) =>
    typeof userId === "string" && userId.length > 0 && userId.length <= 200;
  function readPairs(storage) {
    try {
      const parsed = JSON.parse(storage.getItem(SELECTION_KEY) || "[]");
      if (!Array.isArray(parsed)) return [];
      return parsed.filter(
        (pair) =>
          Array.isArray(pair) && pair.length === 2 && validUser(pair[0]) && isValidVmId(pair[1]),
      );
    } catch {
      return [];
    }
  }
  function writePairs(storage, pairs) {
    try {
      if (pairs.length) storage.setItem(SELECTION_KEY, JSON.stringify(pairs));
      else storage.removeItem(SELECTION_KEY);
    } catch {
      /* 存储不可用时只是不记忆所选项 */
    }
  }
  return {
    read(userId) {
      if (!validUser(userId)) return null;
      for (const storage of stores) {
        const pair = readPairs(storage).find(([user]) => user === userId);
        if (pair) return pair[1];
      }
      return null;
    },
    write(userId, vmId) {
      if (!validUser(userId) || !isValidVmId(vmId)) return;
      for (const storage of stores) {
        const pairs = readPairs(storage).filter(([user]) => user !== userId);
        pairs.push([userId, vmId]);
        writePairs(storage, pairs.slice(-SELECTION_MAX_USERS));
      }
    },
    forget(userId, vmId = null) {
      if (!validUser(userId)) return;
      for (const storage of stores) {
        const pairs = readPairs(storage);
        const next = pairs.filter(([user, vm]) => user !== userId || (vmId && vm !== vmId));
        if (next.length !== pairs.length) writePairs(storage, next);
      }
    },
  };
}

/* ------------------------------------------------------------------ */
/* 门户控制器（状态机）                                                  */
/* ------------------------------------------------------------------ */

const DEFAULT_AUTH = Object.freeze({ mode: "login", busy: false, error: null, notice: null });
const CLOSED_CREATE = Object.freeze({ open: false, seq: 0, busy: false, error: null });

function errorView(error) {
  return {
    code: error?.code || "bad_response",
    status: error?.status || 0,
    ...describeProblem(error?.code, error?.message, error?.status),
  };
}

function signedOutState(auth = DEFAULT_AUTH) {
  return {
    phase: "signed-out",
    bootError: null,
    user: null,
    auth: { ...DEFAULT_AUTH, ...auth },
    vms: [],
    limit: null,
    listStatus: "idle",
    listError: null,
    selectedId: null,
    target: null,
    create: CLOSED_CREATE,
    logout: { busy: false },
    banner: null,
  };
}

function switchedBanner(previous, user) {
  if (!previous) return { kind: "info", message: `已在其他标签页登录为 ${user.username}。`, action: null };
  if (previous.id === user.id) return { kind: "info", message: "登录会话已刷新，可以重新打开虚拟机。", action: null };
  return {
    kind: "info",
    message: `登录账户已在其他标签页切换为 ${user.username}，原账户的工作台已关闭。`,
    action: null,
  };
}

/**
 * 依赖全部可注入：api、存储、计时器、当前时间、门户 location、页面可见性、跨标签通知。
 * target 表示当前要打开的 VM，status 为
 *   opening（open 请求中）→ starting（202 后轮询）→ connecting（iframe 载入中）→ ready
 *   以及 error（可手动重试）。
 * 每次切换都会递增 targetGen、中止旧请求并清除计时器，旧结果一律丢弃。
 * 已知用户时，目录/列表/创建/打开/登出请求都带 X-Cage-Expected-User；
 * 409 session_changed 时立即卸载旧工作台、清空旧账户数据并重新读取会话，不重放原操作。
 */
export function createPortal({
  api,
  selection = createSelectionStore({ local: null, session: null }),
  timers = { setTimeout: (fn, ms) => setTimeout(fn, ms), clearTimeout: (id) => clearTimeout(id) },
  now = () => Date.now(),
  location: portalLocation = globalThis.location,
  isHidden = () => false,
  broadcast = null,
  pollIntervalMs = POLL_INTERVAL_MS,
  warmPollMs = WARM_POLL_MS,
  frameSlowMs = FRAME_SLOW_MS,
  checkGapMs = SESSION_CHECK_GAP_MS,
} = {}) {
  if (!api) throw new Error("createPortal 需要 api");
  let registrationClosed = false;
  const signedOut = (auth) => ({ ...signedOutState(auth), registrationClosed });
  let state = { ...signedOut(), phase: "booting" };
  const listeners = new Set();
  let sessionGen = 0;
  let targetGen = 0;
  let listSeq = 0;
  let appliedListSeq = 0;
  let frameSeq = 0;
  let checkSeq = 0;
  let lastCheckAt = -Infinity;
  let membershipEpoch = 0;
  const localAdds = new Map(); // 本页新建的 VM ID → 新建时的 membershipEpoch
  let targetAbort = null;
  let listAbort = null;
  let authAbort = null;
  let checkAbort = null;
  let createSeq = 0;
  let pollTimer = null;
  let frameTimer = null;
  let pollDeferred = false;
  let openInFlight = false;
  let pendingRestore = false;

  function set(patch) {
    state = { ...state, ...patch };
    for (const listener of [...listeners]) listener(state);
  }

  function setTarget(patch) {
    if (state.target) set({ target: { ...state.target, ...patch } });
  }

  const findVm = (id) => state.vms.find((vm) => vm.id === id) || null;

  // 请求发出时绑定当前显示的账户；服务端发现 Cookie 已属于其他账户时返回 409 session_changed。
  const expected = (options = {}) => ({ ...options, expectedUser: state.user?.id });

  function notifyOtherTabs() {
    try {
      broadcast?.post();
    } catch {
      /* 通知只是加速同步，失败时依赖可见性检查 */
    }
  }

  function clearPollTimer() {
    if (pollTimer !== null) timers.clearTimeout(pollTimer);
    pollTimer = null;
    pollDeferred = false;
  }

  function clearFrameTimer() {
    if (frameTimer !== null) timers.clearTimeout(frameTimer);
    frameTimer = null;
  }

  function cancelTarget() {
    targetGen += 1;
    targetAbort?.abort();
    targetAbort = null;
    openInFlight = false;
    clearPollTimer();
    clearFrameTimer();
  }

  function cancelSession() {
    sessionGen += 1;
    cancelTarget();
    listAbort?.abort();
    listAbort = null;
    authAbort?.abort();
    authAbort = null;
    checkAbort?.abort();
    checkAbort = null;
    pendingRestore = false;
  }

  function expire(notice = "登录已失效，请重新登录。") {
    cancelSession();
    set(signedOut({ notice }));
  }

  /** 统一处理会话类错误；返回 true 表示已处理。 */
  function handleSessionError(error) {
    if (error?.code === "unauthenticated") {
      expire();
      return true;
    }
    if (error?.code === "session_changed") {
      sessionChanged();
      return true;
    }
    return false;
  }

  /* ---- 会话 ---- */

  async function loadSession(previous, announce) {
    const gen = sessionGen;
    const controller = new AbortController();
    authAbort = controller;
    try {
      const { data } = await api.session({ signal: controller.signal });
      if (gen !== sessionGen) return;
      const user = normalizeUser(data?.user);
      if (!user) throw new ApiError({ status: 200, code: "bad_response" });
      signedIn(user, { banner: announce ? switchedBanner(previous, user) : null });
    } catch (error) {
      if (gen !== sessionGen || error?.aborted) return;
      if (error?.code === "unauthenticated") {
        set(signedOut({ notice: announce && previous ? "登录状态已在其他标签页变化，请重新登录。" : null }));
      } else {
        set({ ...signedOut(), phase: "session-error", bootError: errorView(error) });
      }
    }
  }

  async function boot() {
    cancelSession();
    set({ ...signedOut(), phase: "booting" });
    await loadSession(null, false);
  }

  function sessionChanged() {
    const previous = state.user;
    cancelSession();
    // 立即卸载旧工作台并清空旧账户数据，再重新读取会话；不把原操作重放到新账户。
    set({ ...signedOut(), phase: "booting" });
    void loadSession(previous, true);
  }

  /**
   * 重新读取会话（跨标签通知、页面重新可见或获得焦点时）。
   * 新的检查会取代进行中的检查；期间若状态已变化，结果视为过期并忽略。
   * force=false 时在短间隔内合并重复触发。
   */
  async function checkSession({ force = true } = {}) {
    if (state.phase === "booting") return;
    if (state.phase === "session-error") return void boot();
    if (state.phase === "signed-out" && state.auth.busy) return;
    if (state.phase === "signed-in" && state.logout.busy) return;
    if (!force && (checkAbort || now() - lastCheckAt < checkGapMs)) return;
    checkAbort?.abort();
    const controller = new AbortController();
    checkAbort = controller;
    lastCheckAt = now();
    const gen = sessionGen;
    const seq = ++checkSeq;
    const shown = state.user;
    try {
      const { data } = await api.session({ signal: controller.signal });
      if (checkAbort === controller) checkAbort = null;
      if (gen !== sessionGen || seq !== checkSeq) return;
      const user = normalizeUser(data?.user);
      if (!user || (shown && user.id === shown.id)) return;
      signedIn(user, { banner: switchedBanner(shown, user) });
    } catch (error) {
      if (checkAbort === controller) checkAbort = null;
      if (gen !== sessionGen || seq !== checkSeq || error?.aborted) return;
      if (error?.code === "unauthenticated" && shown) {
        expire("已在其他标签页退出登录，或登录已过期。请重新登录。");
      }
    }
  }

  function signedIn(user, { banner = null } = {}) {
    cancelSession();
    localAdds.clear();
    set({ ...signedOut(), phase: "signed-in", user, listStatus: "loading", banner });
    pendingRestore = true;
    void refreshList();
  }

  function setAuth(patch) {
    set({ auth: { ...state.auth, ...patch } });
  }

  function setAuthMode(mode) {
    if (state.phase !== "signed-out" || state.auth.busy) return;
    if (mode !== "login" && mode !== "register") return;
    set({ auth: { ...DEFAULT_AUTH, mode } });
  }

  async function authenticate(kind, { username, password, confirm } = {}) {
    if (state.phase !== "signed-out" || state.auth.busy) return;
    const register = kind === "register";
    if (register && registrationClosed) return; // 已知注册关闭，不再尝试
    let name;
    if (register) {
      const checked = validateUsername(username);
      if (!checked.ok) return setAuth({ error: { field: "username", message: checked.message } });
      name = checked.value;
    } else {
      name = normalizeUsername(username);
      if (!name) return setAuth({ error: { field: "username", message: "请输入用户名。" } });
    }
    const checkedPassword = validatePassword(password, { forLogin: !register });
    if (!checkedPassword.ok) {
      return setAuth({ error: { field: "password", message: checkedPassword.message } });
    }
    if (register && confirm !== password) {
      return setAuth({ error: { field: "confirm", message: "两次输入的密码不一致。" } });
    }
    const gen = sessionGen;
    authAbort?.abort();
    const controller = new AbortController();
    authAbort = controller;
    setAuth({ busy: true, error: null, notice: null });
    try {
      const request = register ? api.register : api.login;
      const { data } = await request(name, checkedPassword.value, { signal: controller.signal });
      if (gen !== sessionGen) return;
      const user = normalizeUser(data?.user);
      if (!user) throw new ApiError({ status: 200, code: "bad_response" });
      signedIn(user);
      notifyOtherTabs();
    } catch (error) {
      if (gen !== sessionGen || error?.aborted) return;
      if (register && error?.code === "registration_closed") {
        registrationClosed = true;
        set({ registrationClosed: true, auth: { ...state.auth, busy: false, error: null } });
        return;
      }
      const field = error?.code === "username_taken" ? "username" : null;
      setAuth({ busy: false, error: { field, message: errorView(error).message } });
    }
  }

  async function logout() {
    if (state.phase !== "signed-in" || state.logout.busy) return;
    const resumeId = state.selectedId;
    // 先卸载 iframe、停止轮询，再请求服务端撤销会话。
    cancelTarget();
    listAbort?.abort();
    listAbort = null;
    pendingRestore = false;
    const options = expected();
    set({ target: null, selectedId: null, logout: { busy: true }, banner: null });
    const gen = sessionGen;
    try {
      await api.logout(options);
    } catch (error) {
      if (gen !== sessionGen) return;
      if (error?.code === "session_changed") return sessionChanged();
      if (error?.code !== "unauthenticated") {
        set({
          logout: { busy: false },
          banner: { kind: "error", message: `退出未完成：${errorView(error).message}`, action: "logout" },
        });
        // 仍处于登录状态：继续加载列表，并恢复原先打开的虚拟机。
        if (state.listStatus !== "ready") {
          pendingRestore = true;
          void refreshList();
        } else if (resumeId && findVm(resumeId)) {
          select(resumeId);
        }
        return;
      }
    }
    if (gen !== sessionGen) return;
    cancelSession();
    set(signedOut({ notice: "已安全退出。" }));
    notifyOtherTabs();
  }

  /* ---- 列表 ---- */

  function applyList(data, seq, epoch) {
    if (seq < appliedListSeq) return false;
    appliedListSeq = seq;
    const fresh = normalizeVmList(data?.vms);
    const ids = new Set(fresh.map((vm) => vm.id));
    // 列表请求发出后本页才新建的 VM 不会出现在这份较旧的列表里，保留它们，避免旧列表覆盖新建结果。
    const kept = state.vms.filter((vm) => !ids.has(vm.id) && (localAdds.get(vm.id) ?? -1) > epoch);
    set({ vms: [...fresh, ...kept], limit: normalizeLimit(data?.limit) ?? state.limit });
    if (state.selectedId && !findVm(state.selectedId)) vmGone(state.selectedId);
    return true;
  }

  function vmGone(id, message = "该虚拟机不存在或已无权访问。") {
    const known = findVm(id);
    if (state.target?.vmId === id || state.selectedId === id) {
      cancelTarget();
      set({ target: null, selectedId: null });
    }
    if (known) set({ vms: state.vms.filter((vm) => vm.id !== id) });
    localAdds.delete(id);
    if (state.user) selection.forget(state.user.id, id);
    set({ banner: { kind: "error", message, action: null } });
  }

  async function refreshList() {
    if (state.phase !== "signed-in" || state.logout.busy) return;
    const gen = sessionGen;
    const seq = ++listSeq;
    const epoch = membershipEpoch;
    listAbort?.abort();
    const controller = new AbortController();
    listAbort = controller;
    if (state.listStatus !== "ready") set({ listStatus: "loading", listError: null });
    try {
      const { data } = await api.listVms(expected({ signal: controller.signal }));
      if (gen !== sessionGen) return;
      if (listAbort === controller) listAbort = null;
      applyList(data, seq, epoch);
      set({ listStatus: "ready", listError: null });
      if (pendingRestore) {
        pendingRestore = false;
        restoreSelection();
      }
    } catch (error) {
      if (gen !== sessionGen || error?.aborted) return;
      if (listAbort === controller) listAbort = null;
      if (handleSessionError(error)) return;
      set({ listStatus: "error", listError: errorView(error) });
    }
  }

  function restoreSelection() {
    if (!state.user) return;
    const remembered = selection.read(state.user.id);
    if (!remembered) return;
    if (findVm(remembered)) select(remembered);
    else selection.forget(state.user.id, remembered);
  }

  function upsertVm(vm) {
    const index = state.vms.findIndex((item) => item.id === vm.id);
    const vms =
      index < 0 ? [...state.vms, vm] : state.vms.map((item, i) => (i === index ? vm : item));
    set({ vms });
  }

  /* ---- 打开与切换 ---- */

  function select(id, { force = false } = {}) {
    if (state.phase !== "signed-in" || state.logout.busy || !findVm(id)) return;
    const current = state.target;
    if (current && current.vmId === id && !force && current.status !== "error") return;
    // 同一目标正在提交 open 时不重复提交。
    if (current && current.vmId === id && openInFlight) return;
    const warm = findVm(id).state === "ready"; // 此前已在运行：202 时更早确认一次
    cancelTarget();
    const gen = targetGen;
    selection.write(state.user.id, id);
    set({
      selectedId: id,
      target: {
        vmId: id,
        status: "opening",
        since: now(),
        warm,
        quickChecked: false,
        error: null,
        pollFailures: 0,
        frame: null,
      },
    });
    void open(gen);
  }

  async function open(gen) {
    if (gen !== targetGen || !state.target || openInFlight) return;
    const id = state.target.vmId;
    const controller = new AbortController();
    targetAbort = controller;
    openInFlight = true;
    let result;
    try {
      result = await api.openVm(id, expected({ signal: controller.signal }));
    } catch (error) {
      if (gen !== targetGen || error?.aborted) return;
      openInFlight = false;
      targetAbort = null;
      return targetFailed(gen, error);
    }
    if (gen !== targetGen) return;
    openInFlight = false;
    targetAbort = null;
    const { status, data } = result;
    const vm = normalizeVm(data?.vm);
    if (!vm || vm.id !== id) {
      return fail(gen, { code: "bad_response", message: "服务器返回的虚拟机信息与请求不一致。" });
    }
    upsertVm(vm);
    if (status === 200) {
      const checked = validateWorkspaceUrl(data.workspace_url, id, portalLocation);
      if (!checked.ok) return fail(gen, { code: "invalid_workspace_url", message: checked.message });
      return mountFrame(gen, checked.url);
    }
    if (status === 202) {
      const quick = state.target.warm && !state.target.quickChecked;
      setTarget({ status: "starting", quickChecked: true });
      return schedulePoll(gen, quick ? warmPollMs : pollIntervalMs);
    }
    return fail(gen, { code: "bad_response", message: `服务器返回了意外的状态（HTTP ${status}）。` });
  }

  function schedulePoll(gen, delay) {
    if (gen !== targetGen) return;
    clearPollTimer();
    pollTimer = timers.setTimeout(() => {
      pollTimer = null;
      if (gen !== targetGen) return;
      if (isHidden()) {
        pollDeferred = true; // 页面隐藏时暂停，恢复可见后补一次
        return;
      }
      void poll(gen);
    }, delay);
  }

  async function poll(gen) {
    if (gen !== targetGen || !state.target || state.target.status !== "starting") return;
    const id = state.target.vmId;
    const seq = ++listSeq;
    const epoch = membershipEpoch;
    const controller = new AbortController();
    targetAbort = controller;
    let data;
    try {
      ({ data } = await api.listVms(expected({ signal: controller.signal })));
    } catch (error) {
      if (gen !== targetGen || error?.aborted) return;
      targetAbort = null;
      if (handleSessionError(error)) return;
      const failures = (state.target.pollFailures || 0) + 1;
      if (failures >= POLL_MAX_FAILURES) return fail(gen, errorView(error));
      setTarget({ pollFailures: failures });
      return schedulePoll(gen, Math.min(pollIntervalMs * 2 ** failures, POLL_MAX_BACKOFF_MS));
    }
    if (gen !== targetGen) return;
    targetAbort = null;
    applyList(data, seq, epoch);
    if (gen !== targetGen) return; // 目标已从列表消失
    const vm = findVm(id);
    if (!vm) return;
    if (state.target.pollFailures) setTarget({ pollFailures: 0 });
    if (vm.state === "ready") return open(gen);
    if (vm.state === "error") {
      return fail(gen, { code: "vm_error", message: vm.message || "虚拟机启动失败。" });
    }
    if (vm.state === "stopped") {
      return fail(gen, { code: "vm_stopped", message: "虚拟机未在启动，可能已被管理员停止。可以重试启动。" });
    }
    if (state.target.warm) setTarget({ warm: false }); // 确认后仍在启动：按开机处理
    return schedulePoll(gen, pollIntervalMs);
  }

  function visibilityChanged() {
    if (isHidden() || !pollDeferred) return;
    pollDeferred = false;
    void poll(targetGen);
  }

  function targetFailed(gen, error) {
    if (handleSessionError(error)) return;
    if (error?.code === "vm_not_found") {
      return vmGone(state.target?.vmId, errorView(error).message);
    }
    return fail(gen, errorView(error));
  }

  function fail(gen, error) {
    if (gen !== targetGen || !state.target) return;
    clearPollTimer();
    clearFrameTimer();
    setTarget({ status: "error", error, frame: null });
  }

  function mountFrame(gen, href) {
    if (gen !== targetGen) return;
    clearFrameTimer();
    const key = ++frameSeq;
    setTarget({
      status: "connecting",
      error: null,
      frame: { key, src: href, loaded: false, slow: false },
    });
    startFrameTimer(gen, key);
  }

  function startFrameTimer(gen, key) {
    clearFrameTimer();
    frameTimer = timers.setTimeout(() => {
      frameTimer = null;
      const frame = state.target?.frame;
      if (gen !== targetGen || !frame || frame.key !== key || frame.loaded) return;
      setTarget({ frame: { ...frame, slow: true } });
    }, frameSlowMs);
  }

  function frameLoaded(key) {
    const frame = state.target?.frame;
    if (!frame || frame.key !== key || frame.loaded) return;
    clearFrameTimer();
    // 载入后不再在内存中保留带票据的地址。
    setTarget({ status: "ready", frame: { key, src: null, loaded: true, slow: false } });
  }

  function frameFailed(key, message = "工作台页面载入失败。") {
    const frame = state.target?.frame;
    if (!frame || frame.key !== key) return;
    fail(targetGen, { code: "frame_error", message });
  }

  function keepWaiting() {
    const frame = state.target?.frame;
    if (!frame || frame.loaded || !frame.slow) return;
    setTarget({ frame: { ...frame, slow: false } });
    startFrameTimer(targetGen, frame.key);
  }

  function retry() {
    const target = state.target;
    if (!target) return;
    if (target.status === "error" || target.frame?.slow) select(target.vmId, { force: true });
  }

  function reconnect() {
    const target = state.target;
    if (!target || openInFlight) return;
    select(target.vmId, { force: true });
  }

  function goHome() {
    if (state.phase !== "signed-in") return;
    cancelTarget();
    selection.forget(state.user.id);
    set({ target: null, selectedId: null });
  }

  /* ---- 新建 ---- */

  function setCreate(patch) {
    set({ create: { ...state.create, ...patch } });
  }

  function openCreate() {
    if (state.phase !== "signed-in" || state.logout.busy || state.create.open) return;
    const seq = ++createSeq;
    set({ create: { ...CLOSED_CREATE, open: true, seq } });
  }

  function closeCreate() {
    if (!state.create.open || state.create.busy) return;
    set({ create: CLOSED_CREATE });
  }

  const atLimit = () => state.limit !== null && state.vms.length >= state.limit;

  async function createVm(name) {
    if (state.phase !== "signed-in" || !state.create.open || state.create.busy) return;
    const seq = state.create.seq;
    const checked = validateVmName(name);
    if (!checked.ok) {
      return setCreate({ error: { field: "name", code: "invalid_input", message: checked.message } });
    }
    if (atLimit()) {
      return setCreate({
        error: { field: null, code: "vm_limit", message: `已达到每个账户 ${state.limit} 台虚拟机的上限。` },
      });
    }
    const gen = sessionGen;
    setCreate({ busy: true, error: null });
    try {
      const { data } = await api.createVm(checked.value, expected());
      if (gen !== sessionGen) return;
      const vm = normalizeVm(data?.vm);
      if (!vm) throw new ApiError({ status: 201, code: "bad_response" });
      localAdds.set(vm.id, ++membershipEpoch);
      upsertVm(vm);
      if (state.create.seq === seq) set({ create: CLOSED_CREATE });
      select(vm.id);
    } catch (error) {
      if (gen !== sessionGen || error?.aborted) return;
      if (handleSessionError(error)) return;
      if (state.create.seq !== seq) return;
      setCreate({ busy: false, error: { field: null, ...errorView(error) } });
      if (error?.code === "vm_limit") void refreshList();
    }
  }

  function dismissBanner() {
    if (state.banner) set({ banner: null });
  }

  return {
    getState: () => state,
    subscribe(listener) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    boot,
    checkSession,
    setAuthMode,
    login: (fields) => authenticate("login", fields),
    register: (fields) => authenticate("register", fields),
    logout,
    refreshList,
    select,
    retry,
    reconnect,
    keepWaiting,
    goHome,
    frameLoaded,
    frameFailed,
    visibilityChanged,
    openCreate,
    closeCreate,
    createVm,
    dismissBanner,
  };
}

/* ------------------------------------------------------------------ */
/* 展示辅助                                                            */
/* ------------------------------------------------------------------ */

export const VM_STATE_LABELS = Object.freeze({
  stopped: "已停止",
  starting: "启动中",
  ready: "运行中",
  error: "启动失败",
  unknown: "状态未知",
});

// The overview row's action for each state: a VM that is already starting is only watched, not started again.
export const VM_ACTION_LABELS = Object.freeze({ ready: "打开", starting: "查看进度", stopped: "启动并打开", error: "启动并打开" });

export const TARGET_STATUS_LABELS = Object.freeze({
  opening: "正在连接",
  starting: "开机中",
  connecting: "正在载入",
  ready: "已连接",
  error: "无法打开",
});

/** 此前已在运行的 VM 返回 202 时显示“确认中”，与真正的开机区分。 */
export function targetStatusLabel(target) {
  if (target?.status === "starting" && target.warm) return "确认中";
  return TARGET_STATUS_LABELS[target?.status] || "";
}

export function formatElapsed(ms) {
  const total = Math.max(0, Math.floor(ms / 1000));
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const seconds = String(total % 60).padStart(2, "0");
  return hours ? `${hours}:${String(minutes).padStart(2, "0")}:${seconds}` : `${minutes}:${seconds}`;
}

export function displayName(vm) {
  return vm?.name || "未命名虚拟机";
}

function formatCreated(value) {
  const date = new Date(value);
  if (!value || Number.isNaN(date.getTime())) return "";
  try {
    return new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" }).format(date);
  } catch {
    return date.toISOString();
  }
}

export function createThemeController(doc, storage) {
  let mode = "system";
  try {
    const saved = storage?.getItem(THEME_KEY);
    if (THEMES.includes(saved)) mode = saved;
  } catch {
    /* 忽略 */
  }
  function apply() {
    const root = doc.documentElement;
    if (mode === "system") root.removeAttribute("data-theme");
    else root.setAttribute("data-theme", mode);
  }
  return {
    get mode() {
      return mode;
    },
    apply,
    cycle() {
      mode = THEMES[(THEMES.indexOf(mode) + 1) % THEMES.length];
      try {
        storage?.setItem(THEME_KEY, mode);
      } catch {
        /* 忽略 */
      }
      apply();
      return mode;
    },
  };
}

/* ------------------------------------------------------------------ */
/* DOM 视图（只用 createElement / textContent，不拼接 HTML）             */
/* ------------------------------------------------------------------ */

const SVG_NS = "http://www.w3.org/2000/svg";

// Functional icons: Lucide geometry on a 24px grid, drawn at stroke 1.75 (see styles.css .icon).
// A string is a path, { c: [cx, cy, r] } a circle and { r: [x, y, w, h, rx] } a rounded rectangle.
const ICONS = {
  plus: ["M5 12h14", "M12 5v14"],
  grid: [{ r: [3, 3, 7, 7, 1.5] }, { r: [14, 3, 7, 7, 1.5] }, { r: [14, 14, 7, 7, 1.5] }, { r: [3, 14, 7, 7, 1.5] }],
  refresh: ["M21 12a9 9 0 1 1-9-9c2.52 0 4.93 1 6.74 2.74L21 8", "M21 3v5h-5"],
  logout: ["M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4", "m16 17 5-5-5-5", "M21 12H9"],
  sun: [{ c: [12, 12, 4] }, "M12 2v2", "M12 20v2", "m4.93 4.93 1.41 1.41", "m17.66 17.66 1.41 1.41", "M2 12h2", "M20 12h2", "m6.34 17.66-1.41 1.41", "m19.07 4.93-1.41 1.41"],
  moon: ["M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"],
  monitor: [{ r: [2, 3, 20, 14, 2] }, "M8 21h8", "M12 17v4"],
  alert: [{ c: [12, 12, 10] }, "M12 8v4", "M12 16h.01"],
  info: [{ c: [12, 12, 10] }, "M12 16v-4", "M12 8h.01"],
  check: [{ c: [12, 12, 10] }, "m9 12 2 2 4-4"],
  close: ["M18 6 6 18", "m6 6 12 12"],
  chevron: ["m9 18 6-6-6-6"],
};

// Hand-drawn art in the brush hand of the Super Cage mark: filled outlines with thick-thin strokes, ink in
// currentColor and clay as the only colour (k: "clay" maps to a CSS class, so the CSP needs no inline styles).
// Generated offline from the centerline doodles by a brush renderer (round 1 of the portal redesign).
// parts reuses another drawing as [name, transform, indexes of its shapes to leave out].
const ART = {
  // Server, terminal, sun, moon and squiggle follow the phone doodles (ui/src/mobile-cli/doodles.jsx) line for line.
  server: {
    box: "0 0 120 90",
    shapes: [
      { d: "M30.6 11.9l5.8 -.4 21.6 -.4 9 .1 24 .8 .3 .8-.4 9-.1 5.3-.5 .8-.6 .4-7.1 .3-29.4 .2-7.2-.2-16.8-.3-.2-.8 .4 -14.4 .5 -.9zM29.4 9.5l-.9 .3-.8 .5-.6 .8-.3 .8-.3 4 0 10.2 .2 1.5 .3 .7 .6 .7 1.3 1 .7 .3 6.8 .5 10.2 .3 6 -.1 15.6 .2 21.7 -.5 .8 0 .9 -.3 .8 -.5 .6 -.7 .4 -.9 .3 -5.8 .1 -9.1-.2-.8-.4-.8-1.9-1.5-.7-.4-8-.6-9.6-.3-21-.3-10.2 .3zM30.3 36.5l2.9 -.2 14.4 -.1 6 -.2 9 0 28.3 .6 .3 .2 .2 .8-.3 5.6 0 8.3-.4 .7-.8 .5-3.4 .3-15.6 0-6.6 .2-29.4-.3-4.1-.2-1.4 0-.3-.2-.1-.4 .3 -7.2-.1-6.6 .1 -.8 .5 -.6zM29.1 34.2l-.8 .3-.8 .6-.5 .8-.3 .9-.3 7.5 .1 6.6 0 .7 .3 .8 .5 .8 2 1.5 .7 .2 19.9 .6 15 -.1 6.6 .2 18.8 -.4 .8 -.1 .9 -.3 .7 -.6 .6 -.7 .4 -.9 .3 -9.4-.2-5.6-.3-.8-.5-.6-1.9-1.5-.8-.3-11.5-.5-10.2-.2-21-.1z" },
      { d: "M35.9 20.1a2.5 2.5 0 1 0 5 0a2.5 2.5 0 1 0 -5 0zM43.7 20.1a2.5 2.5 0 1 0 5 0a2.5 2.5 0 1 0 -5 0zM36 44.8a2.5 2.5 0 1 0 5 0a2.5 2.5 0 1 0 -5 0zM43.8 44.8a2.5 2.5 0 1 0 5 0a2.5 2.5 0 1 0 -5 0z" },
      { d: "M62.4 20.9l3.6 .3 10.8 0 4.8 -.4 1.6 0 .2 -.2-.2-.2-6.4-1-4.2-.2-6 .1-1.2 .1-3.3 .6-.2 .4 .1 .3zM62.6 45.4l3.6 .3 10.8 0 6.2 -.4 .2 -.2-.2-.2-5.6-.9-8.4-.3-3 .1-3.6 .6-.3 .1-.2 .4 .1 .3z", o: 0.6 },
      { d: "M59.6 54.4l-.5 3 0 4.2-.3 2.8-.5 1.4-.4 .9-1.4 1.5-1.7 1.2-1.9 .7-2.8 .6-5.4 .3-1.8 .3-2.4 .7-2.9 1.1-.2 .2 .1 .3 .3 .1 1.2 -.4 1.7 -.2 4.7 0 7.4 -.5 1.9 -.4 1.3 -.5 1.2 -.7 1 -.9 1.4 -1.5 .7 -1.2 .6 -1.3 .4 -2 0 -1.9-.1-4.2-.4-2.4 0 -1.2-.1-.3-.5-.3-.3 .1z" },
      { d: "M84.6 62.4c.7 4.1 2.2 5.9 6.2 6.5-4 .7-5.5 2.4-6.2 6.6-.7-4.2-2.3-5.9-6.4-6.6 4.1-.6 5.7-2.4 6.4-6.5z", k: "clay" },
    ],
  },
  terminal: {
    box: "0 0 120 90",
    shapes: [
      { d: "M19.7 13.7l10.7 -.6 8.4 -.3 22.2 -.2 29.3 .7 12.1 .8 .4 .2 .2 .9 .1 7.2-.4 15.6 .1 15-.4 15 0 3.6-.1 .7-.4 .6-.9 .7-1.3 .3-6 .3-6 .1-15.6 .4-29.9-.2-15.6-.6-8.1-.2-1-.2-.4-.8-.2-1.3 .3 -8.2 0 -9.6-.1-6 .3 -15-.1-16.2 .2 -1 .3 -.6 .8 -.7zM18.8 11.2l-1.5 .4-1.4 1-.8 1.5-.2 1.6-.3 16.8 0 14.4-.2 15.6 .1 8.5 .2 1.4 .3 .7 .5 .7 1.7 1.4 .7 .4 1.4 .3 7.2 .5 31.2 .7 14.5 -.2 7.2 .1 21 -.7 1.6 -.2 .7 -.3 1.3 -1 .6 -.7 .5 -1.6 .1 -3.3 0 -16.2 .4 -14.9 0 -15.1 .2 -6.6-.4-1.5-.4-.7-.5-.6-1.8-1.4-1.4-.5-2.5-.2-23.4-1.1-29.4-.1-21.7 .6z" },
      { d: "M16.6 25l4.2 .4 27.6 -.5 21.6 .1 26.4 .6 5.4 -.3 2.4 -.1 .2 -.2-.2-.3-7.2-.8-1.8-.2-10.2-.3-21-.3-9 0-21.6 .1-12.6 .4-4.4 .6-.2 .4 0 .2z" },
      { d: "M21.4 18.4a2.2 2.2 0 1 0 4.5 0a2.2 2.2 0 1 0 -4.5 0zM28.1 18.3a2.2 2.2 0 1 0 4.5 0a2.2 2.2 0 1 0 -4.5 0zM34.8 18.4a2.2 2.2 0 1 0 4.5 0a2.2 2.2 0 1 0 -4.5 0z" },
      { d: "M30.2 39.5l2.7 2.4 4.3 3.2 4.5 3.8-.5-.4 0 -.8-2.2 2-3.7 2.9-3.3 3.5-2.3 2 0 .4 .4 .2 2.1 -1.1 3.4 -1.5 1.6 -.9 6.2 -4.9 1 -1.6-3.8-3.1-6.7-5.1-3.5-1.7-.3 .2-.1 .2z" },
      { d: "M50.4 59.8l3.6 .6 6 -.2 7.1 0 4.9 -.4 2.3 0 .2 -.1 .2 -.3-.1-.2-.3-.2-7.1-1.3-12.6-.2-1.2 .1-3.4 .9-.2 .3-.1 .3 .1 .4 .3 .2z", k: "clay" },
    ],
  },
  sun: {
    box: "21 6 78 78",
    shapes: [
      { d: "M60.4 28.2c9.6-.4 17.1 7.2 16.8 16.7-.3 9.2-7.8 16.4-17 16.2-9.4-.2-16.6-7.9-16.2-17.2.4-8.9 7.4-15.4 16.4-15.7z", k: "clay" },
      { d: "M59.9 8.6l-.7 1.7-.2 4.8 .8 2.5 0 .6 .4 .2 .3 -.2 .2 -.6 1 -2.3 .1 -4.8-.6-2.2-.2-.3-.7 0zM59.8 71.2l-.6 1.8 0 4.8 1.1 2.4 .1 .6 .3 .2 .3 -.2 .1 -.6 .9 -2.4-.2-4.2-.1-.6-.8-2.1-.5-.3-.3 .1zM24.4 45.4l1.8 .6 4.8 -.2 2.4 -.9 .8 -.1 .1 -.3-.1-.3-.8-.1-2.4-.8-4.2 .1-.6 .1-2.1 .8-.3 .5 .1 .3zM86.9 45l1.8 .7 4.8 0 3.1 -.8 .1 -.3-.1-.3-3.1-.9-4.8-.2-2.1 .6-.2 .3-.1 .6zM34.3 18.9l1 1.6 3.3 3.4 2.4 1.8 .4 0 0 -.4-1.6-2.5-3.4-3.5-1.7-1.1-.4 .1-.1 .2zM79.5 63.9l1.1 1.5 3.4 3.4 2.5 1.6 .3 -.1-.1-.3-1.6-2.4-3-2.9-2.2-1.5-.4 .1-.1 .2zM34.7 71.1l1.8 -.7 3.4 -3.4 .4 -.5 .7 -1.8 .5 -.6-.1-.4-.4-.1-.6 .4-1.9 .7-.5 .3-3.5 3.3-.5 1.3-.5 .8 0 .4 .2 .3 .3 .2 .4 0zM80.7 25.9l1.7 -.8 3.5 -3.4 .3 -.5 .7 -1.9 .4 -.7-.1-.4-.4-.1-.7 .5-1.9 .7-.5 .3-3.2 3.6-.5 1.2-.5 .8 0 .4 .2 .3 .3 .2 .4 0z" },
    ],
  },
  moon: {
    box: "28 11 74 68",
    shapes: [
      { d: "M69.2 14.8l-1.7-.6-2.9 1.3-3.7 2.4-2 1.6-1.3 1.4-1.6 2-1.8 2.6-1.2 2.3-.7 1.7-.7 2.5-.4 2.5-.3 2.5 0 1.9 .3 2.5 .6 2.5 .7 2.4 1 2.3 .9 1.7 1.3 2 1.6 2 1.7 1.8 2.9 2.3 1.5 1.1 2.2 1.2 2.2 1.1 2.4 .8 1.8 .5 2.5 .3 3.1 .2 2.5 0 1.9 -.3 0 -.4-1-1.2-1.6 2.1-1.1 1.3-2 2-2.6 2.1-1.9 1.3-1.5 .8-4.3 1.6-3.3 .7-2.9 .1-4.6-.3-3.3-.8-2.2-.7-2.7-1.2-2-1.2-1.4-1-1.7-1.6-1.6-1.7-1.4-1.9-1.2-2-1-2-.9-2.7-.8-2.8-.4-2.3-.1-2.2 .1 -3.5 .2 -2.2 .6 -2.2 .7 -2.2 1.3 -2.6 1.1 -1.9 1.3 -1.9 1.9 -2.1 2.8 -2.8 1.8 -1.4 1.9 -1.2 2.6 -1.3 2.6 -1.1 1.7 -.5 2.2 -.4 3.4 -.2 2.9 0 2.9 .3 2.5 .6 1 -1.6-.1-5zM67.3 16.1l-.1 4.6 .7 -.7 .2 .2-5.5-.9-3.8 0-3.7 .6-3.6 .9-3 1.2-3.3 1.8-3.1 2.2-1.9 1.6-1.3 1.4-1.9 2.5-1.7 2.7-1.1 2.3-.7 1.7-1.2 4.9-.4 2.5-.1 2.5 .2 3.2 .6 3 .7 2.5 .9 2.3 1.2 2.2 1.7 2.6 1.9 2.4 1.8 1.7 3.4 2.7 2 1.5 1.6 .9 2.3 1 2.9 1 2.4 .6 3.8 .4 4.3 -.3 4.4 -.7 3.5 -1.3 3.9 -2.1 1.6 -1.1 1.5 -1.2 2.6 -2.8 1.5 -2 1.1 -1.8-1-1.8-1.1-.2-2.8 .5-2.3 .1-2.9-.2-2.2-.4-2.8-.8-2.2-.9-2-1-2.8-2-2.9-2.9-1-1.4-1.2-2-1.6-3.7-.5-1.6-.4-2.3-.2-2.8 .1 -2.8 .5 -3.4 1.4 -4.2 1 -2.1 1.2 -1.9 1.4 -1.8 1.6 -1.6 1.7 -1.4 1.8 -1.3 2 -1.1 2.1 -.8-.5-1z" },
      { d: "M84.4 20.2c.6 3.4 1.8 4.8 5.1 5.4-3.3.6-4.5 2-5.1 5.5-.6-3.5-1.9-4.9-5.3-5.5 3.4-.6 4.7-2 5.3-5.4zM95.6 38.8c.4 2 1 2.8 3 3.1-2 .4-2.6 1.2-3 3.2-.4-2-1.1-2.8-3.1-3.2 2-.3 2.7-1.1 3.1-3.1z", k: "clay" },
      { d: "M33.2 71.1l1.7 -.4 3.8 .3 2.2 .5 4.1 1.5 2.5 .4 1.3 -.2 1.2 -.3 3.1 -1.4 .2 -.3-.3-.3-1.5 .5-1.7 .2-3.9-.2-2.2-.5-4.6-1.7-2.6-.3-1.3 .2-2.4 1-.2 .2-.1 .5 .2 .2z", o: 0.6 },
    ],
  },
  squiggle: {
    box: "0 0 200 16",
    shapes: [
      { d: "M3.8 11.4l1.2 -.3 2.3 -.4 4.9 -1.8 2.7 -.7 2.8 -.3 4 .2 2.8 .4 2.7 .7 3.8 1.5 5 2.3 3.6 1.2 4.4 .7 2.5 .1 2.6 0 1.9 -.2 1.8 -.4 4.3 -1.4 2.9 -1.2 4.1 -2.3 2.6 -1 5 -1.1 3.3 -.2 4.5 .5 4 .9 2.7 1 6 2.9 2.9 1.1 2.4 .7 3.1 .5 4.5 .1 3.2 -.3 3 -.8 4.7 -1.8 5.9 -2.8 2.1 -.8 4.4 -.8 4.5 -.4 4 .5 4.4 1.1 2.7 1 4.9 2.6 4.2 1.4 3.7 .8 3.2 .2 4.4 -.5 3.7 -.8 3 -1.1 7 -3.2 2.7 -1 2.1 -.6 2.8 -.3 6.3 -.4 2.4 .2 1.9 .4 .2 -.4-.2-.2-4.5-1.9-2.5-.8-2.5-.3-2.6 .1-3.8 .6-3 .9-2.4 1-4.2 2.3-5.3 1.9-2.2 .5-1.7 .2-4.5 0-3.4-.3-3.3-.9-3.3-1.2-6.1-2.9-2.9-1-4.4-.9-4.4-.2-1.9 .2-1.9 .3-4.3 1.3-3.5 1.4-4.2 2.1-2.6 1-5.5 1.3-1.7 .1-2.3 .1-4.5-.5-3.3-.8-2.7-1.1-6-2.9-2.3-1-2.4-.7-3.2-.6-4.4-.1-3.8 .4-3.1 .9-5.3 1.9-5.2 2.5-2.7 .9-5 1.1-3.4 .2-3.9-.4-3.9-1-2.1-.9-4.9-2.5-4.2-1.4-3.7-.8-3.2-.1-3.1 .2-3.8 .6-2.4 .6-2.4 .9-1.1 .6-2.8 2.3-.1 .5 .1 .4 .3 .4 .5 .1z", k: "clay" },
    ],
  },
  // Portal-only: a plug pulled from its socket (connection trouble) and a server letting off smoke (boot failure).
  unplugged: {
    box: "0 0 120 90",
    shapes: [
      { d: "M6.4 71.2l3.8 .1 2.5 -.5 1.8 -.6 1.8 -.7 1.7 -.9 3 -2.3 2.3 -2.3 1.4 -2.1 2.7 -4.8 2 -4.3 1.3 -1.9 .1 -.2-.1-.4-.2-.1-.4 .1-3.8 4-4.6 6.9-1.4 1.8-1.6 1.5-2.2 1.7-3 1.5-6 2.1-1.4 .3-.3 .5 .1 .3z" },
      { d: "M30.9 41.8l4 -.8 4.1 -.4 5.2 0 4.4 .4 .3 .3 .1 .4-.3 9.6 0 5.4-.1 .9-.3 .4-.5 .3-.9 .3-3.5 .4-2.3 .2-7.1-.3-3.7-.3-.3-.2-.3-8.2 0 -7.2 .2 -.6 .4 -.4 .2 -.2zM29.4 39.4l-.8 .4-.7 .6-.5 .8-.3 .9 .2 8.7-.1 6 .1 .7 .3 .8 .5 .7 2 1.5 .7 .3 3 .5 3.1 .2 5.5 -.1 4.3 -.3 2 -.3 .8 -.3 .8 -.5 .6 -.8 .3 -.9 .3 -6.3 0 -9 0 -.8-.3-.8-.5-.7-1.9-1.5-.7-.2-6.1-.8-3.1 0-3.7 .2-4.9 .6z" },
      { d: "M50.8 46.2l1.8 .5 3.6 -.1 1.8 -.6 .7 -.1 .2 -.3-.1-.3-2.6-1-3.7 0-2.1 .8-.2 .2 .1 .7zM50.6 54.8l1.8 .5 3.5 .1 1.9 -.6 .7 -.1 .2 -.3-.1-.3-2.5-1-3.7-.1-2.1 .7-.3 .5 .1 .3z" },
      { d: "M74.4 39.1l1.9 -.3 2.9 -.4 6.5 -.1 4.1 .2 3.8 .6 .3 .3 .2 .8-.3 7.3-.1 11.4-.1 .9-.5 .7-.6 .4-1 .2-7.7 .4-3.5-.1-6.8-.3-.5-.2-.2-.8 .4 -13.9-.1-5.4 .2 -.7 .6 -.6 1.3 -.6zM73.2 36.7l-.9 .3-.8 .5-.5 .8-.4 .8-.1 .9-.2 18.8 .2 1.4 .4 .8 .6 .6 1.9 1.4 2.5 .5 3.6 .3 6.1 .1 3.7 -.2 4.6 -.6 .8 -.4 .6 -.6 .6 -.7 .3 -.9 .1 -.8 .3 -12.1 0 -6.7-.1-.7-.3-.8-.5-.7-1.9-1.4-.7-.3-7.3-1-2.4-.1-3.1 0-7.1 .6z" },
      { d: "M77.5 45.6a2.2 2.2 0 1 0 4.5 0a2.2 2.2 0 1 0 -4.5 0zM77.7 54.4a2.2 2.2 0 1 0 4.5 0a2.2 2.2 0 1 0 -4.5 0z" },
      { d: "M95.6 51l1.1 .2 5 1.4 2 .9 1.5 .9 2 2 2.8 3.8 1.3 1.4 1.9 1.6 2.9 2.2 .3 -.1-.1-.3-1.3-1.1-1.2-1.4-3.8-5.2-1.2-1.5-1.7-1.7-2.5-2-2.8-1.5-1.8-.6-1.2-.3-3.5 .2-.3 .5 .1 .3z" },
      { d: "M66 33.2l.7 -1.2 0 -3-.1-.6-.6-1.2-.1-.6-.3-.1-.3 .1-.1 .6-.7 1.2-.3 3.6 .6 1.5 .3 .3 .6 0zM70.3 36.1l1.3 -.4 2.1 -2 .4 -1.4 .4 -.7-.1-.4-.4-.1-.7 .3-1.4 .3-2.1 2.1-.6 1.6 0 .4 .4 .4zM61.3 35.1l-.5-1.1-1.9-2.3-1.6-.9-.3 0 0 .2 .6 1.8 2 2.2 1.3 .8 .4 -.1 .1 -.2z", k: "clay" },
    ],
  },
  // The server without its sparkle (shape 4), plus smoke and a clay short-circuit spark.
  fault: {
    box: "0 -14 120 104",
    parts: [["server", null, [4]]],
    shapes: [
      { d: "M74.9 6.1l-.2-.4-.1-1 .7 -1.3 .8 -2.1 .3 -1.5 0 -.7-.4-1.5-1.1-1.7-.1-.5 0 -1.3 .8 -2.5 .2 -2-.4-2-.6-1.1-.2-.2-.3 .3 .4 1.2-.1 1-1.6 3-.4 1.3-.1 .8 .2 1.5 1.1 1.8 .3 1-.2 1.2-1.2 3.2 0 1.6 .7 1.4 .9 1.1 .1 .1 .5 -.1 .1 -.1z", o: 0.5 },
      { d: "M83.7 6.4l-.3-.5 0 -1 1.3 -3 .3 -1.4-.2-1.5-.9-1.8-.2-.4 .1 -.9 .7 -2.6 .1 -1.3-.2-1.4-.6-1.1-.2-.1-.2 .2 .2 .6 0 1.1-1.6 3-.3 1.5 .3 1.5 .8 1.2 .2 .9-.2 1.3-.9 2.8 .1 1.5 .3 .7 .8 1.1 .2 .1 .3 0 .1 -.2z", o: 0.36 },
      { d: "M97.1 6.7l1.3 -.3 2.2 -2.1 .4 -.5 .3 -1.4 .3 -.5-.1-.4-.4-.1-.5 .3-1.4 .2-.6 .4-2.1 2.1-.6 1.6 0 .4 .2 .3 .3 .2 .4 0zM98.5 13l1.2 .4 3 -.3 .6 -.1 1.1 -.9 .6 -.2 .1 -.3-.2-.3-.6-.1-1.3-.6-.6 0-3 .5-1.4 .8-.2 .3 .1 .6zM91.9 3.5l.7 -1.2 .3 -3-.1-.6-.6-1.2-.1-.4-.3-.2-.3 .1-.1 .4-.8 1.1-.3 .6-.3 3 .6 1.6 .2 .3 .3 .1 .4 -.1 .3 -.2z", k: "clay" },
    ],
  },
};

const THEME_LABELS = { system: "跟随系统", light: "浅色", dark: "深色" };
const THEME_ICONS = { system: "monitor", light: "sun", dark: "moon" };

function greetingWord(date = new Date()) {
  const hour = date.getHours();
  if (hour < 5) return "夜深了";
  if (hour < 11) return "早上好";
  if (hour < 13) return "中午好";
  if (hour < 18) return "下午好";
  return "晚上好";
}

function overviewSummary(vms) {
  const count = (state) => vms.filter((vm) => vm.state === state).length;
  const parts = [`你有 ${vms.length} 台虚拟机`];
  if (count("ready")) parts.push(`${count("ready")} 台正在运行`);
  if (count("starting")) parts.push(`${count("starting")} 台正在开机`);
  if (count("error")) parts.push(`${count("error")} 台启动失败`);
  return `${parts.join("，")}。`;
}

export function mountPortal(root, portal, { doc = root.ownerDocument, win = doc.defaultView, theme = null } = {}) {
  let screen = null;
  let uidSeq = 0;
  const uid = (prefix) => `${prefix}-${++uidSeq}`;

  function h(tag, props = {}, children = []) {
    const el = doc.createElement(tag);
    for (const [key, value] of Object.entries(props)) {
      if (value === null || value === undefined || value === false) continue;
      if (key === "class") el.className = value;
      else if (key === "text") el.textContent = value;
      else if (key.startsWith("on") && typeof value === "function") el.addEventListener(key.slice(2), value);
      else el.setAttribute(key, value === true ? "" : String(value));
    }
    for (const child of [].concat(children)) {
      if (child === null || child === undefined || child === false) continue;
      el.append(typeof child === "string" ? doc.createTextNode(child) : child);
    }
    return el;
  }

  function svgNode(tag, attrs) {
    const node = doc.createElementNS(SVG_NS, tag);
    for (const [key, value] of Object.entries(attrs)) {
      if (value !== null && value !== undefined) node.setAttribute(key, String(value));
    }
    return node;
  }

  function icon(name, className = "icon") {
    const svg = svgNode("svg", { viewBox: "0 0 24 24", "aria-hidden": "true", focusable: "false", class: className });
    for (const part of ICONS[name] || []) {
      if (typeof part === "string") svg.append(svgNode("path", { d: part }));
      else if (part.c) svg.append(svgNode("circle", { cx: part.c[0], cy: part.c[1], r: part.c[2] }));
      else if (part.r) svg.append(svgNode("rect", { x: part.r[0], y: part.r[1], width: part.r[2], height: part.r[3], rx: part.r[4] }));
    }
    return svg;
  }

  function drawing(name, className) {
    const spec = ART[name];
    const svg = svgNode("svg", { viewBox: spec.box, fill: "currentColor", "aria-hidden": "true", focusable: "false", class: className });
    const add = (shape, transform = null) =>
      svg.append(svgNode("path", { d: shape.d, opacity: shape.o, transform, class: shape.k ? "k-clay" : null }));
    for (const [part, transform, skip = []] of spec.parts || []) {
      ART[part].shapes.forEach((shape, index) => skip.includes(index) || add(shape, transform));
    }
    spec.shapes.forEach((shape) => add(shape));
    return svg;
  }

  // The brand mark is defined once as a symbol sprite in index.html; "full" from 40px, "small" for 16–32px.
  function mark(kind = "small") {
    const svg = svgNode("svg", { viewBox: "0 0 256 256", "aria-hidden": "true", focusable: "false", class: `mark mark-${kind}` });
    svg.append(svgNode("use", { href: kind === "full" ? "#sc-mark" : "#sc-mark-small" }));
    return svg;
  }

  function brand() {
    return h("span", { class: "brand" }, [mark("small"), h("span", { class: "brand-name", text: "Super Cage" })]);
  }

  function themeButton() {
    const button = h("button", { type: "button", class: "icon-btn", "data-focus-key": "theme" });
    const sync = () => {
      const mode = theme?.mode || "system";
      const label = `主题：${THEME_LABELS[mode]}（点击切换）`;
      button.replaceChildren(icon(THEME_ICONS[mode]));
      button.setAttribute("aria-label", label);
      button.setAttribute("title", label);
    };
    button.addEventListener("click", () => {
      theme?.cycle();
      sync();
    });
    sync();
    if (!theme) button.hidden = true;
    return button;
  }

  function pageHeader({ withBrand = true } = {}) {
    return h("header", { class: "page-head" }, [withBrand ? brand() : h("span"), themeButton()]);
  }

  // 忙碌时用 aria-disabled 而不是 disabled，避免键盘焦点掉到 <body>；重复点击由控制器忽略。
  function setBusy(button, busy) {
    button.setAttribute("aria-disabled", busy ? "true" : "false");
  }

  function setFieldError(input, el, message) {
    el.textContent = message || "";
    el.hidden = !message;
    input.setAttribute("aria-invalid", message ? "true" : "false");
  }

  // A quiet one-line message with a leading icon; only the text part changes.
  function note(className, iconName, role) {
    const text = h("span");
    const detailText = h("span", { class: "note-detail", hidden: true });
    const el = h("div", { class: `note ${className}`, role, hidden: true }, [icon(iconName), h("span", { class: "note-body" }, [text, detailText])]);
    return {
      el,
      // detail: the server's own words when they could not be translated, kept small for support.
      set(message, detail = "") {
        text.textContent = message || "";
        detailText.textContent = detail || "";
        detailText.hidden = !detail;
        el.hidden = !message;
      },
    };
  }

  // Chinese text may break between any two characters, so a balanced line can still split a word ("工/作台").
  // Each clause becomes an inline-block, so lines break between clauses and only split one that is too long.
  function setProse(el, value, pattern = /[^，。；：！？]+[，。；：！？]*|[，。；：！？]+/g) {
    const clauses = String(value || "").match(pattern) || [];
    el.replaceChildren(...clauses.map((clause) => h("span", { class: "clause", text: clause })));
    return el;
  }

  function focusByKey(container, key) {
    if (!key) return false;
    const match = [...container.querySelectorAll("[data-focus-key]")].find((el) => el.dataset.focusKey === key);
    if (match) {
      match.focus({ preventScroll: true });
      return true;
    }
    return false;
  }

  /* ---- 启动中 ---- */

  function buildBooting() {
    const el = h("main", { class: "center-screen" }, [
      h("div", { class: "splash", role: "status" }, [
        mark("full"),
        h("p", { class: "splash-name", text: "Super Cage" }),
        h("p", { class: "splash-line" }, [h("span", { class: "live-dot", "aria-hidden": "true" }), "正在检查登录状态…"]),
      ]),
    ]);
    return { phase: "booting", el, update() {} };
  }

  /* ---- 会话检查失败（与未登录区分） ---- */

  function buildSessionError() {
    const message = h("p", { class: "hero-text" });
    const detail = h("p", { class: "hero-detail", hidden: true });
    const retry = h("button", { type: "button", class: "btn btn-primary btn-lg", onclick: () => portal.boot() }, "重新连接");
    const el = h("div", { class: "page" }, [
      pageHeader(),
      h("main", { class: "page-main" }, [
        h("section", { class: "hero", role: "alert", "aria-labelledby": "session-error-title" }, [
          drawing("unplugged", "art hero-art"),
          h("h1", { id: "session-error-title", class: "hero-title", text: "暂时无法连接门户服务" }),
          message,
          detail,
          setProse(h("p", { class: "hero-text" }), "这不代表你已退出登录。"),
          h("div", { class: "actions" }, [retry]),
        ]),
      ]),
    ]);
    return {
      phase: "session-error",
      el,
      update(state) {
        setProse(message, state.bootError?.message || "请稍后重试。");
        detail.textContent = state.bootError?.detail || "";
        detail.hidden = !state.bootError?.detail;
      },
      focus: () => retry.focus(),
    };
  }

  /* ---- 登录 / 注册 ---- */

  function buildAuth() {
    const ids = {
      title: uid("auth-title"),
      lead: uid("auth-lead"),
      user: uid("auth-user"),
      userHint: uid("auth-user-hint"),
      userError: uid("auth-user-error"),
      pass: uid("auth-pass"),
      passHint: uid("auth-pass-hint"),
      passError: uid("auth-pass-error"),
      confirm: uid("auth-confirm"),
      confirmError: uid("auth-confirm-error"),
    };
    const title = h("h1", { id: ids.title, class: "auth-title" });
    const lead = h("p", { id: ids.lead, class: "auth-lead" });
    const loginTab = h("button", { type: "button", class: "seg", onclick: () => portal.setAuthMode("login") }, "登录");
    const registerTab = h("button", { type: "button", class: "seg", onclick: () => portal.setAuthMode("register") }, "注册");
    const userInput = h("input", {
      id: ids.user,
      name: "username",
      type: "text",
      autocomplete: "username",
      autocapitalize: "none",
      autocorrect: "off",
      spellcheck: "false",
      maxlength: "64",
      required: true,
      "aria-describedby": `${ids.userHint} ${ids.userError}`,
    });
    const userHint = h("p", { id: ids.userHint, class: "field-hint", text: "3–32 个字符：英文字母、数字和 . _ -", hidden: true });
    const userError = h("p", { id: ids.userError, class: "field-error", hidden: true });
    const passInput = h("input", {
      id: ids.pass,
      name: "password",
      type: "password",
      autocomplete: "current-password",
      required: true,
      "aria-describedby": `${ids.passHint} ${ids.passError}`,
    });
    const passHint = h("p", { id: ids.passHint, class: "field-hint", text: "至少 12 个字符。", hidden: true });
    const passError = h("p", { id: ids.passError, class: "field-error", hidden: true });
    const confirmInput = h("input", {
      id: ids.confirm,
      name: "confirm",
      type: "password",
      autocomplete: "new-password",
      "aria-describedby": ids.confirmError,
    });
    const confirmError = h("p", { id: ids.confirmError, class: "field-error", hidden: true });
    const confirmField = h("div", { class: "field" }, [
      h("label", { for: ids.confirm, text: "确认密码" }),
      confirmInput,
      confirmError,
    ]);
    const notice = note("auth-notice", "check", "status");
    const formError = note("form-error", "alert", "alert");
    const submit = h("button", { type: "submit", class: "btn btn-primary btn-lg btn-block" });
    const form = h("form", { class: "auth-form", novalidate: true, "aria-labelledby": ids.title }, [
      h("div", { class: "field" }, [h("label", { for: ids.user, text: "用户名" }), userInput, userHint, userError]),
      h("div", { class: "field" }, [h("label", { for: ids.pass, text: "密码" }), passInput, passHint, passError]),
      confirmField,
      formError.el,
      submit,
    ]);
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      const fields = { username: userInput.value, password: passInput.value, confirm: confirmInput.value };
      if (portal.getState().auth.mode === "register") portal.register(fields);
      else portal.login(fields);
    });
    const backToLogin = h("button", { type: "button", class: "btn btn-primary btn-lg btn-block", "data-focus-key": "back-to-login", onclick: () => portal.setAuthMode("login") }, "返回登录");
    const closedPanel = h("section", { class: "closed-panel", role: "alert", hidden: true }, [
      h("h2", { class: "closed-title", text: "暂未开放自助注册" }),
      setProse(h("p"), "这个门户的账户由管理员开通，请联系管理员获取。"),
      backToLogin,
    ]);
    const el = h("div", { class: "page auth-screen" }, [
      pageHeader({ withBrand: false }),
      h("main", { class: "page-main" }, [
        h("section", { class: "auth-card", "aria-labelledby": ids.title, "aria-describedby": ids.lead }, [
          mark("full"),
          title,
          lead,
          h("div", { class: "auth-panel" }, [
            h("div", { class: "segmented", role: "group", "aria-label": "选择登录或注册" }, [loginTab, registerTab]),
            notice.el,
            closedPanel,
            form,
          ]),
        ]),
      ]),
    ]);
    let lastMode = null;
    let lastError = null;
    let lastClosed = false;
    return {
      phase: "signed-out",
      el,
      update(state) {
        const { mode, busy, error, notice: note } = state.auth;
        const register = mode === "register";
        const closed = register && state.registrationClosed;
        const focusWasInPanel = closedPanel.contains(doc.activeElement);
        closedPanel.hidden = !closed;
        form.hidden = closed;
        if (closed && !lastClosed && el.contains(doc.activeElement)) backToLogin.focus();
        // 提示面板隐藏时焦点会丢失，交给用户名输入框。
        if (!closed && lastClosed && (focusWasInPanel || doc.activeElement === doc.body)) userInput.focus();
        lastClosed = closed;
        title.textContent = register ? "创建门户账户" : "登录 Super Cage 门户";
        setProse(lead, register ? "注册后就能创建自己的虚拟机。" : "打开你的虚拟机，工作台一直在服务器上运行。");
        lead.hidden = closed;
        loginTab.setAttribute("aria-pressed", String(!register));
        registerTab.setAttribute("aria-pressed", String(register));
        setBusy(loginTab, busy);
        setBusy(registerTab, busy);
        passInput.setAttribute("autocomplete", register ? "new-password" : "current-password");
        userHint.hidden = !register;
        passHint.hidden = !register;
        confirmField.hidden = !register;
        confirmInput.disabled = !register;
        setBusy(submit, busy);
        submit.textContent = busy ? (register ? "正在注册…" : "正在登录…") : register ? "注册并登录" : "登录";
        form.setAttribute("aria-busy", String(busy));
        setFieldError(userInput, userError, error?.field === "username" ? error.message : "");
        setFieldError(passInput, passError, error?.field === "password" ? error.message : "");
        setFieldError(confirmInput, confirmError, error?.field === "confirm" ? error.message : "");
        formError.set(error && !error.field ? error.message : "");
        notice.set(note);
        if (mode !== lastMode) {
          if (lastMode !== null) confirmInput.value = "";
          lastMode = mode;
        }
        if (error && error !== lastError && error.field) {
          ({ username: userInput, password: passInput, confirm: confirmInput })[error.field]?.focus();
        }
        lastError = error;
      },
      focus: () => userInput.focus(),
    };
  }

  /* ---- 已登录工作台 ---- */

  function buildShell() {
    const stageId = uid("stage");
    const tabEls = new Map();
    const home = h("button", { type: "button", class: "icon-btn home-btn", "aria-label": "虚拟机总览", title: "虚拟机总览", onclick: () => portal.goHome() }, [
      icon("grid"),
      h("span", { class: "btn-text", text: "总览" }),
    ]);
    const tablist = h("div", { class: "tabs", role: "tablist", "aria-label": "我的虚拟机" });
    const addButton = h("button", { type: "button", class: "icon-btn add-btn", "aria-label": "新建虚拟机", title: "新建虚拟机", onclick: () => portal.openCreate() }, icon("plus"));
    const avatar = h("span", { class: "avatar", "aria-hidden": "true" });
    const userLabel = h("span", { class: "user-label" });
    const userChip = h("span", { class: "user-chip", title: "当前门户账户" }, [avatar, userLabel]);
    const logoutButton = h("button", { type: "button", class: "icon-btn logout-btn", onclick: () => portal.logout() }, [
      icon("logout"),
      h("span", { class: "btn-text", text: "退出" }),
    ]);
    const skipToSwitcher = h("button", { type: "button", class: "skip-link" }, "跳到虚拟机切换");
    const skipToStage = h("button", { type: "button", class: "skip-link" }, "跳到工作台");

    // The open VM's state sits in the bar itself, so the workbench below keeps all the height.
    const ctxStatusText = h("span");
    const ctxStatus = h("span", { class: "chip ctx-status" }, [h("span", { class: "status-dot", "aria-hidden": "true" }), ctxStatusText]);
    const ctxSystem = h("span", { class: "ctx-system" });
    const ctxSep = h("span", { class: "ctx-sep", "aria-hidden": "true", text: "·" });
    const ctxId = h("span", { class: "ctx-id mono" });
    const reconnectButton = h("button", { type: "button", class: "icon-btn", "data-focus-key": "reconnect", "aria-label": "重新连接当前虚拟机", title: "重新连接", onclick: () => portal.reconnect() }, icon("refresh"));
    const contextBar = h("div", { class: "contextbar", role: "region", "aria-label": "当前虚拟机", hidden: true }, [
      ctxStatus,
      h("span", { class: "ctx-meta" }, [ctxSystem, ctxSep, ctxId]),
      reconnectButton,
    ]);

    const bannerText = h("span", { class: "banner-text" });
    const bannerAction = h("button", { type: "button", class: "btn btn-sm" });
    const bannerClose = h("button", { type: "button", class: "icon-btn", "aria-label": "关闭提示", onclick: () => portal.dismissBanner() }, icon("close"));
    const bannerIcon = h("span", { class: "banner-icon" }, icon("alert"));
    const banner = h("div", { class: "banner", role: "alert", hidden: true }, [bannerIcon, bannerText, bannerAction, bannerClose]);

    const frameHost = h("div", { class: "frame-host" });
    const overlay = h("div", { class: "overlay" });
    const stage = h("main", { id: stageId, class: "stage", tabindex: "-1", "aria-label": "工作台" }, [frameHost, overlay]);
    const live = h("p", { class: "sr-only", "aria-live": "polite" });

    const dialog = buildCreateDialog();
    const barRule = h("span", { class: "bar-rule", "aria-hidden": "true" });
    const switcher = h("nav", { class: "switcher", "aria-label": "虚拟机切换" }, [tablist, addButton]);

    const el = h("div", { class: "shell" }, [
      skipToSwitcher,
      skipToStage,
      h("header", { class: "topbar" }, [
        h("div", { class: "bar-start" }, [brand(), home]),
        barRule,
        switcher,
        contextBar,
        h("div", { class: "account" }, [themeButton(), userChip, logoutButton]),
      ]),
      banner,
      stage,
      live,
      dialog.el,
    ]);

    let mountedFrame = null;
    const shellListeners = new AbortController();
    let frameRequest = 0;
    let parkedFocus = null;
    let overlaySig = null;
    let liveKey = "";
    let lastSelected = null;
    let ticker = null;
    let current = portal.getState();

    function selectedTab() {
      return tabEls.get(current.selectedId)?.tab || tablist.querySelector("[role=tab]");
    }

    skipToSwitcher.addEventListener("click", () => (selectedTab() || home).focus());
    skipToStage.addEventListener("click", () => (mountedFrame?.el || stage).focus());

    win.addEventListener("message", async (event) => {
      const frame = mountedFrame;
      if (!frame || event.source !== frame.el.contentWindow || event.origin !== frame.origin) return;
      const { type, vm_id: id } = event.data || {};
      if (type === "cage:virtual-machines-changed") {
        void portal.refreshList();
        return;
      }
      if (phoneMessage(frame, event.data || {})) return;
      if (type !== "cage:open-virtual-machine" || !isValidVmId(id)) return;
      const request = ++frameRequest;
      if (!current.vms.some((vm) => vm.id === id)) await portal.refreshList();
      if (request !== frameRequest || mountedFrame !== frame || current.logout.busy) return;
      if (current.vms.some((vm) => vm.id === id)) {
        event.source.postMessage({ type: "cage:open-virtual-machine-accepted", vm_id: id }, frame.origin);
        portal.select(id);
      } else {
        event.source.postMessage({ type: "cage:open-virtual-machine-failed", vm_id: id }, frame.origin);
      }
    }, { signal: shellListeners.signal });

    tablist.addEventListener("keydown", (event) => {
      const tabs = [...tablist.querySelectorAll("[role=tab]")];
      const index = tabs.indexOf(doc.activeElement);
      if (index < 0 || !tabs.length) return;
      const keys = { ArrowRight: index + 1, ArrowDown: index + 1, ArrowLeft: index - 1, ArrowUp: index - 1, Home: 0, End: tabs.length - 1 };
      if (!(event.key in keys)) return;
      event.preventDefault();
      const next = (keys[event.key] + tabs.length) % tabs.length;
      tabs.forEach((tab, i) => (tab.tabIndex = i === next ? 0 : -1));
      tabs[next].focus();
      tabs[next].scrollIntoView?.({ block: "nearest", inline: "nearest" });
    });

    // Fade an edge of the tab strip only while a tab actually reaches past it (measured on the tabs, not on
    // scrollLeft, so the strip's own padding never leaves a fade over a fully visible selected tab).
    function updateTabFade() {
      const tabs = tablist.querySelectorAll("[role=tab]");
      const strip = tablist.getBoundingClientRect();
      const first = tabs[0]?.getBoundingClientRect();
      const last = tabs[tabs.length - 1]?.getBoundingClientRect();
      tablist.dataset.fadeStart = String(!!first && first.left < strip.left - 1);
      tablist.dataset.fadeEnd = String(!!last && last.right > strip.right + 1);
    }
    tablist.addEventListener("scroll", updateTabFade, { passive: true });
    win.addEventListener("resize", updateTabFade, { signal: shellListeners.signal });
    // Keep the selected tab clear of the edge fade after the strip narrows (the context bar widens as a VM's
    // status text changes), which can push a tab that was in view on selection back under the edge.
    function revealSelectedTab() {
      const tab = tablist.querySelector('[role=tab][aria-selected="true"]');
      if (!tab) return;
      const strip = tablist.getBoundingClientRect();
      const box = tab.getBoundingClientRect();
      const edge = 40;
      if (box.right > strip.right - 4) tablist.scrollLeft += box.right - strip.right + edge;
      else if (box.left < strip.left + 4) tablist.scrollLeft -= strip.left - box.left + edge;
    }
    // The strip also changes width when the context bar appears or the banner shows, without a window resize.
    const tabResize = typeof win.ResizeObserver === "function"
      ? new win.ResizeObserver(() => {
          revealSelectedTab();
          updateTabFade();
        })
      : null;
    tabResize?.observe(tablist);

    function createTab(id) {
      const dot = h("span", { class: "status-dot", "aria-hidden": "true" });
      const name = h("span", { class: "tab-name" });
      const status = h("span", { class: "sr-only" });
      const tab = h("button", { type: "button", role: "tab", class: "tab", "aria-controls": stageId, "data-focus-key": `tab-${id}` }, [dot, name, status]);
      tab.addEventListener("click", () => portal.select(id));
      return { tab, name, status };
    }

    function updateTabs(state) {
      const ids = new Set(state.vms.map((vm) => vm.id));
      for (const [id, entry] of tabEls) {
        if (!ids.has(id)) {
          entry.tab.remove();
          tabEls.delete(id);
        }
      }
      let previous = null;
      const focusedTab = tablist.contains(doc.activeElement) ? doc.activeElement : null;
      state.vms.forEach((vm, index) => {
        let entry = tabEls.get(vm.id);
        if (!entry) {
          entry = createTab(vm.id);
          tabEls.set(vm.id, entry);
        }
        const selected = vm.id === state.selectedId;
        const target = selected ? state.target : null;
        const label = target ? targetStatusLabel(target) : VM_STATE_LABELS[vm.state];
        entry.tab.setAttribute("aria-selected", String(selected));
        const rovingHere = focusedTab ? focusedTab === entry.tab : selected || (!state.selectedId && index === 0);
        entry.tab.tabIndex = rovingHere ? 0 : -1;
        entry.tab.dataset.state = vm.state;
        entry.tab.dataset.status = target?.status || "";
        entry.name.textContent = displayName(vm);
        entry.status.textContent = `，${label}`;
        const system = vm.system_label ? ` · ${vm.system_label}` : "";
        entry.tab.setAttribute("title", `${displayName(vm)}${system} · ${label} · ${vm.id.slice(0, 8)}`);
        const expected = previous ? previous.nextSibling : tablist.firstChild;
        if (expected !== entry.tab) tablist.insertBefore(entry.tab, expected);
        previous = entry.tab;
      });
      if (state.selectedId !== lastSelected) {
        lastSelected = state.selectedId;
        tabEls.get(state.selectedId)?.tab.scrollIntoView?.({ block: "nearest", inline: "nearest" });
      }
    }

    function updateContext(state) {
      const target = state.target;
      home.setAttribute("aria-current", target ? "false" : "page");
      if (!target) {
        contextBar.hidden = true;
        contextBar.dataset.status = "none";
        return;
      }
      const vm = state.vms.find((item) => item.id === target.vmId);
      contextBar.hidden = false;
      contextBar.dataset.status = target.status;
      ctxStatus.dataset.status = target.status;
      ctxStatusText.textContent = targetStatusLabel(target);
      // Connected is the normal case: the green dot on the tab says it, the chip only speaks up otherwise.
      ctxStatus.hidden = target.status === "ready";
      ctxSystem.hidden = !vm?.system_label;
      ctxSep.hidden = !vm?.system_label;
      ctxSystem.textContent = vm?.system_label || "";
      ctxSystem.setAttribute("title", vm?.system_id ? `系统：${vm.system_label}（${vm.system_id}）` : "系统");
      ctxId.textContent = `vm-${target.vmId.slice(0, 8)}`;
      ctxId.setAttribute("title", `虚拟机 ID：${target.vmId}`);
      setBusy(reconnectButton, target.status === "opening");
    }

    function updateBanner(state) {
      const info = state.banner;
      banner.hidden = !info;
      if (!info) return;
      const kind = info.kind === "info" ? "info" : "error";
      if (banner.dataset.kind !== kind) bannerIcon.replaceChildren(icon(kind === "info" ? "info" : "alert"));
      banner.dataset.kind = kind;
      banner.setAttribute("role", info.kind === "info" ? "status" : "alert");
      setProse(bannerText, info.message);
      bannerAction.hidden = info.action !== "logout";
      if (info.action === "logout") {
        bannerAction.textContent = "重试退出";
        bannerAction.onclick = () => portal.logout();
      }
    }

    // The workspace's phone UI (served to phones, see ui/src/mobile-cli/web-shell.mjs) says so once it has
    // loaded; on a phone-sized window the portal then steps back and the workspace fills the screen.
    // Back (the system button or swipe) leaves the workspace for the list once the workspace has no
    // screen of its own left to go back to.
    let phoneFrame = null;
    let lastReentry = 0;
    function phoneMessage(frame, { type, url }) {
      if (type === "cage:phone-ui") {
        if (phoneFrame === frame) return true;
        phoneFrame = frame;
        el.dataset.phoneUi = "true";
        if (!win.history.state?.cagePortal) win.history.pushState({ ...win.history.state, cagePortal: "workspace" }, "");
        return true;
      }
      if (type === "cage:portal-home") {
        if (win.history.state?.cagePortal) win.history.back();
        else portal.goHome();
        return true;
      }
      if (type === "cage:workspace-unauthenticated") {
        // The workspace's sign-in lapsed. Check the portal's own; when it holds, enter the workspace again.
        if (Date.now() - lastReentry < 10000) return true;
        lastReentry = Date.now();
        void portal.checkSession().then(() => {
          if (portal.getState().phase === "signed-in" && mountedFrame === frame) portal.reconnect();
        });
        return true;
      }
      if (type === "cage:open-link") {
        // Links from the workspace open outside the sandboxed frame, http(s) only, with no opener or referrer.
        let link = null;
        try { link = new URL(String(url)); } catch { /* not a URL */ }
        if (link && (link.protocol === "https:" || link.protocol === "http:") && !link.username && !link.password) {
          win.open(link.href, "_blank", "noopener,noreferrer");
        }
        return true;
      }
      return false;
    }
    win.addEventListener("popstate", () => {
      if (phoneFrame && !win.history.state?.cagePortal && portal.getState().target) portal.goHome();
    }, { signal: shellListeners.signal });

    function unmountFrame() {
      if (!mountedFrame) return;
      if (phoneFrame === mountedFrame) {
        phoneFrame = null;
        delete el.dataset.phoneUi;
      }
      mountedFrame.listeners.abort(); // 释放 load/error 回调
      mountedFrame.el.remove(); // 移除即销毁浏览上下文，断开其中的流和 WebSocket
      mountedFrame = null;
    }

    function updateFrame(state) {
      const target = state.target;
      const frame = target?.frame;
      if (!frame) return unmountFrame();
      if (mountedFrame && mountedFrame.key === frame.key) {
        mountedFrame.el.classList.toggle("is-loading", !frame.loaded);
        return;
      }
      unmountFrame();
      // 挂载前再次校验地址（纵深防御）。
      const checked = validateWorkspaceUrl(frame.src, target.vmId, win.location);
      if (!checked.ok) {
        const key = frame.key;
        queueMicrotask(() => portal.frameFailed(key, checked.message));
        return;
      }
      const vm = state.vms.find((item) => item.id === target.vmId);
      const iframe = doc.createElement("iframe");
      iframe.setAttribute("sandbox", IFRAME_SANDBOX);
      iframe.setAttribute("referrerpolicy", IFRAME_REFERRER_POLICY);
      iframe.setAttribute("allow", iframeAllow(checked.origin));
      iframe.setAttribute("title", `工作台：${displayName(vm)}`);
      iframe.className = "workspace-frame is-loading";
      const listeners = new AbortController();
      const key = frame.key;
      iframe.addEventListener("load", () => portal.frameLoaded(key), { signal: listeners.signal });
      iframe.addEventListener("error", () => portal.frameFailed(key), { signal: listeners.signal });
      iframe.setAttribute("src", checked.url);
      mountedFrame = { key, el: iframe, listeners, origin: checked.origin };
      frameHost.append(iframe);
    }

    function overlaySignature(state) {
      const target = state.target;
      if (!target) {
        const list = state.vms.map((vm) => `${vm.id}:${vm.state}:${vm.name}:${vm.system_label}:${vm.message || ""}:${vm.message_detail || ""}`).join(",");
        return `home|${state.listStatus}|${state.limit}|${state.listError?.message || ""}|${state.listError?.detail || ""}|${state.user?.username || ""}|${list}`;
      }
      if (target.status === "ready") return `ready|${target.vmId}`;
      const vm = state.vms.find((item) => item.id === target.vmId);
      return `t|${target.vmId}|${target.status}|${target.warm ? 1 : 0}|${target.frame?.slow ? 1 : 0}|${target.pollFailures}|${target.error?.code || ""}|${target.error?.message || ""}|${target.error?.detail || ""}|${vm?.message_detail || ""}|${vm?.name || ""}|${vm?.system_label || ""}`;
    }

    function button(label, { kind = "", focusKey = null, autofocus = false, onclick, iconName = null } = {}) {
      return h("button", {
        type: "button",
        class: `btn ${kind}`.trim(),
        "data-focus-key": focusKey,
        "data-autofocus": autofocus,
        onclick,
      }, [iconName ? icon(iconName) : null, label]);
    }

    // A pause moment: drawing, serif heading, a line or two, actions.
    function hero({ art, title, lines = [], actions = [], role = null }) {
      return h("section", { class: "hero", role }, [
        drawing(art, "art hero-art"),
        // Titles keep a quoted 「name」 together with what follows it rather than splitting the name.
        setProse(h("h2", { class: "hero-title" }), title, /[^「]+|「[^」]*」[^「]*/g),
        ...lines,
        actions.length ? h("div", { class: "actions" }, actions) : null,
      ]);
    }

    const text = (value, className = "hero-text") => setProse(h("p", { class: className }), value);
    // The server's untranslated words, if any: small, muted, plain text.
    const detailLine = (value) => (value ? h("p", { class: "hero-detail", text: value }) : null);

    function steps(current, warm) {
      const labels = ["连接", warm ? "确认" : "开机", "载入工作台"];
      return h("ol", { class: "steps", "aria-hidden": "true" }, labels.map((label, index) =>
        h("li", { "data-step": index < current ? "done" : index === current ? "current" : "todo", text: label }),
      ));
    }

    function buildOverlay(state) {
      const target = state.target;
      if (!target) return buildHome(state);
      const vm = state.vms.find((item) => item.id === target.vmId);
      // Headings quote the name; a very long one is clipped there (the tab title and the list keep it whole).
      const chars = Array.from(displayName(vm));
      const name = chars.length > 16 ? `${chars.slice(0, 15).join("")}…` : chars.join("");
      if (target.status === "ready") return [];
      if (target.status === "opening") {
        return [hero({ art: "terminal", title: `正在连接「${name}」`, role: "status", lines: [steps(0, target.warm), text("正在向门户确认虚拟机状态…")] })];
      }
      if (target.status === "starting") {
        const lines = [
          steps(1, target.warm),
          ...(target.warm
            ? [text("这台虚拟机此前在运行。"), text("正在确认工作台已就绪，通常几秒内完成。")]
            : [text("首次开机通常需要数十秒到几分钟。"), text("开机在服务器上进行，切换虚拟机或离开页面都不会中断。")]),
          h("p", { class: "hero-meta" }, [
            vm?.system_label ? h("span", { text: vm.system_label }) : null,
            vm?.system_label ? h("span", { class: "meta-sep", "aria-hidden": "true", text: "·" }) : null,
            h("span", { class: "elapsed", "data-since": String(target.since) }),
          ]),
        ];
        if (target.pollFailures) lines.push(text(`网络不稳定，正在重试（${target.pollFailures}/${POLL_MAX_FAILURES}）…`, "hero-text warn-text"));
        return [hero({ art: "terminal", title: target.warm ? `正在恢复「${name}」的工作台` : `「${name}」正在开机`, role: "status", lines })];
      }
      if (target.status === "connecting") {
        if (!target.frame?.slow) {
          return [h("div", { class: "veil" }, hero({ art: "terminal", title: `正在载入「${name}」的工作台`, role: "status", lines: [steps(2, target.warm), text("虚拟机已就绪，正在打开工作台。")] }))];
        }
        return [
          h("div", { class: "veil" }, hero({
            art: "terminal",
            title: "工作台载入时间较长",
            role: "alert",
            lines: [text("可以继续等待，或重新连接。")],
            actions: [
              button("重新连接", { kind: "btn-primary", focusKey: "retry", autofocus: true, onclick: () => portal.retry() }),
              button("继续等待", { focusKey: "wait", onclick: () => portal.keepWaiting() }),
            ],
          })),
        ];
      }
      const error = target.error || {};
      const failedBoot = error.code === "vm_error" || error.code === "vm_stopped";
      return [
        hero({
          art: failedBoot ? "fault" : "unplugged",
          title: failedBoot ? `「${name}」启动失败` : `无法打开「${name}」`,
          role: "alert",
          lines: [
            text(error.message || "发生未知错误。", "hero-text error-text"),
            detailLine(error.detail || (error.code === "vm_error" ? vm?.message_detail : "")),
          ],
          actions: [
            button("重试", { kind: "btn-primary", focusKey: "retry", autofocus: true, onclick: () => portal.retry() }),
            button("返回总览", { focusKey: "home", onclick: () => portal.goHome() }),
          ],
        }),
      ];
    }

    function vmRow(vm) {
      const created = formatCreated(vm.created_at);
      const meta = [];
      if (vm.system_label) meta.push(vm.system_label);
      meta.push(h("span", { class: "mono", text: `vm-${vm.id.slice(0, 8)}` }));
      if (created) meta.push(`创建于 ${created}`);
      const metaLine = h("span", { class: "vm-meta" });
      meta.forEach((part, index) => metaLine.append(...(index ? [" · ", part] : [part])));
      return h("li", {}, h("button", { type: "button", class: "vm-card", "data-state": vm.state, "data-focus-key": `card-${vm.id}`, onclick: () => portal.select(vm.id) }, [
        h("span", { class: "vm-main" }, [
          h("span", { class: "vm-name", text: displayName(vm) }),
          metaLine,
          vm.state === "error" && vm.message ? h("span", { class: "vm-error", text: vm.message }) : null,
          vm.state === "error" && vm.message_detail ? h("span", { class: "vm-error-detail", text: vm.message_detail }) : null,
        ]),
        h("span", { class: "chip", "data-state": vm.state }, [
          h("span", { class: "status-dot", "data-state": vm.state, "aria-hidden": "true" }),
          VM_STATE_LABELS[vm.state],
        ]),
        h("span", { class: "vm-go" }, [h("span", { class: "vm-go-text", text: VM_ACTION_LABELS[vm.state] || "启动并打开" }), icon("chevron")]),
      ]));
    }

    function buildHome(state) {
      if (state.listStatus === "loading" || state.listStatus === "idle") {
        return [h("div", { class: "hero-quiet", role: "status" }, [h("span", { class: "live-dot", "aria-hidden": "true" }), h("h2", { text: "正在加载虚拟机列表" })])];
      }
      if (state.listStatus === "error") {
        return [hero({
          art: "unplugged",
          title: "无法加载虚拟机列表",
          role: "alert",
          lines: [text(state.listError?.message || "请稍后重试。", "hero-text error-text"), detailLine(state.listError?.detail)],
          actions: [button("重试", { kind: "btn-primary", focusKey: "list-retry", autofocus: true, onclick: () => portal.refreshList() })],
        })];
      }
      const username = state.user?.username || "";
      if (!state.vms.length) {
        return [hero({
          art: "server",
          title: "创建你的第一台虚拟机",
          lines: [text(`欢迎，${username}。每台虚拟机有独立的磁盘和工作台，创建后会自动开机并打开。`)],
          actions: [button("新建虚拟机", { kind: "btn-clay btn-lg", focusKey: "empty-create", autofocus: true, iconName: "plus", onclick: () => portal.openCreate() })],
        })];
      }
      const full = state.limit !== null && state.vms.length >= state.limit;
      const hour = new Date().getHours();
      const daytime = hour >= 5 && hour < 18;
      return [
        h("section", { class: "home", "aria-labelledby": "home-title" }, [
          h("header", { class: "home-hero" }, [
            h("h1", { class: "greeting" }, [h("span", { text: `${greetingWord()}，${username}` }), drawing(daytime ? "sun" : "moon", "art greeting-art")]),
            drawing("squiggle", "art greeting-underline"),
            setProse(h("p", { class: "home-lead" }), overviewSummary(state.vms)),
          ]),
          h("div", { class: "list-head" }, [
            h("h2", { id: "home-title", text: "我的虚拟机" }),
            h("span", { class: "home-quota", text: state.limit !== null ? `已创建 ${state.vms.length} / ${state.limit} 台` : `已创建 ${state.vms.length} 台` }),
            button("新建虚拟机", { kind: full ? "btn-sm" : "btn-primary btn-sm", focusKey: "home-create", iconName: "plus", onclick: () => portal.openCreate() }),
          ]),
          h("ul", { class: "vm-list", role: "list" }, state.vms.map(vmRow)),
          full ? h("p", { class: "list-foot" }, [icon("info"), `已达到每个账户 ${state.limit} 台的上限。需要更多时请联系管理员。`]) : null,
        ]),
      ];
    }

    function updateOverlay(state) {
      const sig = overlaySignature(state);
      if (sig === overlaySig) return;
      overlaySig = sig;
      const active = doc.activeElement;
      const hadFocus = active && overlay.contains(active);
      const key = hadFocus ? active.dataset.focusKey : null;
      const content = buildOverlay(state);
      overlay.replaceChildren(...content);
      overlay.hidden = content.length === 0;
      stage.dataset.mode = state.target ? state.target.status : "home";
      if (hadFocus && !focusByKey(overlay, key)) {
        const fallback = overlay.querySelector("[data-autofocus]");
        (fallback || stage).focus({ preventScroll: true });
      }
      updateElapsed();
    }

    function updateElapsed() {
      const el = overlay.querySelector(".elapsed");
      if (!el) return;
      el.textContent = `已等待 ${formatElapsed(Date.now() - Number(el.dataset.since))}`;
    }

    function updateTicker(state) {
      const needs = state.target?.status === "starting";
      if (needs && ticker === null) ticker = win.setInterval(updateElapsed, 1000);
      if (!needs && ticker !== null) {
        win.clearInterval(ticker);
        ticker = null;
      }
    }

    function announce(state) {
      const target = state.target;
      const key = target ? `${target.vmId}:${target.status}:${target.warm ? 1 : 0}:${target.frame?.slow ? 1 : 0}` : "";
      if (key === liveKey) return;
      liveKey = key;
      if (!target) {
        live.textContent = "";
        return;
      }
      const name = displayName(state.vms.find((vm) => vm.id === target.vmId));
      const message = {
        opening: `正在连接${name}`,
        starting: target.warm ? `正在确认${name}的工作台` : `${name}正在开机`,
        connecting: target.frame?.slow ? `${name}的工作台载入时间较长` : `正在载入${name}的工作台`,
        ready: `${name}的工作台已打开`,
        error: `${name}：${target.error?.message || "无法打开"}`,
      }[target.status];
      live.textContent = message || "";
    }

    function update(state) {
      current = state;
      const username = state.user?.username || "";
      userLabel.textContent = username;
      avatar.textContent = Array.from(username)[0] || "";
      userChip.setAttribute("title", `当前门户账户：${username}`);
      setBusy(logoutButton, state.logout.busy);
      const logoutLabel = state.logout.busy ? "正在退出…" : `退出登录（${username}）`;
      logoutButton.setAttribute("aria-label", logoutLabel);
      logoutButton.setAttribute("title", logoutLabel);
      const full = state.limit !== null && state.vms.length >= state.limit;
      addButton.setAttribute("title", full ? `已达到 ${state.limit} 台上限` : "新建虚拟机");
      // With no VMs yet the empty overview carries the create action; a lone "+" in the bar would only repeat it.
      switcher.hidden = !state.vms.length;
      barRule.hidden = !state.vms.length;
      // On the overview the remembered VM keeps aria-selected for the tab pattern, but should not look open.
      el.dataset.view = state.target ? "vm" : "home";
      updateTabs(state);
      updateContext(state);
      updateTabFade();
      updateBanner(state);
      updateFrame(state);
      updateOverlay(state);
      updateTicker(state);
      dialog.update(state);
      announce(state);
      settleParkedFocus(state);
    }

    function settleParkedFocus(state) {
      if (!parkedFocus) return;
      if (doc.activeElement !== parkedFocus) {
        parkedFocus = null; // 用户已自行移动焦点
        return;
      }
      if (state.listStatus !== "ready" && state.listStatus !== "error") return;
      const next =
        tabEls.get(state.selectedId)?.tab ||
        overlay.querySelector("[data-autofocus]") ||
        tablist.querySelector('[role=tab][tabindex="0"]');
      parkedFocus = null;
      next?.focus({ preventScroll: true });
    }

    return {
      phase: "signed-in",
      el,
      update,
      focus() {
        const preferred = tabEls.get(current.selectedId)?.tab || overlay.querySelector("[data-autofocus]");
        const target = preferred || tablist.querySelector("[role=tab]") || home;
        target.focus({ preventScroll: true });
        // 列表仍在加载时先把焦点暂放在这里，加载完成后再交给更合适的元素。
        parkedFocus = preferred ? null : target;
      },
      destroy() {
        shellListeners.abort();
        tabResize?.disconnect();
        unmountFrame();
        if (ticker !== null) win.clearInterval(ticker);
        ticker = null;
        dialog.destroy();
      },
    };
  }

  function buildCreateDialog() {
    const ids = {
      title: uid("create-title"),
      lead: uid("create-lead"),
      name: uid("create-name"),
      nameError: uid("create-name-error"),
    };
    const heading = h("h2", { id: ids.title, class: "dialog-title", text: "新建虚拟机" });
    const input = h("input", {
      id: ids.name,
      name: "vm-name",
      type: "text",
      autocomplete: "off",
      maxlength: "256",
      placeholder: "例如：开发环境",
      "aria-describedby": ids.nameError,
    });
    const nameError = h("p", { id: ids.nameError, class: "field-error", hidden: true });
    const formError = note("form-error", "alert", "alert");
    const quota = h("p", { class: "quota" });
    const cancel = h("button", { type: "button", class: "btn", onclick: () => portal.closeCreate() }, "取消");
    const submit = h("button", { type: "submit", class: "btn btn-primary" }, "创建并打开");
    const form = h("form", { class: "dialog-form", novalidate: true }, [
      h("div", { class: "dialog-head" }, [
        heading,
        setProse(h("p", { id: ids.lead, class: "dialog-lead" }), "创建后会自动开机并打开工作台，系统由管理员预设。"),
      ]),
      h("div", { class: "field" }, [
        h("label", { for: ids.name, text: "名称" }),
        input,
        nameError,
      ]),
      formError.el,
      h("div", { class: "dialog-actions" }, [quota, h("div", { class: "actions" }, [cancel, submit])]),
    ]);
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      portal.createVm(input.value);
    });
    const el = h("dialog", { class: "dialog", "aria-labelledby": ids.title, "aria-describedby": ids.lead }, [form]);
    el.addEventListener("cancel", (event) => {
      event.preventDefault(); // 由状态驱动关闭，忙碌时保持打开
      portal.closeCreate();
    });
    el.addEventListener("close", () => {
      // close 事件是异步派发的：若期间已重新打开（新会话），这是上一次关闭的过时事件，忽略。
      if (el.open) return;
      const create = portal.getState().create;
      if (create.open && create.seq === shownSeq) portal.closeCreate();
    });

    let shownSeq = 0;
    let lastError = null;

    function show(seq) {
      const fresh = shownSeq !== seq;
      if (fresh) {
        shownSeq = seq;
        input.value = ""; // 每次打开都重新开始
        lastError = null;
      }
      if (!el.open) {
        if (typeof el.showModal === "function") el.showModal();
        else el.setAttribute("open", "");
      }
      if (fresh) input.focus();
    }

    function hide() {
      if (!el.open) return;
      if (typeof el.close === "function") el.close();
      else el.removeAttribute("open");
    }

    return {
      el,
      update(state) {
        const create = state.create;
        if (!create.open) {
          hide();
          return;
        }
        show(create.seq);
        const full = state.limit !== null && state.vms.length >= state.limit;
        quota.textContent = state.limit !== null ? `已创建 ${state.vms.length} / ${state.limit} 台${full ? "，已达上限" : ""}` : `已创建 ${state.vms.length} 台`;
        quota.dataset.full = String(full);
        setBusy(submit, create.busy || full);
        setBusy(cancel, create.busy);
        input.readOnly = create.busy;
        submit.textContent = create.busy ? "正在创建…" : "创建并打开";
        const error = create.error;
        setFieldError(input, nameError, error?.field === "name" ? error.message : "");
        formError.set(error && !error.field ? error.message : "", error && !error.field ? error.detail : "");
        if (error?.field === "name" && error !== lastError) input.focus();
        lastError = error;
      },
      destroy: hide,
    };
  }

  const SCREENS = {
    booting: buildBooting,
    "session-error": buildSessionError,
    "signed-out": buildAuth,
    "signed-in": buildShell,
  };

  function titleFor(state) {
    if (state.phase !== "signed-in") return "Super Cage 门户";
    const target = state.target;
    if (!target) return "虚拟机总览 · Super Cage";
    const vm = state.vms.find((item) => item.id === target.vmId);
    return `${displayName(vm)} · Super Cage`;
  }

  function render(state) {
    if (!screen || screen.phase !== state.phase) {
      const previous = screen;
      screen = SCREENS[state.phase]();
      root.replaceChildren(screen.el);
      root.removeAttribute("aria-busy");
      previous?.destroy?.();
      screen.update(state);
      screen.focus?.();
    } else {
      screen.update(state);
    }
    doc.title = titleFor(state);
  }

  const unsubscribe = portal.subscribe(render);
  render(portal.getState());
  return {
    destroy() {
      unsubscribe();
      screen?.destroy?.();
      screen = null;
    },
  };
}

/* ------------------------------------------------------------------ */
/* 浏览器启动                                                          */
/* ------------------------------------------------------------------ */

// The visible area above an on-screen keyboard, for the full-screen workspace and bottom sheets on phones:
// iOS pans the page instead of resizing it, so fixed layers follow the visual viewport.
function trackViewport(doc, win) {
  const viewport = win.visualViewport;
  if (!viewport) return;
  const style = doc.documentElement.style;
  const update = () => {
    if (Math.abs(viewport.scale - 1) > 0.01) return;
    style.setProperty("--vv-top", `${Math.max(0, viewport.offsetTop)}px`);
    style.setProperty("--vv-height", `${viewport.height}px`);
    style.setProperty("--vv-bottom", `${Math.max(0, win.innerHeight - viewport.offsetTop - viewport.height)}px`);
  };
  viewport.addEventListener("resize", update);
  viewport.addEventListener("scroll", update);
  update();
}

export function start(doc = globalThis.document, win = globalThis) {
  const root = doc?.getElementById("app");
  if (!root || root.dataset.mounted === "1") return null;
  root.dataset.mounted = "1";
  const local = safeStorage("localStorage");
  const theme = createThemeController(doc, local);
  theme.apply();
  // 登录或退出后通知同源的其他标签页重新读取会话（只是加速同步，授权仍以服务端为准）。
  let channel = null;
  try {
    channel = typeof win.BroadcastChannel === "function" ? new win.BroadcastChannel(SESSION_CHANNEL) : null;
  } catch {
    channel = null;
  }
  const broadcast = {
    post() {
      if (channel) channel.postMessage({ type: "session" });
      else local?.setItem(SESSION_PING_KEY, `${Date.now()}-${Math.random()}`);
    },
  };
  const portal = createPortal({
    api: createApi(),
    selection: createSelectionStore(),
    location: win.location,
    isHidden: () => doc.visibilityState === "hidden",
    broadcast,
  });
  trackViewport(doc, win);
  mountPortal(root, portal, { doc, win, theme });
  channel?.addEventListener("message", (event) => {
    if (event.data?.type === "session") void portal.checkSession();
  });
  win.addEventListener("storage", (event) => {
    if (event.key === SESSION_PING_KEY) void portal.checkSession();
  });
  doc.addEventListener("visibilitychange", () => {
    if (doc.visibilityState !== "visible") return;
    portal.visibilityChanged();
    void portal.checkSession({ force: false });
  });
  win.addEventListener("focus", () => void portal.checkSession({ force: false }));
  win.addEventListener("online", () => {
    const state = portal.getState();
    if (state.target?.status === "error" && state.target.error?.code === "network_error") portal.retry();
    else if (state.phase === "signed-in" && state.listStatus === "error") void portal.refreshList();
    else if (state.phase === "session-error") void portal.boot();
  });
  win.addEventListener("pageshow", (event) => {
    if (event.persisted) void portal.boot();
  });
  void portal.boot();
  return portal;
}

if (typeof document !== "undefined") start();
