// 门户纯逻辑与控制器测试：注入假 fetch 与可控计时器，不依赖网络和浏览器。
import { describe, test } from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import * as app from "./app.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const ID_A = "a".repeat(32);
const ID_B = "b".repeat(32);
const ID_C = "0123456789abcdef0123456789abcdef";
const USER = { id: "u-1001", username: "alice" };
const USER2 = { id: "u-2002", username: "bob" };
const HEADER = "X-Cage-Expected-User";
const PORTAL = { protocol: "https:", hostname: "cage.example.org", port: "", origin: "https://cage.example.org" };
const DEV = { protocol: "http:", hostname: "cage.localhost", port: "8787", origin: "http://cage.localhost:8787" };
const ALLOWED_ENDPOINTS = new Set([
  "GET /api/tenant/session",
  "POST /api/tenant/login",
  "POST /api/tenant/register",
  "POST /api/tenant/logout",
  "GET /api/tenant/vms",
  "POST /api/tenant/vms",
  ...[ID_A, ID_B, ID_C].map((id) => `POST /api/tenant/vms/${id}/open`),
]);

const vm = (id, state = "stopped", extra = {}) => ({
  id,
  name: `VM-${id.slice(0, 2)}`,
  system_id: "debian-12",
  system_label: "Debian 12",
  state,
  created_at: "2026-11-18T12:00:00Z",
  ...extra,
});
const workspace = (id, ticket = "Tk_abc-123.xyz") => `https://vm-${id}.cage.example.org/_tenant/enter#${ticket}`;

async function flush() {
  for (let i = 0; i < 20; i++) await new Promise((resolve) => setImmediate(resolve));
}

function fakeFetch({ ignoreAbort = false } = {}) {
  const calls = [];
  function fetch(url, init = {}) {
    return new Promise((resolve, reject) => {
      const call = {
        url,
        init,
        method: init.method || "GET",
        body: init.body === undefined ? undefined : JSON.parse(init.body),
        done: false,
        aborted: false,
      };
      call.respond = (status, body) => {
        if (call.done && !ignoreAbort) return;
        call.done = true;
        const payload = status === 204 || body === undefined ? null : typeof body === "string" ? body : JSON.stringify(body);
        resolve(new Response(payload, { status, headers: { "Content-Type": "application/json" } }));
      };
      call.fail = (error = new TypeError("fetch failed")) => {
        if (call.done) return;
        call.done = true;
        reject(error);
      };
      const { signal } = init;
      const abort = () => {
        call.aborted = true;
        if (ignoreAbort || call.done) return;
        call.done = true;
        reject(new DOMException("aborted", "AbortError"));
      };
      if (signal?.aborted) abort();
      else signal?.addEventListener("abort", abort);
      calls.push(call);
    });
  }
  const pending = () => calls.filter((call) => !call.done);
  return {
    fetch,
    calls,
    pending,
    count: (method, url) => calls.filter((call) => call.method === method && call.url === url).length,
    next(method, url) {
      const call = calls.find((item) => !item.done && item.method === method && item.url === url);
      assert.ok(call, `应有待处理请求 ${method} ${url}；当前待处理：${pending().map((c) => `${c.method} ${c.url}`).join("，") || "无"}`);
      return call;
    },
  };
}

function fakeClock() {
  let now = 1_000_000;
  let seq = 0;
  const queue = new Map();
  return {
    timers: {
      setTimeout(fn, ms) {
        const id = ++seq;
        queue.set(id, { at: now + ms, fn });
        return id;
      },
      clearTimeout(id) {
        queue.delete(id);
      },
    },
    now: () => now,
    size: () => queue.size,
    async advance(ms) {
      await flush();
      const end = now + ms;
      for (;;) {
        let next = null;
        for (const entry of queue) if (entry[1].at <= end && (!next || entry[1].at < next[1].at)) next = entry;
        if (!next) break;
        queue.delete(next[0]);
        now = next[1].at;
        next[1].fn();
        await flush();
      }
      now = end;
      await flush();
    },
  };
}

function memoryStorage() {
  const map = new Map();
  return {
    getItem: (key) => (map.has(key) ? map.get(key) : null),
    setItem: (key, value) => map.set(key, String(value)),
    removeItem: (key) => map.delete(key),
    dump: () => Object.fromEntries(map),
  };
}

function setup({ location = PORTAL, ignoreAbort = false, local = memoryStorage(), session = memoryStorage() } = {}) {
  const f = fakeFetch({ ignoreAbort });
  const clock = fakeClock();
  const visibility = { hidden: false };
  const broadcast = { posts: 0, post() { this.posts += 1; } };
  const portal = app.createPortal({
    api: app.createApi({ fetch: f.fetch }),
    selection: app.createSelectionStore({ local, session }),
    timers: clock.timers,
    now: clock.now,
    location,
    isHidden: () => visibility.hidden,
    broadcast,
  });
  return {
    f,
    clock,
    portal,
    local,
    session,
    visibility,
    broadcast,
    get state() {
      return portal.getState();
    },
  };
}

async function signIn(ctx, { vms = [], limit = 5, user = USER } = {}) {
  const booting = ctx.portal.boot();
  ctx.f.next("GET", "/api/tenant/session").respond(200, { user });
  await flush();
  ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms, limit });
  await flush();
  await booting;
  assert.equal(ctx.state.phase, "signed-in");
}

function assertOnlyContractEndpoints(f) {
  for (const call of f.calls) assert.ok(ALLOWED_ENDPOINTS.has(`${call.method} ${call.url}`), `不应调用 ${call.method} ${call.url}`);
}

/* ------------------------------------------------------------------ */

describe("输入校验", () => {
  test("用户名转小写去空白并按契约校验", () => {
    assert.deepEqual(app.validateUsername("  Alice.Dev_1 "), { ok: true, value: "alice.dev_1" });
    assert.equal(app.validateUsername("ab").ok, false);
    assert.equal(app.validateUsername("a".repeat(33)).ok, false);
    assert.equal(app.validateUsername("a".repeat(32)).ok, true);
    assert.equal(app.validateUsername("-abc").ok, false);
    assert.equal(app.validateUsername(".abc").ok, false);
    assert.equal(app.validateUsername("张三丰").ok, false);
    assert.equal(app.validateUsername("ab c").ok, false);
    assert.equal(app.validateUsername("").ok, false);
  });

  test("密码至少 12 字符、至多 128 UTF-8 字节且不改写空白", () => {
    assert.equal(app.validatePassword("12345678901").ok, false);
    assert.deepEqual(app.validatePassword("  space kept "), { ok: true, value: "  space kept " });
    assert.equal(app.validatePassword("中".repeat(42)).ok, true); // 126 字节
    assert.equal(app.validatePassword("中".repeat(43)).ok, false); // 129 字节
    assert.equal(app.validatePassword("短", { forLogin: true }).ok, true);
    assert.equal(app.validatePassword("", { forLogin: true }).ok, false);
  });

  test("VM 名称去首尾空白、1–64 个 Unicode 字符、无控制字符", () => {
    assert.deepEqual(app.validateVmName("  开发环境 "), { ok: true, value: "开发环境" });
    assert.equal(app.validateVmName("   ").ok, false);
    assert.equal(app.validateVmName("😀".repeat(64)).ok, true);
    assert.equal(app.validateVmName("😀".repeat(65)).ok, false);
    assert.equal(app.validateVmName("a\u0007b").ok, false);
    assert.equal(app.validateVmName("a\u0085b").ok, false);
  });

  test("规范化服务端 VM 与系统数据", () => {
    assert.equal(app.normalizeVm({ ...vm(ID_A), id: "ABC" }), null);
    assert.equal(app.normalizeVm({ ...vm(ID_A), state: "paused" }).state, "unknown");
    assert.equal(app.normalizeVm(vm(ID_A, "ready", { message: "x" })).message, undefined);
    assert.equal(app.normalizeVm(vm(ID_A, "error", { message: "  镜像缺失\u0000 " })).message, "镜像缺失");
    const normalized = app.normalizeVm({ ...vm(ID_A), system_id: "Bad ID", system_label: "" });
    assert.equal(normalized.system_id, "");
    assert.deepEqual(
      app.normalizeSystemList([{ id: "debian-12", label: "Debian 12" }, { id: "debian-12", label: "dup" }, { id: "UPPER", label: "x" }, { id: "ubuntu", label: "" }, null]),
      [{ id: "debian-12", label: "Debian 12" }, { id: "ubuntu", label: "ubuntu" }],
    );
    assert.equal(app.isValidSystemId("a".repeat(33)), false);
  });
});

