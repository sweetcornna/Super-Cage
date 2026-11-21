// 仅供测试和本地预览的独立 mock 服务，不是生产路径，也不被 app.mjs 引用。
// 它按 docs/api-contract.md 实现门户 API 与 VM 源的票据入口，认证不可绕过：
// 会话 Cookie、精确 Origin、JSON Content-Type、票据单次消费/60 秒/绑定会话与 VM 均会校验；
// 带 X-Cage-Expected-User 且与 Cookie 账户不一致时返回 409 session_changed（不返回数据、不写状态、不撤销会话）。
// 只监听 127.0.0.1。手动预览：node ui/tenant/mock-server.mjs --port 8787
// 然后在浏览器打开 http://cage.localhost:8787/

import http from "node:http";
import { createHash, randomBytes, scryptSync, timingSafeEqual } from "node:crypto";
import { readFile } from "node:fs/promises";
import { gzipSync } from "node:zlib";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { createPhoneFixture } from "./phone-fixture.mjs";
import { createDesktopFixture } from "./desktop-fixture.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const STATIC = {
  "/": ["index.html", "text/html; charset=utf-8"],
  "/index.html": ["index.html", "text/html; charset=utf-8"],
  "/app.mjs": ["app.mjs", "text/javascript; charset=utf-8"],
  "/styles.css": ["styles.css", "text/css; charset=utf-8"],
};
// The gateway serves these from its embedded copy (internal/tenant/portal_icons.go).
const ICONS = join(HERE, "../../internal/tenant/portal-icons");
const PORTAL_ICONS = {
  "/favicon.ico": "image/x-icon", "/icon.svg": "image/svg+xml", "/apple-touch-icon.png": "image/png",
  "/icon-192.png": "image/png", "/icon-512.png": "image/png", "/icon-maskable-512.png": "image/png",
};
const PORTAL_MANIFEST = JSON.stringify({
  name: "Super Cage", short_name: "Super Cage", lang: "zh-CN", start_url: "/", scope: "/", display: "standalone",
  background_color: "#faf9f5", theme_color: "#faf9f5",
  icons: [
    { src: "/icon-192.png", sizes: "192x192", type: "image/png", purpose: "any" },
    { src: "/icon-512.png", sizes: "512x512", type: "image/png", purpose: "any" },
    { src: "/icon-maskable-512.png", sizes: "512x512", type: "image/png", purpose: "maskable" },
  ],
});
const USERNAME = /^[a-z0-9][a-z0-9._-]{2,31}$/;
const SYSTEM_ID = /^[a-z0-9-]{1,32}$/;
const SESSION_COOKIE = "cage_tenant_session";
const VM_COOKIE = "cage_tenant_vm";

export const DEFAULT_SYSTEMS = Object.freeze([
  Object.freeze({ id: "debian-12", label: "Debian 12" }),
  Object.freeze({ id: "ubuntu-24-04", label: "Ubuntu 24.04 LTS" }),
]);