describe("workspace_url 精确校验", () => {
  test("接受与 VM ID 约定完全一致的地址", () => {
    assert.deepEqual(app.validateWorkspaceUrl(workspace(ID_A), ID_A, PORTAL), {
      ok: true,
      url: workspace(ID_A),
      origin: `https://vm-${ID_A}.cage.example.org`,
    });
    const dev = `http://vm-${ID_C}.cage.localhost:8787/_tenant/enter#abc`;
    assert.equal(app.validateWorkspaceUrl(dev, ID_C, DEV).ok, true);
  });

  const rejects = {
    其他VM: workspace(ID_B),
    大写ID: `https://vm-${ID_A.toUpperCase()}.cage.example.org/_tenant/enter#t`,
    协议不同: `http://vm-${ID_A}.cage.example.org/_tenant/enter#t`,
    多余端口: `https://vm-${ID_A}.cage.example.org:8443/_tenant/enter#t`,
    默认端口非规范: `https://vm-${ID_A}.cage.example.org:443/_tenant/enter#t`,
    主机后缀: `https://vm-${ID_A}.cage.example.org.evil.test/_tenant/enter#t`,
    主机前缀: `https://x.vm-${ID_A}.cage.example.org/_tenant/enter#t`,
    门户主机: `https://cage.example.org/_tenant/enter#t`,
    路径尾斜杠: `https://vm-${ID_A}.cage.example.org/_tenant/enter/#t`,
    根路径: `https://vm-${ID_A}.cage.example.org/#t`,
    查询参数: `https://vm-${ID_A}.cage.example.org/_tenant/enter?ticket=t#t`,
    票据在查询: `https://vm-${ID_A}.cage.example.org/_tenant/enter?t`,
    凭据: `https://user:pw@vm-${ID_A}.cage.example.org/_tenant/enter#t`,
    空票据: `https://vm-${ID_A}.cage.example.org/_tenant/enter#`,
    无票据: `https://vm-${ID_A}.cage.example.org/_tenant/enter`,
    非法票据字符: `https://vm-${ID_A}.cage.example.org/_tenant/enter#a%20b`,
    反斜杠: `https://vm-${ID_A}.cage.example.org\\_tenant/enter#t`,
    脚本协议: "javascript:alert(1)",
    数据协议: "data:text/html,<p>x</p>",
  };
  for (const [name, url] of Object.entries(rejects)) {
    test(`拒绝：${name}`, () => assert.equal(app.validateWorkspaceUrl(url, ID_A, PORTAL).ok, false));
  }

  test("拒绝非字符串、非法 VM ID 与非 localhost 的 HTTP 门户", () => {
    assert.equal(app.validateWorkspaceUrl(null, ID_A, PORTAL).ok, false);
    assert.equal(app.validateWorkspaceUrl(workspace(ID_A), "not-an-id", PORTAL).ok, false);
    const plain = { protocol: "http:", hostname: "cage.example.org", port: "", origin: "http://cage.example.org" };
    assert.equal(app.validateWorkspaceUrl(`http://vm-${ID_A}.cage.example.org/_tenant/enter#t`, ID_A, plain).ok, false);
    assert.equal(app.validateWorkspaceUrl(`http://vm-${ID_A}.cage.localhost/_tenant/enter#t`, ID_A, DEV).ok, false, "端口必须一致");
  });
});

describe("API 客户端", () => {
  test("POST 使用同源会话、JSON 与禁止缓存，且保持默认 mode", async () => {
    const f = fakeFetch();
    const api = app.createApi({ fetch: f.fetch });
    const pending = api.login("alice", "secret password");
    const call = f.next("POST", "/api/tenant/login");
    assert.deepEqual(call.body, { username: "alice", password: "secret password" });
    assert.equal(call.init.headers["Content-Type"], "application/json");
    assert.equal(call.init.credentials, "same-origin");
    assert.equal(call.init.cache, "no-store");
    assert.equal(call.init.redirect, "error");
    assert.equal("mode" in call.init, false, "不能改为 same-origin，否则 no-referrer 下 Origin 会变成 null");
    call.respond(200, { user: USER });
    assert.deepEqual((await pending).data, { user: USER });
  });

  test("GET 不带请求体；204 返回空数据", async () => {
    const f = fakeFetch();
    const api = app.createApi({ fetch: f.fetch });
    const list = api.listVms();
    const call = f.next("GET", "/api/tenant/vms");
    assert.equal(call.init.body, undefined);
    assert.equal(call.init.headers["Content-Type"], undefined);
    call.respond(200, { vms: [], limit: 5 });
    await list;
    const out = api.logout();
    f.next("POST", "/api/tenant/logout").respond(204);
    assert.deepEqual(await out, { status: 204, data: null });
  });

  test("创建请求只携带名称", async () => {
    const f = fakeFetch();
    const api = app.createApi({ fetch: f.fetch });
    const created = api.createVm("开发环境");
    const call = f.next("POST", "/api/tenant/vms");
    assert.deepEqual(call.body, { name: "开发环境" });
    call.respond(201, { vm: vm(ID_A) });
    await created;
  });

  test("错误信封、非 JSON 错误、网络错误与中止", async () => {
    const f = fakeFetch();
    const api = app.createApi({ fetch: f.fetch });
    const first = api.session();
    f.next("GET", "/api/tenant/session").respond(401, { error: { code: "unauthenticated", message: "请先登录\u0007。" } });
    await assert.rejects(first, (error) => error.code === "unauthenticated" && error.status === 401 && error.message === "请先登录 。");
    const second = api.listVms();
    f.next("GET", "/api/tenant/vms").respond(502, "<html>bad gateway</html>");
    await assert.rejects(second, (error) => error.code === "server_error" && /稍后/.test(error.message));
    const third = api.listVms();
    f.next("GET", "/api/tenant/vms").fail();
    await assert.rejects(third, (error) => error.code === "network_error" && !error.aborted);
    const controller = new AbortController();
    const fourth = api.listVms({ signal: controller.signal });
    controller.abort();
    await assert.rejects(fourth, (error) => error.aborted === true);
    const fifth = api.listVms();
    f.next("GET", "/api/tenant/vms").respond(200, "[]");
    await assert.rejects(fifth, (error) => error.code === "bad_response");
  });

  test("非法 VM ID 不发请求", async () => {
    const f = fakeFetch();
    const api = app.createApi({ fetch: f.fetch });
    await assert.rejects(api.openVm("../systems"), (error) => error.code === "vm_not_found");
    assert.equal(f.calls.length, 0);
  });
});

describe("会话与认证", () => {
  test("未登录 401 进入登录页；请求失败进入可重试错误页而非登录页", async () => {
    const ctx = setup();
    const first = ctx.portal.boot();
    ctx.f.next("GET", "/api/tenant/session").respond(401, { error: { code: "unauthenticated", message: "请先登录。" } });
    await first;
    assert.equal(ctx.state.phase, "signed-out");

    const second = ctx.portal.boot();
    ctx.f.next("GET", "/api/tenant/session").fail();
    await second;
    assert.equal(ctx.state.phase, "session-error");
    assert.equal(ctx.state.bootError.code, "network_error");

    await signIn(ctx, { vms: [vm(ID_A)] });
    assert.equal(ctx.state.vms.length, 1);
  });

  test("注册：客户端校验不发请求，成功后同时登录并加载列表", async () => {
    const ctx = setup();
    const booting = ctx.portal.boot();
    ctx.f.next("GET", "/api/tenant/session").respond(401, { error: { code: "unauthenticated", message: "" } });
    await booting;
    ctx.portal.setAuthMode("register");
    await ctx.portal.register({ username: "a!", password: "x", confirm: "x" });
    assert.equal(ctx.state.auth.error.field, "username");
    await ctx.portal.register({ username: "Alice", password: "short", confirm: "short" });
    assert.equal(ctx.state.auth.error.field, "password");
    await ctx.portal.register({ username: "Alice", password: "long enough pw", confirm: "long enough pW" });
    assert.equal(ctx.state.auth.error.field, "confirm");
    assert.equal(ctx.f.calls.length, 1, "校验失败不发请求");

    const registering = ctx.portal.register({ username: " Alice ", password: "long enough pw", confirm: "long enough pw" });
    assert.equal(ctx.state.auth.busy, true);
    await ctx.portal.register({ username: "alice", password: "long enough pw", confirm: "long enough pw" });
    assert.equal(ctx.f.count("POST", "/api/tenant/register"), 1, "忙碌时不重复提交");
    const call = ctx.f.next("POST", "/api/tenant/register");
    assert.deepEqual(call.body, { username: "alice", password: "long enough pw" });
    call.respond(201, { user: USER });
    await registering;
    assert.equal(ctx.state.phase, "signed-in");
    assert.equal(ctx.state.listStatus, "loading");
    ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms: [], limit: 5 });
    await flush();
    assert.equal(ctx.state.listStatus, "ready");
    assert.equal(ctx.state.limit, 5);
  });

  test("登录失败显示服务端文本；用户名冲突定位到用户名字段", async () => {
    const ctx = setup();
    const booting = ctx.portal.boot();
    ctx.f.next("GET", "/api/tenant/session").respond(401, { error: { code: "unauthenticated", message: "" } });
    await booting;
    const login = ctx.portal.login({ username: "bob", password: "whatever-pass" });
    ctx.f.next("POST", "/api/tenant/login").respond(401, { error: { code: "invalid_credentials", message: "用户名或密码不正确。" } });
    await login;
    assert.equal(ctx.state.phase, "signed-out");
    assert.deepEqual(ctx.state.auth.error, { field: null, message: "用户名或密码不正确。" });
    assert.equal(ctx.state.auth.busy, false);

    ctx.portal.setAuthMode("register");
    const register = ctx.portal.register({ username: "bob", password: "long enough pw", confirm: "long enough pw" });
    ctx.f.next("POST", "/api/tenant/register").respond(409, { error: { code: "username_taken", message: "该用户名已被使用。" } });
    await register;
    assert.equal(ctx.state.auth.error.field, "username");
  });

  test("检测到会话被其他标签页替换时重新同步", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A)] });
    const check = ctx.portal.checkSession();
    ctx.f.next("GET", "/api/tenant/session").respond(401, { error: { code: "unauthenticated", message: "" } });
    await check;
    assert.equal(ctx.state.phase, "signed-out");
    assert.match(ctx.state.auth.notice, /重新登录/);
  });
});

describe("打开、启动等待与轮询", () => {
  test("202 → 每约两秒只轮询目标 → ready 后再次 open → 校验并挂载 iframe", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A), vm(ID_B, "starting")] });
    ctx.portal.select(ID_A);
    assert.equal(ctx.state.target.status, "opening");
    assert.equal(ctx.state.selectedId, ID_A);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(202, { vm: vm(ID_A, "starting") });
    await flush();
    assert.equal(ctx.state.target.status, "starting");
    assert.equal(ctx.clock.size(), 1, "只有一个轮询计时器");

    await ctx.clock.advance(1999);
    assert.equal(ctx.f.count("GET", "/api/tenant/vms"), 1, "两秒内不轮询");
    await ctx.clock.advance(1);
    ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms: [vm(ID_A, "starting"), vm(ID_B, "starting")], limit: 5 });
    await flush();
    assert.equal(ctx.state.target.status, "starting");
    assert.equal(ctx.clock.size(), 1);

    await ctx.clock.advance(2000);
    ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms: [vm(ID_A, "ready"), vm(ID_B, "starting")], limit: 5 });
    await flush();
    assert.equal(ctx.state.target.status, "starting", "取得票据前保持启动中，避免闪烁");
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(200, { vm: vm(ID_A, "ready"), workspace_url: workspace(ID_A) });
    await flush();
    assert.equal(ctx.state.target.status, "connecting");
    assert.equal(ctx.state.target.frame.src, workspace(ID_A));
    assert.equal(ctx.f.count("POST", `/api/tenant/vms/${ID_B}/open`), 0, "不为其他 VM 发起 open");

    ctx.portal.frameLoaded(ctx.state.target.frame.key);
    assert.equal(ctx.state.target.status, "ready");
    assert.equal(ctx.state.target.frame.src, null, "载入后不在内存保留带票据的地址");
    assert.equal(ctx.clock.size(), 0, "就绪后无计时器");
    await ctx.clock.advance(60_000);
    assert.equal(ctx.f.pending().length, 0, "就绪后不再轮询");
    assert.equal(ctx.f.count("POST", `/api/tenant/vms/${ID_A}/open`), 2);
    assertOnlyContractEndpoints(ctx.f);
  });

  test("不重复提交 open：同一目标重复选择、重新连接与重试均被忽略", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A)] });
    ctx.portal.select(ID_A);
    ctx.portal.select(ID_A);
    ctx.portal.reconnect();
    ctx.portal.retry();
    assert.equal(ctx.f.count("POST", `/api/tenant/vms/${ID_A}/open`), 1);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(202, { vm: vm(ID_A, "starting") });
    await flush();
    ctx.portal.select(ID_A);
    ctx.portal.retry();
    assert.equal(ctx.f.count("POST", `/api/tenant/vms/${ID_A}/open`), 1);
    await ctx.clock.advance(2000);
    ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms: [vm(ID_A, "ready")], limit: 5 });
    await flush();
    ctx.portal.reconnect(); // 第二次 open 进行中
    assert.equal(ctx.f.count("POST", `/api/tenant/vms/${ID_A}/open`), 2);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(200, { vm: vm(ID_A, "ready"), workspace_url: workspace(ID_A) });
    await flush();
    const key = ctx.state.target.frame.key;
    ctx.portal.select(ID_A);
    assert.equal(ctx.state.target.frame.key, key, "已打开的目标不重新挂载");
  });

  test("页面隐藏时暂停轮询，恢复可见后只补一次", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A)] });
    ctx.portal.select(ID_A);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(202, { vm: vm(ID_A, "starting") });
    await flush();
    ctx.visibility.hidden = true;
    await ctx.clock.advance(10_000);
    assert.equal(ctx.f.count("GET", "/api/tenant/vms"), 1, "隐藏期间不轮询");
    ctx.visibility.hidden = false;
    ctx.portal.visibilityChanged();
    ctx.portal.visibilityChanged();
    await flush();
    assert.equal(ctx.f.count("GET", "/api/tenant/vms"), 2, "恢复后只补一次");
    ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms: [vm(ID_A, "starting")], limit: 5 });
    await flush();
    assert.equal(ctx.clock.size(), 1);
  });

  test("启动错误展示服务端文本并可手动重试；停止状态不自动重复 open", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A)] });
    ctx.portal.select(ID_A);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(202, { vm: vm(ID_A, "starting") });
    await ctx.clock.advance(2000);
    ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms: [vm(ID_A, "error", { message: "所选系统镜像缺失，请稍后重试。" })], limit: 5 });
    await flush();
    assert.equal(ctx.state.target.status, "error");
    assert.deepEqual(ctx.state.target.error, { code: "vm_error", message: "所选系统镜像缺失，请稍后重试。" });
    assert.equal(ctx.clock.size(), 0);

    ctx.portal.retry();
    assert.equal(ctx.state.target.status, "opening");
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(202, { vm: vm(ID_A, "starting") });
    await ctx.clock.advance(2000);
    ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms: [vm(ID_A, "stopped")], limit: 5 });
    await flush();
    assert.equal(ctx.state.target.error.code, "vm_stopped");
    assert.equal(ctx.f.count("POST", `/api/tenant/vms/${ID_A}/open`), 2, "停止状态不自动再 open");
  });

  test("资源紧张返回可重试错误，重试后恢复", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A)] });
    ctx.portal.select(ID_A);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(503, { error: { code: "runtime_unavailable", message: "主机资源紧张，请稍后重试。" } });
    await flush();
    assert.deepEqual(ctx.state.target.error, { code: "runtime_unavailable", status: 503, message: "主机资源紧张，请稍后重试。" });
    ctx.portal.retry();
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(200, { vm: vm(ID_A, "ready"), workspace_url: workspace(ID_A) });
    await flush();
    assert.equal(ctx.state.target.status, "connecting");
  });

  test("轮询网络波动退避重试，连续失败后转为可重试错误", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A)] });
    ctx.portal.select(ID_A);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(202, { vm: vm(ID_A, "starting") });
    await ctx.clock.advance(2000);
    ctx.f.next("GET", "/api/tenant/vms").fail();
    await flush();
    assert.equal(ctx.state.target.status, "starting");
    assert.equal(ctx.state.target.pollFailures, 1);
    await ctx.clock.advance(3999);
    assert.equal(ctx.f.pending().length, 0, "第一次退避 4 秒");
    await ctx.clock.advance(1);
    ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms: [vm(ID_A, "starting")], limit: 5 });
    await flush();
    assert.equal(ctx.state.target.pollFailures, 0, "成功后清零");
    for (let i = 0; i < app.POLL_MAX_FAILURES; i++) {
      await ctx.clock.advance(app.POLL_MAX_BACKOFF_MS);
      ctx.f.next("GET", "/api/tenant/vms").fail();
      await flush();
    }
    assert.equal(ctx.state.target.status, "error");
    assert.equal(ctx.state.target.error.code, "network_error");
    assert.equal(ctx.clock.size(), 0);
  });

  test("服务端返回不符合约定的地址时拒绝加载", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A), vm(ID_B)] });
    ctx.portal.select(ID_A);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(200, { vm: vm(ID_A, "ready"), workspace_url: workspace(ID_B) });
    await flush();
    assert.equal(ctx.state.target.status, "error");
    assert.equal(ctx.state.target.error.code, "invalid_workspace_url");
    assert.equal(ctx.state.target.frame, null);

    ctx.portal.select(ID_B);
    ctx.f.next("POST", `/api/tenant/vms/${ID_B}/open`).respond(200, { vm: vm(ID_A, "ready"), workspace_url: workspace(ID_A) });
    await flush();
    assert.equal(ctx.state.target.error.code, "bad_response", "响应中的 VM 与请求不一致");
  });

  test("iframe 载入过慢时提示，可继续等待或重新获取入口", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A, "ready")] });
    ctx.portal.select(ID_A);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(200, { vm: vm(ID_A, "ready"), workspace_url: workspace(ID_A) });
    await flush();
    await ctx.clock.advance(app.FRAME_SLOW_MS);
    assert.equal(ctx.state.target.frame.slow, true);
    ctx.portal.keepWaiting();
    assert.equal(ctx.state.target.frame.slow, false);
    await ctx.clock.advance(app.FRAME_SLOW_MS);
    assert.equal(ctx.state.target.frame.slow, true);
    const oldKey = ctx.state.target.frame.key;
    ctx.portal.retry();
    assert.equal(ctx.state.target.frame, null, "重试先卸载旧 iframe");
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(200, { vm: vm(ID_A, "ready"), workspace_url: workspace(ID_A, "fresh") });
    await flush();
    assert.notEqual(ctx.state.target.frame.key, oldKey);
    ctx.portal.frameLoaded(oldKey);
    assert.equal(ctx.state.target.status, "connecting", "旧 iframe 的 load 回调被忽略");
  });
});