const ENTER_HTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Super Cage 工作台入口</title><script defer src="/_tenant/enter.js"></script></head>
<body><p id="status">正在进入工作台…</p></body></html>`;

const ENTER_JS = `(async () => {
  const ticket = location.hash.slice(1);
  history.replaceState(null, "", location.pathname);
  const status = document.getElementById("status");
  if (!ticket) { status.textContent = "入口无效，请回到门户重新连接。"; return; }
  try {
    const response = await fetch("/_tenant/enter", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ticket }),
      credentials: "same-origin",
      cache: "no-store",
    });
    if (response.status === 204) { location.replace("/"); return; }
    status.textContent = "入口已失效，请回到门户重新连接。";
  } catch {
    status.textContent = "连接失败，请回到门户重新连接。";
  }
})();`;

const WORKBENCH_JS = `(() => {
  const source = new EventSource("/_mock/stream");
  source.onmessage = (event) => { document.getElementById("beat").textContent = event.data; };
})();`;

function hashPassword(password, salt) {
  return scryptSync(password, salt, 32, { N: 1024, r: 8, p: 1 });
}

function parseCookies(header = "") {
  const cookies = {};
  for (const part of header.split(";")) {
    const index = part.indexOf("=");
    if (index < 0) continue;
    cookies[part.slice(0, index).trim()] = part.slice(index + 1).trim();
  }
  return cookies;
}

function token(bytes = 32) {
  return randomBytes(bytes).toString("base64url");
}

function escapeHtml(value) {
  return String(value).replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);
}

export async function startMockServer({
  port = 0,
  portalName = "cage.localhost",
  bootDelayMs = 1500,
  vmLimit = 5,
  globalLimit = 20,
  systems = DEFAULT_SYSTEMS,
  ticketTtlMs = 60_000,
  registrationOpen = true,
  uiDir = HERE,
  workbenchDir,
  // Answer the phone UI's /api/mobile/* routes from phone-fixture.mjs and serve
  // mobile.html at "/" when the browser chose the phone layout (cookie only:
  // the user-agent rule belongs to the gateway and is tested in Go).
  phoneFixture = false,
} = {}) {
  const phoneApi = phoneFixture ? createPhoneFixture() : null;
  const desktopApi = phoneFixture ? createDesktopFixture() : null;
  const state = {
    users: new Map(), // username -> { id, username, salt, hash }
    sessions: new Map(), // token -> { id, userId, revoked }
    vms: new Map(), // id -> record
    tickets: new Map(), // ticket -> { vmId, sessionId, expires, used }
    vmCookies: new Map(), // value -> { vmId, sessionId }
    streams: new Set(), // { vmId, sessionId, res }
    systems: systems.map((system) => ({ ...system })),
    requests: [], // { method, host, path, origin, referer }
    issuedTickets: [], // 仅供测试断言票据从未出现在路径或 Referer 中
    entered: [], // { vmId, userId }
    served: [], // { vmId, userId }
    faults: [], // { method, path, status, code, message }
    bootFailures: new Map(), // vmId 或 "*" -> message
    openDelayMs: 0,
    registrationOpen,
  };
  const timers = new Set();
  const server = http.createServer((req, res) => {
    handle(req, res).catch((error) => {
      if (!res.headersSent) problem(res, 500, "server_error", `mock 内部错误：${error.message}`);
      else res.destroy();
    });
  });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(port, "127.0.0.1", resolve);
  });
  const actualPort = server.address().port;
  const portalHost = `${portalName}:${actualPort}`;
  const origin = `http://${portalHost}`;
  const vmHost = (id) => `vm-${id}.${portalHost}`;
  const vmOrigin = (id) => `http://${vmHost(id)}`;

  function later(fn, ms) {
    const id = setTimeout(() => {
      timers.delete(id);
      fn();
    }, ms);
    timers.add(id);
    return id;
  }

  function send(res, status, body, type = "application/json; charset=utf-8", headers = {}) {
    res.writeHead(status, { "Content-Type": type, ...headers });
    res.end(body);
  }

  function json(res, status, body, headers = {}) {
    if (status === 204) {
      res.writeHead(204, headers);
      return res.end();
    }
    send(res, status, JSON.stringify(body), "application/json; charset=utf-8", headers);
  }

  function problem(res, status, code, message, headers = {}) {
    json(res, status, { error: { code, message } }, headers);
  }

  async function readJson(req) {
    const chunks = [];
    let size = 0;
    for await (const chunk of req) {
      size += chunk.length;
      if (size > 65536) throw Object.assign(new Error("too large"), { code: "too_large" });
      chunks.push(chunk);
    }
    const text = Buffer.concat(chunks).toString("utf8");
    return text ? JSON.parse(text) : {};
  }

  const isJson = (req) => /^application\/json(\s*;|$)/i.test(req.headers["content-type"] || "");

  function currentSession(req) {
    const value = parseCookies(req.headers.cookie)[SESSION_COOKIE];
    const session = value && state.sessions.get(value);
    if (!session || session.revoked) return null;
    const user = [...state.users.values()].find((item) => item.id === session.userId);
    return user ? { session, user } : null;
  }

  function startSession(res, user) {
    const value = token();
    state.sessions.set(value, { id: value, userId: user.id, revoked: false });
    res.setHeader("Set-Cookie", `${SESSION_COOKIE}=${value}; Path=/; HttpOnly; SameSite=Strict`);
  }

  function revokeSession(session) {
    session.revoked = true;
    for (const [value, ticket] of state.tickets) if (ticket.sessionId === session.id) state.tickets.delete(value);
    for (const [value, cookie] of state.vmCookies) if (cookie.sessionId === session.id) state.vmCookies.delete(value);
    for (const stream of state.streams) {
      if (stream.sessionId === session.id) {
        state.streams.delete(stream);
        stream.res.end();
      }
    }
  }

  const publicUser = (user) => ({ id: user.id, username: user.username });
  const publicVm = (vm) => {
    const out = {
      id: vm.id,
      name: vm.name,
      system_id: vm.system_id,
      system_label: vm.system_label,
      state: vm.state,
      created_at: vm.created_at,
    };
    if (vm.state === "error" && vm.message) out.message = vm.message;
    return out;
  };
  const userVms = (user) =>
    [...state.vms.values()]
      .filter((vm) => vm.owner === user.id)
      .sort((a, b) => a.created_at.localeCompare(b.created_at) || a.seq - b.seq);

  function takeFault(method, path) {
    const index = state.faults.findIndex((fault) => fault.method === method && (fault.path === path || fault.path === "*"));
    if (index < 0) return null;
    return state.faults.splice(index, 1)[0];
  }

  function boot(vm) {
    vm.state = "starting";
    delete vm.message;
    later(() => {
      if (vm.state !== "starting") return;
      const failure = state.bootFailures.get(vm.id) ?? state.bootFailures.get("*");
      if (failure !== undefined) {
        state.bootFailures.delete(vm.id);
        state.bootFailures.delete("*");
        vm.state = "error";
        vm.message = failure;
      } else {
        vm.state = "ready";
      }
    }, bootDelayMs);
  }

  async function handle(req, res) {
    const host = String(req.headers.host || "").toLowerCase();
    const url = new URL(req.url, "http://mock.invalid");
    state.requests.push({ method: req.method, host, path: req.url, origin: req.headers.origin || null, referer: req.headers.referer || null });
    res.setHeader("Cache-Control", "no-store");
    res.setHeader("X-Content-Type-Options", "nosniff");
    res.setHeader("Referrer-Policy", "no-referrer");
    if (host === portalHost) return portal(req, res, url);
    const match = /^vm-([0-9a-f]{32})\./.exec(host);
    if (match && host === vmHost(match[1])) return workbench(req, res, url, match[1]);
    return problem(res, 403, "origin_refused", "请求主机不受支持。");
  }

  async function portal(req, res, url) {
    res.setHeader("X-Frame-Options", "DENY");
    res.setHeader(
      "Content-Security-Policy",
      `default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; manifest-src 'self'; frame-src http://*.${portalHost}; base-uri 'none'; form-action 'self'; frame-ancestors 'none'`,
    );
    if ((req.method === "GET" || req.method === "HEAD") && url.pathname === "/manifest.webmanifest") return send(res, 200, req.method === "HEAD" ? "" : PORTAL_MANIFEST, "application/manifest+json");
    if ((req.method === "GET" || req.method === "HEAD") && PORTAL_ICONS[url.pathname]) {
      const body = await readFile(join(ICONS, url.pathname.slice(1))).catch(() => null);
      if (body) return send(res, 200, req.method === "HEAD" ? "" : body, PORTAL_ICONS[url.pathname]);
    }
    if ((req.method === "GET" || req.method === "HEAD") && STATIC[url.pathname]) {
      const [name, type] = STATIC[url.pathname];
      const body = await readFile(join(uiDir, name));
      // As the gateway does (internal/tenant/portal_static.go): gzip when accepted, never cached.
      if (/\bgzip\b/.test(String(req.headers["accept-encoding"] || ""))) {
        const packed = gzipSync(body, { level: 9 });
        return send(res, 200, req.method === "HEAD" ? "" : packed, type, { "Content-Encoding": "gzip", Vary: "Accept-Encoding" });
      }
      return send(res, 200, req.method === "HEAD" ? "" : body, type, { Vary: "Accept-Encoding" });
    }
    if (!url.pathname.startsWith("/api/tenant/")) return problem(res, 404, "not_found", "未找到。");
    let body = {};
    if (req.method === "POST") {
      if (req.headers.origin !== origin) return problem(res, 403, "origin_refused", "请求来源无效。");
      if (!isJson(req)) return problem(res, 415, "invalid_content_type", "请求必须使用 application/json。");
      try {
        body = await readJson(req);
      } catch {
        return problem(res, 400, "invalid_input", "请求内容不是有效的 JSON。");
      }
      if (!body || typeof body !== "object" || Array.isArray(body)) return problem(res, 400, "invalid_input", "请求内容无效。");
    }
    const fault = takeFault(req.method, url.pathname);
    if (fault) return problem(res, fault.status, fault.code, fault.message);
    const route = `${req.method} ${url.pathname}`;
    if (route === "POST /api/tenant/register" || route === "POST /api/tenant/login") {
      const register = route.endsWith("register");
      const username = typeof body.username === "string" ? body.username.trim().toLowerCase() : "";
      const password = typeof body.password === "string" ? body.password : "";
      if (register) {
        if (!state.registrationOpen) return problem(res, 403, "registration_closed", "此门户未开放自助注册，请联系管理员。");
        if (!USERNAME.test(username)) return problem(res, 400, "invalid_input", "用户名需为 3–32 个字符，以字母或数字开头，可包含 . _ -。");
        if (Array.from(password).length < 12 || Buffer.byteLength(password) > 128) {
          return problem(res, 400, "invalid_input", "密码需至少 12 个字符且不超过 128 字节。");
        }
        if (state.users.has(username)) return problem(res, 409, "username_taken", "该用户名已被使用。");
        const salt = randomBytes(16);
        const user = { id: token(12), username, salt, hash: hashPassword(password, salt) };
        state.users.set(username, user);
        startSession(res, user);
        return json(res, 201, { user: publicUser(user) });
      }
      const user = state.users.get(username);
      const salt = user?.salt || randomBytes(16);
      const hash = hashPassword(password, salt);
      if (!user || !timingSafeEqual(hash, user.hash)) return problem(res, 401, "invalid_credentials", "用户名或密码不正确。");
      startSession(res, user);
      return json(res, 200, { user: publicUser(user) });
    }
    const auth = currentSession(req);
    if (!auth) return problem(res, 401, "unauthenticated", "请先登录。");
    if (route === "GET /api/tenant/session") return json(res, 200, { user: publicUser(auth.user) });
    const expectedUser = req.headers["x-cage-expected-user"];
    if (expectedUser !== undefined && expectedUser !== auth.user.id) {
      return problem(res, 409, "session_changed", "登录账户已变化，请重新读取会话。");
    }
    if (route === "POST /api/tenant/logout") {
      revokeSession(auth.session);
      return json(res, 204, null);
    }
    if (route === "GET /api/tenant/systems") return json(res, 200, { systems: state.systems.map((s) => ({ ...s })) });
    if (route === "GET /api/tenant/vms") return json(res, 200, { vms: userVms(auth.user).map(publicVm), limit: vmLimit });
    if (route === "POST /api/tenant/vms") return createVM(res, body, auth.user.id);
    const openMatch = /^\/api\/tenant\/vms\/([^/]+)\/open$/.exec(url.pathname);
    if (req.method === "POST" && openMatch) {
      const vm = state.vms.get(openMatch[1]);
      if (!vm || vm.owner !== auth.user.id) return problem(res, 404, "vm_not_found", "虚拟机不存在。");
      if (state.openDelayMs) await new Promise((resolve) => later(resolve, state.openDelayMs));
      if (vm.state === "ready") {
        const ticket = token();
        state.tickets.set(ticket, { vmId: vm.id, sessionId: auth.session.id, expires: Date.now() + ticketTtlMs, used: false });
        state.issuedTickets.push(ticket);
        return json(res, 200, { vm: publicVm(vm), workspace_url: `${vmOrigin(vm.id)}/_tenant/enter#${ticket}` });
      }
      if (vm.state === "stopped" || vm.state === "error") boot(vm);
      return json(res, 202, { vm: publicVm(vm) });
    }
    return problem(res, 404, "not_found", "未找到。");
  }

  function createVM(res, body, owner) {
    const name = typeof body.name === "string" ? body.name.trim() : "";
    const length = Array.from(name).length;
    if (!length || length > 64 || /\p{Cc}/u.test(name)) return problem(res, 400, "invalid_input", "名称需为 1–64 个字符且不含控制字符。");
    const system = body.system_id === undefined ? state.systems[0]
      : typeof body.system_id === "string" && SYSTEM_ID.test(body.system_id)
        ? state.systems.find((s) => s.id === body.system_id) : null;
    if (!system) return problem(res, 400, "invalid_input", "暂无可安装的系统，请联系管理员。");
    if (userVms({ id: owner }).length >= vmLimit) return problem(res, 409, "vm_limit", `每个账户最多 ${vmLimit} 台虚拟机。`);
    if (state.vms.size >= globalLimit) return problem(res, 409, "vm_limit", "服务器虚拟机总数已达上限。");
    const vm = {
      id: randomBytes(16).toString("hex"), owner, name,
      system_id: system.id, system_label: system.label, state: "stopped",
      created_at: new Date().toISOString(), seq: state.vms.size,
    };
    state.vms.set(vm.id, vm);
    return json(res, 201, { vm: publicVm(vm) });
  }

  async function workbenchVMs(req, res, pathname, id, auth) {
    const endpoint = "/api/client/virtual-machines";
    if (pathname === endpoint && req.method === "GET") {
      return json(res, 200, { vms: userVms({ id: auth.session.userId }).map(publicVm), current_id: id });
    }
    if (req.method !== "POST") return problem(res, 404, "not_found", "未找到。");
    if (req.headers.origin !== vmOrigin(id)) return problem(res, 403, "origin_refused", "请求来源无效。");
    if (!isJson(req)) return problem(res, 415, "invalid_content_type", "请求必须使用 application/json。");
    const body = await readJson(req).catch(() => null);
    if (!body || typeof body !== "object" || Array.isArray(body)) return problem(res, 400, "invalid_input", "请求内容无效。");
    if (pathname === endpoint && Object.keys(body).length === 1) return createVM(res, { name: body.name }, auth.session.userId);
    const target = /^\/api\/client\/virtual-machines\/([0-9a-f]{32})\/open$/.exec(pathname)?.[1];
    if (target && Object.keys(body).length === 0) {
      const vm = state.vms.get(target);
      if (!vm || vm.owner !== auth.session.userId) return problem(res, 404, "vm_not_found", "虚拟机不存在。");
      if (vm.state === "stopped" || vm.state === "error") boot(vm);
      return json(res, 200, { vm_id: target, vm: publicVm(vm) });
    }
    return problem(res, 400, "invalid_input", "请求内容无效。");
  }

  function vmAuth(req, id) {
    const value = parseCookies(req.headers.cookie)[VM_COOKIE];
    const cookie = value && state.vmCookies.get(value);
    if (!cookie || cookie.vmId !== id) return null;
    const session = state.sessions.get(cookie.sessionId);
    const vm = state.vms.get(id);
    if (!session || session.revoked || !vm || vm.owner !== session.userId) return null;
    return { session, vm };
  }

  async function workbench(req, res, url, id) {
    const ancestors = `frame-ancestors ${origin}`;
    if (url.pathname === "/_tenant/enter" && req.method === "GET") {
      res.setHeader("Content-Security-Policy", `default-src 'none'; script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; ${ancestors}`);
      return send(res, 200, ENTER_HTML, "text/html; charset=utf-8");
    }
    if (url.pathname === "/_tenant/enter.js" && req.method === "GET") {
      return send(res, 200, ENTER_JS, "text/javascript; charset=utf-8");
    }
    if (url.pathname === "/_tenant/enter" && req.method === "POST") {
      if (req.headers.origin !== vmOrigin(id)) return problem(res, 403, "origin_refused", "请求来源无效。");
      if (!isJson(req)) return problem(res, 415, "invalid_content_type", "请求必须使用 application/json。");
      let body;
      try {
        body = await readJson(req);
      } catch {
        return problem(res, 400, "invalid_input", "请求内容无效。");
      }
      const ticket = typeof body?.ticket === "string" ? state.tickets.get(body.ticket) : null;
      const session = ticket && state.sessions.get(ticket.sessionId);
      if (!ticket || ticket.used || ticket.vmId !== id || ticket.expires < Date.now() || !session || session.revoked) {
        return problem(res, 401, "unauthenticated", "入口已失效。");
      }
      ticket.used = true;
      state.tickets.delete(body.ticket);
      const value = token();
      state.vmCookies.set(value, { vmId: id, sessionId: session.id });
      state.entered.push({ vmId: id, userId: session.userId });
      return json(res, 204, null, { "Set-Cookie": `${VM_COOKIE}=${value}; Path=/; HttpOnly; SameSite=Strict` });
    }
    const auth = vmAuth(req, id);
    if (!auth) return problem(res, 401, "unauthenticated", "需要从门户重新进入工作台。");
    if (url.pathname.startsWith("/api/client/virtual-machines")) return workbenchVMs(req, res, url.pathname, id, auth);
    if (phoneApi && url.pathname.startsWith("/api/mobile/")) {
      const input = req.method === "POST" ? await readJson(req).catch(() => ({})) : undefined;
      const reply = phoneApi(req.method, url.pathname, input, url.search);
      if (reply.status !== 200) return send(res, reply.status, reply.body, "text/plain; charset=utf-8");
      return json(res, 200, reply.body);
    }
    if (workbenchDir && req.method === "GET") {
      if (url.pathname === "/api/client/context") return json(res, 200, {
        mode: "local", connection_id: "local", server_platform: "linux",
        tenant_workspace: true, virtual_machines: true, portal_origin: origin,
      });
      if (url.pathname === "/api/account") return json(res, 200, { exists: true, unlocked: true, username: "fixture" });
      if (url.pathname === "/api/environment/initialization") return json(res, 200, { required: false });
      const desktopReply = desktopApi?.(req.method, url.pathname);
      if (desktopReply) return json(res, 200, desktopReply);
      if (url.pathname === "/api/status") return json(res, 200, { job: { running: false }, ready: false, projects: [] });
      if (url.pathname === "/api/browser/tabs") return json(res, 200, { tabs: [] });
      if (url.pathname.startsWith("/api/")) return json(res, 200, {});

      // The gateway's rule (internal/webentry): the layout cookie, else the browser's claim to be a phone.
      const layout = parseCookies(req.headers.cookie)["cage-layout"];
      const agent = String(req.headers["user-agent"] || "");
      const hint = String(req.headers["sec-ch-ua-mobile"] || "");
      const phoneAgent = hint === "?1" || (hint !== "?0" && !agent.includes("iPad") &&
        (/iPhone|iPod/.test(agent) || (agent.includes("Android") ? agent.includes("Mobile") : agent.includes("Mobi"))));
      const phone = layout === "phone" || layout === "phone-auto" || (layout !== "desktop" && phoneAgent);
      const root = phoneApi && phone ? "mobile.html" : "index.html";
      const asset = url.pathname === "/" ? root : url.pathname.slice(1);
      if (/^(?:assets\/[\w.-]+|cage\.(?:png|svg)|(?:index|mobile)\.html|favicon\.ico|apple-touch-icon\.png|manifest\.webmanifest|entry-guard\.js|icon-[\w-]+\.png)$/.test(asset)) {
        const types = { html: "text/html", js: "text/javascript", css: "text/css", png: "image/png", svg: "image/svg+xml", woff2: "font/woff2", ico: "image/x-icon", webmanifest: "application/manifest+json" };
        const type = types[asset.split(".").at(-1)];
        if (type) {
          const file = await readFile(join(workbenchDir, asset)).catch(() => null);
          if (!file) return problem(res, 404, "not_found", "未找到。");
          res.setHeader("Content-Security-Policy", `default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data: blob:; base-uri 'none'; form-action 'self'; ${ancestors}`);
          if (asset === "index.html" || url.pathname === "/") state.served.push({ vmId: id, userId: auth.session.userId, document: asset });
          // Cache and encode as the workbench does (internal/workbench/static_assets.go, through the
          // gateway): hashed assets are immutable, the rest revalidates by ETag; .br companions
          // are served when accepted. Keeps load measurements against this mock realistic.
          const hashed = /^assets\/(?:[^/]+\/)*[^/]+-[A-Za-z0-9_-]{8}(?:\.[A-Za-z0-9]+)+$/.test(asset);
          const etag = `"${createHash("sha256").update(file).digest("hex").slice(0, 32)}"`;
          res.setHeader("Cache-Control", hashed ? "private, max-age=31536000, immutable" : "no-cache");
          res.setHeader("ETag", etag);
          if (url.pathname === "/") res.setHeader("Vary", "Cookie, User-Agent, Sec-CH-UA-Mobile");
          if (req.headers["if-none-match"] === etag) { res.writeHead(304); return res.end(); }
          const brotli = /\bbr\b/.test(String(req.headers["accept-encoding"] || "")) ? await readFile(join(workbenchDir, asset + ".br")).catch(() => null) : null;
          if (brotli) {
            res.setHeader("Vary", [res.getHeader("Vary"), "Accept-Encoding"].filter(Boolean).join(", "));
            return send(res, 200, brotli, type, { "Content-Encoding": "br" });
          }
          return send(res, 200, file, type);
        }
      }
    }
    if (url.pathname === "/" && req.method === "GET") {
      state.served.push({ vmId: id, userId: auth.session.userId });
      res.setHeader("Content-Security-Policy", `default-src 'none'; script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; ${ancestors}`);
      return send(
        res,
        200,
        `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>Mock 工作台</title><script defer src="/_mock/workbench.js"></script></head>` +
          `<body><h1 id="vm" data-vm-id="${id}">Mock 工作台：${escapeHtml(auth.vm.name)}</h1><p>${escapeHtml(auth.vm.system_label)} · vm-${id}</p><p id="beat"></p></body></html>`,
        "text/html; charset=utf-8",
      );
    }
    if (url.pathname === "/_mock/workbench.js" && req.method === "GET") {
      return send(res, 200, WORKBENCH_JS, "text/javascript; charset=utf-8");
    }
    if (url.pathname === "/_mock/stream" && req.method === "GET") {
      res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-store" });
      const stream = { vmId: id, sessionId: auth.session.id, res };
      state.streams.add(stream);
      res.write(`data: connected ${id}\n\n`);
      const beat = setInterval(() => res.write(`data: ${Date.now()}\n\n`), 1000);
      req.on("close", () => {
        clearInterval(beat);
        state.streams.delete(stream);
      });
      return;
    }
    return problem(res, 404, "not_found", "未找到。");
  }

  return {
    origin,
    port: actualPort,
    portalName,
    portalHost,
    vmOrigin,
    vmHost,
    state,
    /** 让下一次匹配的请求返回指定错误。 */
    failNext(method, path, status, code, message) {
      state.faults.push({ method, path, status, code, message });
    },
    /** 让下一次开机（指定 VM 或任意 VM）失败。 */
    failNextBoot(vmId = "*", message = "镜像暂不可用，请稍后重试。") {
      state.bootFailures.set(vmId, message);
    },
    setRegistrationOpen(open) {
      state.registrationOpen = Boolean(open);
    },
    setSystems(list) {
      state.systems = list.map((system) => ({ ...system }));
    },
    openStreams(vmId) {
      return [...state.streams].filter((stream) => !vmId || stream.vmId === vmId).length;
    },
    async close() {
      for (const id of timers) clearTimeout(id);
      timers.clear();
      for (const stream of state.streams) stream.res.destroy();
      state.streams.clear();
      server.closeAllConnections?.();
      await new Promise((resolve) => server.close(resolve));
    },
  };
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const args = process.argv.slice(2);
  const option = (name, fallback) => {
    const index = args.indexOf(name);
    return index >= 0 && args[index + 1] ? args[index + 1] : fallback;
  };
  const mock = await startMockServer({
    port: Number(option("--port", "8787")),
    bootDelayMs: Number(option("--boot-ms", "4000")),
    systems: args.includes("--no-systems") ? [] : DEFAULT_SYSTEMS,
    registrationOpen: !args.includes("--registration-closed"),
  });
  console.log(`Super Cage 多租户门户 mock（仅测试用）：${mock.origin}/`);
  const stop = async () => {
    await mock.close();
    process.exit(0);
  };
  process.on("SIGINT", stop);
  process.on("SIGTERM", stop);
}