describe("切换：卸载 iframe、取消并忽略过时请求", () => {
  test("启动中切换会中止旧请求、清除旧轮询，只轮询新目标且不发停止请求", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A), vm(ID_B)] });
    ctx.portal.select(ID_A);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(202, { vm: vm(ID_A, "starting") });
    await ctx.clock.advance(2000);
    const stalePoll = ctx.f.next("GET", "/api/tenant/vms");
    ctx.portal.select(ID_B);
    assert.equal(stalePoll.aborted, true, "旧轮询请求被中止");
    assert.equal(ctx.state.target.vmId, ID_B);
    assert.equal(ctx.clock.size(), 0, "旧轮询计时器已清除");
    ctx.f.next("POST", `/api/tenant/vms/${ID_B}/open`).respond(202, { vm: vm(ID_B, "starting") });
    await flush();
    await ctx.clock.advance(2000);
    ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms: [vm(ID_A, "starting"), vm(ID_B, "starting")], limit: 5 });
    await flush();
    assert.equal(ctx.clock.size(), 1, "只存在一个轮询计时器");
    assert.equal(ctx.f.count("POST", `/api/tenant/vms/${ID_A}/open`), 1);
    assertOnlyContractEndpoints(ctx.f);
  });

  test("过时响应即使晚到也被忽略", async () => {
    const ctx = setup({ ignoreAbort: true });
    await signIn(ctx, { vms: [vm(ID_A), vm(ID_B)] });
    ctx.portal.select(ID_A);
    const staleOpen = ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`);
    ctx.portal.select(ID_B);
    assert.equal(staleOpen.aborted, true);
    staleOpen.respond(200, { vm: vm(ID_A, "ready"), workspace_url: workspace(ID_A) });
    await flush();
    assert.equal(ctx.state.target.vmId, ID_B);
    assert.equal(ctx.state.target.status, "opening");
    assert.equal(ctx.state.target.frame, null);
    ctx.f.next("POST", `/api/tenant/vms/${ID_B}/open`).respond(200, { vm: vm(ID_B, "ready"), workspace_url: workspace(ID_B) });
    await flush();
    assert.equal(ctx.state.target.frame.src, workspace(ID_B));
  });

  test("已挂载 iframe 在切换时同步卸载", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A, "ready"), vm(ID_B, "ready")] });
    ctx.portal.select(ID_A);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(200, { vm: vm(ID_A, "ready"), workspace_url: workspace(ID_A) });
    await flush();
    ctx.portal.frameLoaded(ctx.state.target.frame.key);
    const frames = [];
    const unsubscribe = ctx.portal.subscribe((state) => frames.push(state.target?.frame ?? null));
    ctx.portal.select(ID_B);
    unsubscribe();
    assert.equal(frames[0], null, "切换后的第一次状态通知里 iframe 已卸载");
  });
});

describe("会话变化", () => {
  test("轮询中会话失效：回到登录页、清理计时器与 iframe", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A)] });
    ctx.portal.select(ID_A);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(202, { vm: vm(ID_A, "starting") });
    await ctx.clock.advance(2000);
    ctx.f.next("GET", "/api/tenant/vms").respond(401, { error: { code: "unauthenticated", message: "请先登录。" } });
    await flush();
    assert.equal(ctx.state.phase, "signed-out");
    assert.equal(ctx.state.target, null);
    assert.equal(ctx.state.vms.length, 0);
    assert.equal(ctx.clock.size(), 0);
    assert.match(ctx.state.auth.notice, /失效/);
  });

  test("VM 不存在或不归当前账户：移出列表、忘记所选并提示", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A), vm(ID_B)] });
    ctx.portal.select(ID_A);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(404, { error: { code: "vm_not_found", message: "虚拟机不存在。" } });
    await flush();
    assert.equal(ctx.state.target, null);
    assert.deepEqual(ctx.state.vms.map((item) => item.id), [ID_B]);
    assert.equal(ctx.state.banner.message, "虚拟机不存在。");
    assert.equal(app.createSelectionStore({ local: ctx.local, session: ctx.session }).read(USER.id), null);
  });

  test("登出：先卸载 iframe 并停止轮询，再请求撤销；成功后回到登录页", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A, "ready"), vm(ID_B)] });
    ctx.portal.select(ID_A);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(200, { vm: vm(ID_A, "ready"), workspace_url: workspace(ID_A) });
    await flush();
    assert.ok(ctx.state.target.frame);
    const logout = ctx.portal.logout();
    assert.equal(ctx.state.target, null, "请求返回前已卸载 iframe");
    assert.equal(ctx.state.logout.busy, true);
    ctx.portal.logout();
    assert.equal(ctx.f.count("POST", "/api/tenant/logout"), 1, "不重复提交登出");
    ctx.f.next("POST", "/api/tenant/logout").respond(204);
    await logout;
    assert.equal(ctx.state.phase, "signed-out");
    assert.equal(ctx.state.auth.notice, "已安全退出。");
    assert.equal(ctx.clock.size(), 0);
    await ctx.clock.advance(60_000);
    assert.equal(ctx.f.pending().length, 0);
  });

  test("登出进行中不再打开、切换或新建", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A), vm(ID_B)] });
    const logout = ctx.portal.logout();
    ctx.portal.select(ID_A);
    ctx.portal.openCreate();
    await ctx.portal.refreshList();
    assert.equal(ctx.state.target, null);
    assert.equal(ctx.state.create.open, false);
    assert.equal(ctx.f.calls.filter((call) => call.url.endsWith("/open") || call.url.endsWith("/systems")).length, 0);
    ctx.f.next("POST", "/api/tenant/logout").respond(204);
    await logout;
    assert.equal(ctx.state.phase, "signed-out");
  });

  test("登出失败保持登录并提示，可再次退出；401 视为已退出", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A)] });
    ctx.portal.select(ID_A);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(202, { vm: vm(ID_A, "starting") });
    await flush();
    const first = ctx.portal.logout();
    assert.equal(ctx.clock.size(), 0, "登出时停止轮询");
    ctx.f.next("POST", "/api/tenant/logout").respond(503, { error: { code: "runtime_unavailable", message: "服务暂不可用。" } });
    await first;
    assert.equal(ctx.state.phase, "signed-in");
    assert.equal(ctx.state.banner.action, "logout");
    assert.match(ctx.state.banner.message, /服务暂不可用/);
    const second = ctx.portal.logout();
    ctx.f.next("POST", "/api/tenant/logout").respond(401, { error: { code: "unauthenticated", message: "" } });
    await second;
    assert.equal(ctx.state.phase, "signed-out");
  });
});

describe("刷新恢复与本地存储", () => {
  test("恢复所选 VM 前用当前账户列表重新验证，再走同一个 open 接口", async () => {
    const local = memoryStorage();
    const session = memoryStorage();
    const store = app.createSelectionStore({ local, session });
    store.write(USER.id, ID_B);
    store.write("u-other", ID_A);
    const ctx = setup({ local, session });
    await signIn(ctx, { vms: [vm(ID_A), vm(ID_B)] });
    assert.equal(ctx.state.selectedId, ID_B);
    assert.equal(ctx.f.count("POST", `/api/tenant/vms/${ID_B}/open`), 1);
    assert.equal(ctx.f.count("POST", `/api/tenant/vms/${ID_A}/open`), 0, "不恢复其他账户的选择");
  });

  test("记住的 VM 不在当前列表中时不打开并被遗忘", async () => {
    const local = memoryStorage();
    const session = memoryStorage();
    app.createSelectionStore({ local, session }).write(USER.id, ID_C);
    const ctx = setup({ local, session });
    await signIn(ctx, { vms: [vm(ID_A)] });
    assert.equal(ctx.state.target, null);
    assert.equal(ctx.f.calls.filter((call) => call.url.endsWith("/open")).length, 0);
    assert.equal(app.createSelectionStore({ local, session }).read(USER.id), null);
  });

  test("存储中只有账户 ID 与 VM ID，绝无票据、地址、用户名或密码", async () => {
    const ctx = setup();
    const booting = ctx.portal.boot();
    ctx.f.next("GET", "/api/tenant/session").respond(401, { error: { code: "unauthenticated", message: "" } });
    await booting;
    const login = ctx.portal.login({ username: "alice", password: "very secret password" });
    ctx.f.next("POST", "/api/tenant/login").respond(200, { user: USER });
    await login;
    ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms: [vm(ID_A, "ready")], limit: 5 });
    await flush();
    ctx.portal.select(ID_A);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(200, { vm: vm(ID_A, "ready"), workspace_url: workspace(ID_A, "SECRET-TICKET") });
    await flush();
    const dumped = JSON.stringify([ctx.local.dump(), ctx.session.dump()]);
    for (const forbidden of ["SECRET-TICKET", "_tenant", "https:", "alice", "very secret password"]) {
      assert.equal(dumped.includes(forbidden), false, `存储中不应出现 ${forbidden}`);
    }
    assert.deepEqual(JSON.parse(ctx.local.dump()[app.SELECTION_KEY]), [[USER.id, ID_A]]);
    ctx.portal.goHome();
    assert.equal(ctx.local.dump()[app.SELECTION_KEY], undefined, "返回总览后不再恢复");
    assert.equal(ctx.state.target, null);
  });

  test("存储不可用时安全降级", () => {
    const broken = { getItem() { throw new Error("blocked"); }, setItem() { throw new Error("blocked"); }, removeItem() { throw new Error("blocked"); } };
    const store = app.createSelectionStore({ local: broken, session: broken });
    store.write(USER.id, ID_A);
    assert.equal(store.read(USER.id), null);
  });
});

describe("新建虚拟机", () => {
  test("只用名称创建并自动选中打开", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A)] });
    ctx.portal.openCreate();
    assert.equal(ctx.state.create.open, true);
    await ctx.portal.createVm("  ");
    assert.equal(ctx.state.create.error.field, "name");
    assert.equal(ctx.f.count("POST", "/api/tenant/vms"), 0);

    const creating = ctx.portal.createVm("  测试环境 ");
    await ctx.portal.createVm("again");
    assert.equal(ctx.f.count("POST", "/api/tenant/vms"), 1, "忙碌时不重复创建");
    const call = ctx.f.next("POST", "/api/tenant/vms");
    assert.deepEqual(call.body, { name: "测试环境" });
    call.respond(201, { vm: vm(ID_B, "stopped", { name: "测试环境", system_id: "ubuntu-24-04", system_label: "Ubuntu 24.04" }) });
    await creating;
    assert.equal(ctx.state.create.open, false);
    assert.deepEqual(ctx.state.vms.map((item) => item.id), [ID_A, ID_B]);
    assert.equal(ctx.state.selectedId, ID_B);
    assert.equal(ctx.state.vms[1].system_label, "Ubuntu 24.04");
    assert.equal(ctx.f.count("POST", `/api/tenant/vms/${ID_B}/open`), 1, "新建后默认选中并尝试打开");
  });

  test("重新打开弹窗清空上次的输入错误", async () => {
    const ctx = setup();
    await signIn(ctx);
    ctx.portal.openCreate();
    const previous = ctx.state.create.seq;
    await ctx.portal.createVm("");
    assert.equal(ctx.state.create.error.field, "name");
    ctx.portal.closeCreate();
    ctx.portal.openCreate();
    assert.ok(ctx.state.create.seq > previous);
    assert.equal(ctx.state.create.error, null);
  });

  test("配额：本地已满不发请求；服务端 vm_limit 显示文本并刷新列表", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A)], limit: 1 });
    ctx.portal.openCreate();
    await ctx.portal.createVm("第二台");
    assert.equal(ctx.state.create.error.code, "vm_limit");
    assert.equal(ctx.f.count("POST", "/api/tenant/vms"), 0);
    ctx.portal.closeCreate();

    const ctx2 = setup();
    await signIn(ctx2, { vms: [vm(ID_A)], limit: 5 });
    ctx2.portal.openCreate();
    const creating = ctx2.portal.createVm("第二台");
    ctx2.f.next("POST", "/api/tenant/vms").respond(409, { error: { code: "vm_limit", message: "服务器虚拟机总数已达上限。" } });
    await creating;
    assert.equal(ctx2.state.create.open, true);
    assert.equal(ctx2.state.create.error.message, "服务器虚拟机总数已达上限。");
    assert.equal(ctx2.f.count("GET", "/api/tenant/vms"), 2, "刷新一次列表");
  });

});

describe("错误文本映射", () => {
  // Every code internal/tenant can answer with, and its English message: none may reach the user as English.
  const GATEWAY = [
    ["session_changed", 409, "The signed-in account has changed. Refresh your session and try again.", "登录账户已在其他标签页变化。"],
    ["registration_closed", 403, "Self-service registration is closed. Contact the administrator for an account.", "此门户未开放自助注册，请联系管理员获取账户。"],
    ["invalid_credentials", 401, "Invalid username or password.", "用户名或密码不正确。"],
    ["username_taken", 409, "Username is already registered.", "该用户名已被使用。"],
    ["vm_limit", 409, "The workspace quota has been reached.", "已达到虚拟机数量上限。"],
    ["invalid_input", 400, "Invalid request fields.", "输入内容无效，请检查后重试。"],
    ["invalid_input", 400, "Invalid request target.", "输入内容无效，请检查后重试。"],
    ["origin_refused", 403, "Request host is not allowed.", "请求来源被拒绝，请从门户地址重新打开页面。"],
    ["origin_refused", 403, "Request origin is not allowed.", "请求来源被拒绝，请从门户地址重新打开页面。"],
    ["runtime_unavailable", 503, "The service or workspace is temporarily unavailable.", "虚拟机运行时暂时不可用，请稍后重试。"],
    ["not_found", 404, "Resource not found.", "请求的内容不存在。"],
    ["vm_not_found", 404, "Workspace not found.", "虚拟机不存在或无权访问。"],
    ["unauthenticated", 401, "A valid sign-in is required.", "登录已失效，请重新登录。"],
    ["rate_limited", 429, "Too many requests. Try again shortly.", "操作过于频繁，请稍后再试。"],
    ["invalid_content_type", 415, "Use application/json with UTF-8.", "请求格式不受支持。"],
  ];

  test("网关的每个错误码都换成中文，不显示英文原文", () => {
    for (const [code, status, english, chinese] of GATEWAY) {
      assert.deepEqual(app.describeProblem(code, english, status), { message: chinese }, code);
    }
  });

  test("已经是中文的服务端文本原样显示", () => {
    assert.deepEqual(app.describeProblem("vm_limit", "每个账户最多 5 台虚拟机。", 409), { message: "每个账户最多 5 台虚拟机。" });
    assert.deepEqual(app.describeProblem("mystery", "  镜像缺失\u0000 ", 500), { message: "镜像缺失" });
  });

  test("未知错误码：通用中文句子，服务端原文作为次要说明", () => {
    assert.deepEqual(app.describeProblem("quota_frozen", "Quota is frozen.", 409), { message: "请求失败（HTTP 409），请稍后重试。", detail: "Quota is frozen." });
    assert.deepEqual(app.describeProblem(undefined, "Unexpected token < in JSON", 0), { message: "请求失败，请稍后重试。", detail: "Unexpected token < in JSON" });
    assert.deepEqual(app.describeProblem("quota_frozen", "", 502), { message: "请求失败（HTTP 502），请稍后重试。" });
    assert.deepEqual(app.describeProblem("toString", "x", 400), { message: "请求失败（HTTP 400），请稍后重试。", detail: "x" }, "原型上的名字不算已知错误码");
  });

  test("虚拟机启动失败文本：网关的固定句子换成中文，其他英文保留为说明", () => {
    const startup = "The workspace is unavailable. Open it again to retry.";
    assert.deepEqual(app.describeVmFailure(startup), { message: "虚拟机暂时无法启动，可以重新打开再试。" });
    assert.deepEqual(app.describeVmFailure("disk quota exceeded"), { message: "虚拟机启动失败，可以重试。", detail: "disk quota exceeded" });
    assert.deepEqual(app.describeVmFailure("所选系统镜像缺失。"), { message: "所选系统镜像缺失。" });
    assert.equal(app.describeVmFailure("  "), null);
    const failed = app.normalizeVm(vm(ID_A, "error", { message: startup }));
    assert.equal(failed.message, "虚拟机暂时无法启动，可以重新打开再试。");
    assert.equal("message_detail" in failed, false);
    assert.equal(app.normalizeVm(vm(ID_A, "error", { message: "disk quota exceeded" })).message_detail, "disk quota exceeded");
  });

  test("控制器经过映射：英文错误显示中文，未知错误码保留原文", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [], limit: 5 });
    ctx.portal.openCreate();
    const creating = ctx.portal.createVm("第一台");
    ctx.f.next("POST", "/api/tenant/vms").respond(400, { error: { code: "invalid_input", message: "Invalid request fields." } });
    await creating;
    assert.equal(ctx.state.create.error.message, "输入内容无效，请检查后重试。");
    assert.equal("detail" in ctx.state.create.error, false);

    const again = ctx.portal.createVm("第一台");
    ctx.f.next("POST", "/api/tenant/vms").respond(409, { error: { code: "quota_frozen", message: "Quota is frozen." } });
    await again;
    assert.equal(ctx.state.create.error.message, "请求失败（HTTP 409），请稍后重试。");
    assert.equal(ctx.state.create.error.detail, "Quota is frozen.");
  });

  test("总览行的操作文字：开机中的虚拟机只查看进度", () => {
    assert.deepEqual({ ...app.VM_ACTION_LABELS }, { ready: "打开", starting: "查看进度", stopped: "启动并打开", error: "启动并打开" });
  });
});

describe("会话绑定与跨标签同步", () => {
  const unauth = { error: { code: "unauthenticated", message: "请先登录。" } };
  const changed = { error: { code: "session_changed", message: "登录账户已变化。" } };

  async function openReady(ctx, id = ID_A) {
    ctx.portal.select(id);
    ctx.f.next("POST", `/api/tenant/vms/${id}/open`).respond(200, { vm: vm(id, "ready"), workspace_url: workspace(id) });
    await flush();
    assert.ok(ctx.state.target.frame);
  }

  test("已知用户的目录/列表/创建/打开/登出请求都带 X-Cage-Expected-User，会话与登录请求不带", async () => {
    const ctx = setup();
    const booting = ctx.portal.boot();
    const sessionCall = ctx.f.next("GET", "/api/tenant/session");
    assert.equal(sessionCall.init.headers[HEADER], undefined);
    sessionCall.respond(401, unauth);
    await booting;
    const login = ctx.portal.login({ username: "alice", password: "long enough pw" });
    const loginCall = ctx.f.next("POST", "/api/tenant/login");
    assert.equal(loginCall.init.headers[HEADER], undefined);
    loginCall.respond(200, { user: USER });
    await login;
    const list = ctx.f.next("GET", "/api/tenant/vms");
    assert.equal(list.init.headers[HEADER], USER.id);
    list.respond(200, { vms: [vm(ID_A)], limit: 5 });
    await flush();
    ctx.portal.select(ID_A);
    assert.equal(ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).init.headers[HEADER], USER.id);
    ctx.portal.openCreate();
    const creating = ctx.portal.createVm("测试");
    const create = ctx.f.next("POST", "/api/tenant/vms");
    assert.equal(create.init.headers[HEADER], USER.id);
    create.respond(201, { vm: vm(ID_B) });
    await creating;
    const logout = ctx.portal.logout();
    assert.equal(ctx.f.next("POST", "/api/tenant/logout").init.headers[HEADER], USER.id);
    ctx.f.next("POST", "/api/tenant/logout").respond(204);
    await logout;
  });

  test("创建时 409 session_changed：立即卸载旧工作台、关闭弹窗、重读会话，不把创建重放到新账户", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A, "ready")] });
    await openReady(ctx);
    ctx.portal.openCreate();
    const creating = ctx.portal.createVm("新环境");
    ctx.f.next("POST", "/api/tenant/vms").respond(409, changed);
    await creating;
    assert.equal(ctx.state.phase, "booting", "旧账户界面立即清空");
    assert.equal(ctx.state.target, null, "旧 iframe 已卸载");
    assert.equal(ctx.state.vms.length, 0);
    assert.equal(ctx.state.create.open, false);
    ctx.f.next("GET", "/api/tenant/session").respond(200, { user: USER2 });
    await flush();
    assert.equal(ctx.state.user.id, USER2.id);
    assert.match(ctx.state.banner.message, /切换为 bob/);
    const bobList = ctx.f.next("GET", "/api/tenant/vms");
    assert.equal(bobList.init.headers[HEADER], USER2.id);
    bobList.respond(200, { vms: [], limit: 5 });
    await flush();
    assert.equal(ctx.f.count("POST", "/api/tenant/vms"), 1, "创建没有被重放");
    assert.equal(ctx.state.create.open, false);
  });

  test("轮询或打开时 409 session_changed：不在新账户下重放打开", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A)] });
    ctx.portal.select(ID_A);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(202, { vm: vm(ID_A, "starting") });
    await ctx.clock.advance(2000);
    ctx.f.next("GET", "/api/tenant/vms").respond(409, changed);
    await flush();
    assert.equal(ctx.state.target, null);
    assert.equal(ctx.clock.size(), 0, "旧轮询已停止");
    ctx.f.next("GET", "/api/tenant/session").respond(200, { user: USER2 });
    await flush();
    ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms: [vm(ID_B)], limit: 5 });
    await flush();
    await ctx.clock.advance(10_000);
    assert.equal(ctx.f.count("POST", `/api/tenant/vms/${ID_A}/open`), 1);
    assert.equal(ctx.f.count("POST", `/api/tenant/vms/${ID_B}/open`), 0, "不自动替新账户打开");
    assert.equal(ctx.state.target, null);
  });

  test("登出时 409 session_changed：不退出新账户，只重新同步", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A)] });
    const logout = ctx.portal.logout();
    ctx.f.next("POST", "/api/tenant/logout").respond(409, changed);
    await logout;
    ctx.f.next("GET", "/api/tenant/session").respond(200, { user: USER2 });
    await flush();
    assert.equal(ctx.state.phase, "signed-in");
    assert.equal(ctx.state.user.id, USER2.id);
    assert.equal(ctx.f.count("POST", "/api/tenant/logout"), 1);
    assert.equal(ctx.broadcast.posts, 0);
  });

  test("重新检查会话：同一账户保持工作台；换了账户则卸载并切换；已退出则回到登录页", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A, "ready")] });
    await openReady(ctx);
    const frameKey = ctx.state.target.frame.key;
    const same = ctx.portal.checkSession();
    ctx.f.next("GET", "/api/tenant/session").respond(200, { user: USER });
    await same;
    assert.equal(ctx.state.target.frame.key, frameKey, "同一账户不打断工作台");

    const switched = ctx.portal.checkSession();
    ctx.f.next("GET", "/api/tenant/session").respond(200, { user: USER2 });
    await switched;
    assert.equal(ctx.state.user.id, USER2.id);
    assert.equal(ctx.state.target, null);
    assert.equal(ctx.state.banner.kind, "info");
    ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms: [], limit: 5 });
    await flush();

    const gone = ctx.portal.checkSession();
    ctx.f.next("GET", "/api/tenant/session").respond(401, unauth);
    await gone;
    assert.equal(ctx.state.phase, "signed-out");
    assert.match(ctx.state.auth.notice, /其他标签页退出/);
  });

  test("登录页收到跨标签通知后读取会话并进入对应账户", async () => {
    const ctx = setup();
    const booting = ctx.portal.boot();
    ctx.f.next("GET", "/api/tenant/session").respond(401, unauth);
    await booting;
    const check = ctx.portal.checkSession();
    ctx.f.next("GET", "/api/tenant/session").respond(200, { user: USER2 });
    await check;
    assert.equal(ctx.state.phase, "signed-in");
    assert.match(ctx.state.banner.message, /已在其他标签页登录为 bob/);
  });

  test("过期的会话检查结果被忽略：新检查取代旧检查，本地状态变化后晚到的结果不生效", async () => {
    const ctx = setup({ ignoreAbort: true });
    await signIn(ctx, { vms: [vm(ID_A)] });
    ctx.portal.checkSession();
    const first = ctx.f.next("GET", "/api/tenant/session");
    ctx.portal.checkSession();
    assert.equal(first.aborted, true, "新的检查中止旧检查");
    const second = ctx.f.calls.filter((call) => call.url === "/api/tenant/session").at(-1);
    first.respond(200, { user: USER2 });
    await flush();
    assert.equal(ctx.state.user.id, USER.id, "被取代的检查结果不生效");
    second.respond(200, { user: USER });
    await flush();
    assert.equal(ctx.state.user.id, USER.id);

    ctx.portal.checkSession();
    const late = ctx.f.calls.filter((call) => call.url === "/api/tenant/session").at(-1);
    const logout = ctx.portal.logout();
    ctx.f.next("POST", "/api/tenant/logout").respond(204);
    await logout;
    late.respond(200, { user: USER });
    await flush();
    assert.equal(ctx.state.phase, "signed-out", "退出后晚到的会话结果不会重新登录");
  });

  test("焦点与可见性触发的检查在短时间内合并", async () => {
    const ctx = setup();
    await signIn(ctx);
    ctx.portal.checkSession({ force: false });
    ctx.portal.checkSession({ force: false });
    assert.equal(ctx.f.count("GET", "/api/tenant/session"), 2, "boot 一次 + 合并后一次");
    ctx.f.next("GET", "/api/tenant/session").respond(200, { user: USER });
    await flush();
    ctx.portal.checkSession({ force: false });
    assert.equal(ctx.f.count("GET", "/api/tenant/session"), 2, "间隔内不重复检查");
    await ctx.clock.advance(app.SESSION_CHECK_GAP_MS);
    ctx.portal.checkSession({ force: false });
    assert.equal(ctx.f.count("GET", "/api/tenant/session"), 3);
  });

  test("登录、注册与退出成功后通知其他标签页；退出失败不通知", async () => {
    const ctx = setup();
    await signIn(ctx);
    const failed = ctx.portal.logout();
    ctx.f.next("POST", "/api/tenant/logout").respond(503, { error: { code: "runtime_unavailable", message: "x" } });
    await failed;
    assert.equal(ctx.broadcast.posts, 0);
    const ok = ctx.portal.logout();
    ctx.f.next("POST", "/api/tenant/logout").respond(204);
    await ok;
    assert.equal(ctx.broadcast.posts, 1);
    const login = ctx.portal.login({ username: "alice", password: "long enough pw" });
    ctx.f.next("POST", "/api/tenant/login").respond(200, { user: USER });
    await login;
    assert.equal(ctx.broadcast.posts, 2);
  });
});

describe("竞态与恢复", () => {
  test("旧列表晚到不会覆盖刚新建的 VM", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A)] });
    ctx.portal.openCreate();
    ctx.portal.refreshList();
    const staleList = ctx.f.next("GET", "/api/tenant/vms");
    const creating = ctx.portal.createVm("新环境");
    ctx.f.next("POST", "/api/tenant/vms").respond(201, { vm: vm(ID_B, "stopped", { name: "新环境" }) });
    await creating;
    assert.equal(ctx.state.selectedId, ID_B);
    staleList.respond(200, { vms: [vm(ID_A)], limit: 5 });
    await flush();
    assert.deepEqual(ctx.state.vms.map((item) => item.id), [ID_A, ID_B], "新建的 VM 仍在列表中");
    assert.equal(ctx.state.selectedId, ID_B);
    assert.equal(ctx.state.target.vmId, ID_B);
    assert.equal(ctx.state.banner, null, "没有误报“虚拟机不存在”");

    // 新建之后发出的列表若不含该 VM，才视为真正消失。
    ctx.f.next("POST", `/api/tenant/vms/${ID_B}/open`).respond(202, { vm: vm(ID_B, "starting") });
    await ctx.clock.advance(2000);
    ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms: [vm(ID_A)], limit: 5 });
    await flush();
    assert.deepEqual(ctx.state.vms.map((item) => item.id), [ID_A]);
    assert.equal(ctx.state.target, null);
  });

  test("退出失败后继续加载：列表未就绪时重新加载并恢复所选，已打开的 VM 重新连接", async () => {
    const local = memoryStorage();
    const session = memoryStorage();
    app.createSelectionStore({ local, session }).write(USER.id, ID_A);
    const ctx = setup({ local, session });
    const booting = ctx.portal.boot();
    ctx.f.next("GET", "/api/tenant/session").respond(200, { user: USER });
    await booting;
    assert.equal(ctx.state.listStatus, "loading");
    const firstList = ctx.f.next("GET", "/api/tenant/vms");
    const logout = ctx.portal.logout();
    assert.equal(firstList.aborted, true);
    ctx.f.next("POST", "/api/tenant/logout").respond(503, { error: { code: "runtime_unavailable", message: "服务暂不可用。" } });
    await logout;
    assert.equal(ctx.state.phase, "signed-in");
    ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms: [vm(ID_A, "ready")], limit: 5 });
    await flush();
    assert.equal(ctx.state.listStatus, "ready", "列表继续加载完成");
    assert.equal(ctx.state.selectedId, ID_A, "恢复记住的所选 VM");
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(200, { vm: vm(ID_A, "ready"), workspace_url: workspace(ID_A) });
    await flush();
    assert.ok(ctx.state.target.frame);

    const second = ctx.portal.logout();
    assert.equal(ctx.state.target, null);
    ctx.f.next("POST", "/api/tenant/logout").respond(503, { error: { code: "runtime_unavailable", message: "服务暂不可用。" } });
    await second;
    assert.equal(ctx.state.target.vmId, ID_A, "退出未完成时重新打开原来的 VM");
    assert.equal(ctx.state.banner.action, "logout");
    assert.equal(ctx.f.count("POST", `/api/tenant/vms/${ID_A}/open`), 2);
  });

  test("此前已运行的 VM 返回 202：约 350ms 先确认一次，之后约 2 秒，文案为确认而非开机", async () => {
    const ctx = setup();
    await signIn(ctx, { vms: [vm(ID_A, "ready"), vm(ID_B, "stopped")] });
    ctx.portal.select(ID_A);
    assert.equal(ctx.state.target.warm, true);
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(202, { vm: vm(ID_A, "starting") });
    await flush();
    assert.equal(app.targetStatusLabel(ctx.state.target), "确认中");
    await ctx.clock.advance(app.WARM_POLL_MS - 1);
    assert.equal(ctx.f.count("GET", "/api/tenant/vms"), 1);
    await ctx.clock.advance(1);
    ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms: [vm(ID_A, "starting"), vm(ID_B)], limit: 5 });
    await flush();
    assert.equal(ctx.state.target.warm, false, "确认后仍在启动则按开机显示");
    assert.equal(app.targetStatusLabel(ctx.state.target), "开机中");
    await ctx.clock.advance(1999);
    assert.equal(ctx.f.count("GET", "/api/tenant/vms"), 2);
    await ctx.clock.advance(1);
    ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms: [vm(ID_A, "ready"), vm(ID_B)], limit: 5 });
    await flush();
    ctx.f.next("POST", `/api/tenant/vms/${ID_A}/open`).respond(202, { vm: vm(ID_A, "starting") });
    await flush();
    await ctx.clock.advance(app.WARM_POLL_MS);
    assert.equal(ctx.f.count("GET", "/api/tenant/vms"), 3, "快速确认只做一次");

    ctx.portal.select(ID_B);
    assert.equal(ctx.state.target.warm, false);
    ctx.f.next("POST", `/api/tenant/vms/${ID_B}/open`).respond(202, { vm: vm(ID_B, "starting") });
    await ctx.clock.advance(app.WARM_POLL_MS);
    assert.equal(ctx.f.count("GET", "/api/tenant/vms"), 3, "冷启动的 VM 不做快速确认");
    assert.equal(app.targetStatusLabel(ctx.state.target), "开机中");
  });

  test("注册关闭：显示提示、不再尝试注册，登录不受影响", async () => {
    const ctx = setup();
    const booting = ctx.portal.boot();
    ctx.f.next("GET", "/api/tenant/session").respond(401, { error: { code: "unauthenticated", message: "" } });
    await booting;
    ctx.portal.setAuthMode("register");
    const register = ctx.portal.register({ username: "carol", password: "long enough pw", confirm: "long enough pw" });
    ctx.f.next("POST", "/api/tenant/register").respond(403, { error: { code: "registration_closed", message: "未开放注册。" } });
    await register;
    assert.equal(ctx.state.registrationClosed, true);
    assert.equal(ctx.state.auth.error, null);
    assert.equal(ctx.state.auth.busy, false);
    await ctx.portal.register({ username: "carol", password: "long enough pw", confirm: "long enough pw" });
    assert.equal(ctx.f.count("POST", "/api/tenant/register"), 1, "已知关闭后不再请求注册");
    ctx.portal.setAuthMode("login");
    assert.equal(ctx.state.registrationClosed, true);
    const login = ctx.portal.login({ username: "alice", password: "long enough pw" });
    ctx.f.next("POST", "/api/tenant/login").respond(200, { user: USER });
    await login;
    assert.equal(ctx.state.phase, "signed-in");
    ctx.f.next("GET", "/api/tenant/vms").respond(200, { vms: [], limit: 5 });
    await flush();
    const logout = ctx.portal.logout();
    ctx.f.next("POST", "/api/tenant/logout").respond(204);
    await logout;
    assert.equal(ctx.state.registrationClosed, true, "本页内保持已知的关闭状态");
  });
});

describe("静态约束", () => {
  test("app.mjs 不拼接 HTML、不用内联样式或动态代码，iframe 属性与契约一致", async () => {
    const source = await readFile(join(HERE, "app.mjs"), "utf8");
    for (const pattern of [/\.innerHTML\b/, /\.outerHTML\b/, /insertAdjacentHTML/, /document\.write/, /\beval\s*\(/, /new Function\b/, /\.style\./, /setAttribute\(\s*["']style["']/, /\bimport\s*\(/, /^\s*import\s/m]) {
      assert.equal(pattern.test(source), false, `不应出现 ${pattern}`);
    }
    assert.equal(app.IFRAME_SANDBOX, "allow-scripts allow-same-origin allow-forms allow-downloads");
    assert.equal(app.IFRAME_REFERRER_POLICY, "no-referrer");
    assert.equal(app.iframeAllow("https://vm-x.cage.example.org"), "clipboard-read https://vm-x.cage.example.org; clipboard-write https://vm-x.cage.example.org");
    assert.match(source, /setAttribute\("allow", iframeAllow\(checked\.origin\)\)/, "只委托给已校验的 VM 源");
    const urls = source.match(/https?:\/\/[^\s"'`]+/g) || [];
    assert.deepEqual([...new Set(urls)], ["http://www.w3.org/2000/svg"], "除 SVG 命名空间外不引用任何外部地址");
    assert.equal(/localStorage\.setItem|sessionStorage\.setItem/.test(source), false, "只经由封装的存储写入");
  });

  test("index.html 只同源加载模块和样式，无内联脚本或样式", async () => {
    const html = await readFile(join(HERE, "index.html"), "utf8");
    const scripts = [...html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/g)];
    assert.equal(scripts.length, 1);
    assert.match(scripts[0][1], /type="module"/);
    assert.match(scripts[0][1], /src="\/app\.mjs"/);
    assert.equal(scripts[0][2].trim(), "");
    assert.equal(/<style\b|\sstyle=|\son[a-z]+=/i.test(html), false);
    const refs = [...html.matchAll(/(?:href|src)="([^"]+)"/g)].map((m) => m[1]);
    // The brand mark is inlined once as <symbol>s and reused through <use href="#id">: fragment references
    // must resolve to a symbol in this same file; every other reference is still only the two same-origin files.
    const symbols = new Set([...html.matchAll(/<symbol\b[^>]*\bid="([^"]+)"/g)].map((m) => m[1]));
    for (const ref of refs.filter((value) => value.startsWith("#"))) assert.ok(symbols.has(ref.slice(1)), `${ref} 指向本文件中的 symbol`);
    // Besides the two files, only the gateway's fixed icons and web manifest (internal/tenant/portal_icons.go).
    assert.deepEqual(refs.filter((value) => !value.startsWith("#")), [
      "/favicon.ico", "/icon.svg", "/apple-touch-icon.png", "/manifest.webmanifest", "/styles.css", "/app.mjs",
    ]);
    assert.equal(/<image\b|<foreignObject\b/i.test(html), false, "内联 SVG 不加载图像或嵌入 HTML");
    assert.equal(/https?:\/\//.test(html), false);
    assert.match(html, /<html lang="zh-CN">/);
  });

  test("styles.css 不加载外部资源", async () => {
    const css = await readFile(join(HERE, "styles.css"), "utf8");
    assert.equal(/@import|url\(|https?:\/\//i.test(css), false);
    assert.match(css, /prefers-color-scheme: dark/);
    assert.match(css, /\[data-theme="dark"\]/);
  });
});

describe("手机上的工作台：门户自己判断", () => {
  const device = ({ agent = "", mobile, coarse = true, hover = false, width = 390, height = 844, screen = [390, 844] } = {}) => ({
    navigator: { userAgent: agent, userAgentData: mobile === undefined ? undefined : { mobile } },
    matchMedia: (query) => ({ matches: query === "(pointer: coarse)" ? coarse : query === "(hover: hover)" ? hover : false }),
    innerWidth: width,
    innerHeight: height,
    screen: { width: screen[0], height: screen[1] },
  });
  const iPhone = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1";
  const iPad = "Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1";
  const pixel = "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36";
  const mac = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15";

  test("phoneDevice 与网关（internal/webentry）和页面守卫（entry-layout）的规则一致", () => {
    assert.equal(app.phoneDevice(device({ agent: iPhone })), true);
    assert.equal(app.phoneDevice(device({ agent: pixel, mobile: true })), true);
    assert.equal(app.phoneDevice(device({ agent: pixel, mobile: false, width: 980, height: 2000 })), false, "Client Hints 说不是手机，且是电脑版网页");
    assert.equal(app.phoneDevice(device({ agent: iPad, width: 820, height: 1180, screen: [820, 1180] })), false, "平板");
    assert.equal(app.phoneDevice(device({ agent: mac, width: 375, height: 667, screen: [375, 667] })), true, "报告为电脑的触屏小窗口");
    assert.equal(app.phoneDevice(device({ agent: mac, coarse: false, hover: true, width: 375, height: 667 })), false, "缩窄的电脑浏览器");
    assert.equal(app.phoneDevice(device({ agent: mac, width: 980, height: 450 })), false, "横屏请求电脑版网页");
    assert.equal(app.phoneDevice(device({ agent: mac, width: 900, height: 450 })), false, "布局视口宽于屏幕");
  });

  test("externalLink 只接受 http(s)、无凭据、有长度上限、没有控制和方向字符", () => {
    assert.deepEqual(app.externalLink("https://example.com/a?b=1#c"), { href: "https://example.com/a?b=1#c", host: "example.com" });
    assert.deepEqual(app.externalLink("http://example.com:8080/"), { href: "http://example.com:8080/", host: "example.com:8080" });
    assert.equal(app.externalLink("https://bücher.example/").host, "xn--bcher-kva.example", "国际化域名显示为 punycode");
    for (const bad of [
      "javascript:alert(1)", "data:text/html,x", "file:///etc/passwd", "blob:https://example.com/x", "about:blank",
      "https://user:pass@example.com/", "https://user@example.com/", "not a url", "", null, 42, { toString: () => "https://example.com/" },
      "https://example.com/" + "a".repeat(app.LINK_MAX_LENGTH),
      "https://exa\tmple.com/", "https://example.com/\n", "https://example.com/\u0000", "https://example.com/\u0085",
      "https://example.com/\u202egnp.exe", "https://example.com/\u2066x\u2069", "https://exa\u200bmple.com/", "https://example.com/\u061c", "\ufeffhttps://example.com/",
    ]) assert.equal(app.externalLink(bad), null, JSON.stringify(String(bad)).slice(0, 60));
    assert.ok(app.externalLink("https://example.com/" + "a".repeat(app.LINK_MAX_LENGTH - 20)));
  });

  test("重新进入：每台虚拟机每分钟最多一次，持续请求时间隔翻倍，安静后恢复", () => {
    let now = 0;
    const allow = app.createReentryLimiter(() => now);
    const minute = app.REENTRY_MIN_GAP_MS;
    assert.equal(allow("a"), true);
    assert.equal(allow("a"), false);
    assert.equal(allow("b"), true, "另一台虚拟机各自计数");
    now = minute - 1;
    assert.equal(allow("a"), false);
    const allowed = [];
    for (now = minute; now < 40 * minute; now += 1000) if (allow("a")) allowed.push(now / minute);
    assert.deepEqual(allowed, [1, 3, 7, 15, 31], "一直请求：间隔 1、2、4、8、16 分钟");
    now = 31 * minute + 2 * app.REENTRY_MAX_GAP_MS;
    assert.equal(allow("a"), true, "安静一段时间后");
    now += minute - 1;
    assert.equal(allow("a"), false);
    now += 1;
    assert.equal(allow("a"), true, "又回到一分钟");
  });

  test("门户不替工作台打开标签页，也不接受工作台给出的深度", async () => {
    const source = await readFile(join(HERE, "app.mjs"), "utf8");
    assert.equal(/\bwin\.open\(|window\.open\(/.test(source), false, "门户代码里没有 window.open");
    const handler = source.slice(source.indexOf("function phoneMessage("), source.indexOf("win.addEventListener(\"popstate\""));
    assert.equal(/depth/.test(handler), false);
    assert.match(handler, /type === "cage:phone-ui"\) return true/);
  });
});

describe("服务器在线更新（仅管理员可见）", () => {
  test("管理员会话加载更新状态、检查更新、应用更新并在重启恢复后触发刷新", async () => {
    let reloaded = "";
    const f = fakeFetch();
    const clock = fakeClock();
    const portal = app.createPortal({
      api: app.createApi({ fetch: f.fetch }),
      selection: app.createSelectionStore({ local: memoryStorage(), session: memoryStorage() }),
      timers: clock.timers,
      now: clock.now,
      location: PORTAL,
      isHidden: () => false,
      onReload: (ver) => { reloaded = ver || "reloaded"; },
    });
    const booting = portal.boot();
    f.next("GET", "/api/tenant/session").respond(200, { user: USER, self_update: true });
    await flush();
    f.next("GET", "/api/tenant/vms").respond(200, { vms: [vm(ID_A, "ready")], limit: 5 });
    f.next("GET", "/api/tenant/update").respond(200, {
      enabled: true,
      current_version: "0.1.19",
      arch: "amd64",
      latest_version: "0.1.20",
      update_available: true,
      job: { running: false, phase: "idle", progress: 0 },
    });
    await booting;
    await flush();
    assert.equal(portal.getState().canUpdate, true);
    assert.equal(portal.getState().update.data.update_available, true);

    portal.openUpdate();
    assert.equal(portal.getState().update.open, true);
    f.next("GET", "/api/tenant/update").respond(200, {
      enabled: true,
      current_version: "0.1.19",
      arch: "amd64",
      latest_version: "0.1.20",
      update_available: true,
      job: { running: false, phase: "idle", progress: 0 },
    });
    await flush();

    const applying = portal.applyUpdate();
    const applyCall = f.next("POST", "/api/tenant/update/apply");
    assert.equal(applyCall.init.headers[HEADER], USER.id);
    assert.deepEqual(applyCall.body, { version: "0.1.20" });
    applyCall.respond(202, {
      enabled: true,
      current_version: "0.1.19",
      arch: "amd64",
      latest_version: "0.1.20",
      update_available: true,
      job: { running: true, phase: "downloading", target_version: "0.1.20", progress: 20, logs: ["12:00:00 下载中"] },
    });
    await applying;
    await flush();
    assert.equal(portal.getState().update.data.job.phase, "downloading");

    await clock.advance(app.UPDATE_POLL_MS);
    f.next("GET", "/api/tenant/update").respond(200, {
      enabled: true,
      current_version: "0.1.19",
      arch: "amd64",
      job: { running: true, phase: "restarting", target_version: "0.1.20", progress: 100 },
    });
    await flush();
    assert.equal(portal.getState().update.reconnecting, true);

    // 网关重启期间的瞬时断连继续轮询，重启完成后自动刷新页面。
    await clock.advance(app.UPDATE_POLL_MS);
    f.next("GET", "/api/tenant/update").fail();
    await flush();
    assert.equal(portal.getState().update.reconnecting, true);

    await clock.advance(app.UPDATE_POLL_MS);
    f.next("GET", "/api/tenant/update").respond(200, {
      enabled: true,
      current_version: "0.1.20",
      arch: "amd64",
      latest_version: "0.1.20",
      update_available: false,
      job: { running: false, phase: "idle", progress: 0 },
    });
    await flush();
    assert.equal(reloaded, "0.1.20");
    assert.equal(portal.getState().update.reloading, true);
  });
});
