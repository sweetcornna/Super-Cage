// 真实浏览器端到端测试：无头 Chrome + CDP（Node 内置 WebSocket，无 npm 依赖）对接仅测试用 mock。
// 覆盖 CSP、跨源 iframe 票据流程、旧 iframe 释放流、键盘操作、刷新恢复、手机尺寸、深浅色、错误恢复、登出与账户隔离。
// 找不到 Chrome 时跳过；可用 CAGE_TEST_BROWSER_PATH 指定浏览器，TENANT_SMOKE_SHOTS=<目录> 保存截图。
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { startMockServer } from "./mock-server.mjs";

const PASSWORD = "correct horse battery";
const SANDBOX = "allow-scripts allow-same-origin allow-forms allow-downloads";
const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function findChrome() {
  return (
    [
      process.env.CAGE_TEST_BROWSER_PATH,
      "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
      "/Applications/Chromium.app/Contents/MacOS/Chromium",
      "/usr/bin/google-chrome",
      "/usr/bin/google-chrome-stable",
      "/usr/bin/chromium",
      "/usr/bin/chromium-browser",
    ].find((path) => path && existsSync(path)) || null
  );
}

async function launchChrome(executable) {
  const profile = await mkdtemp(join(tmpdir(), "cage-tenant-chrome-"));
  const child = spawn(
    executable,
    [
      "--headless=new",
      "--remote-debugging-port=0",
      `--user-data-dir=${profile}`,
      "--no-first-run",
      "--no-default-browser-check",
      "--disable-gpu",
      "--disable-extensions",
      "--disable-background-networking",
      "--disable-component-update",
      "--disable-sync",
      "--mute-audio",
      "--window-size=1280,800",
      "about:blank",
    ],
    { stdio: ["ignore", "ignore", "pipe"] },
  );
  const endpoint = await new Promise((resolve, reject) => {
    let output = "";
    const timer = setTimeout(() => reject(new Error(`Chrome 启动超时\n${output}`)), 20000);
    child.stderr.on("data", (chunk) => {
      output += chunk;
      const match = /DevTools listening on (ws:\/\/\S+)/.exec(output);
      if (match) {
        clearTimeout(timer);
        resolve(match[1]);
      }
    });
    child.once("exit", (code) => {
      clearTimeout(timer);
      reject(new Error(`Chrome 意外退出（${code}）\n${output}`));
    });
  });
  return {
    endpoint,
    async close() {
      child.kill("SIGKILL");
      await rm(profile, { recursive: true, force: true }).catch(() => {});
    },
  };
}

function connect(url) {
  const socket = new WebSocket(url);
  const pending = new Map();
  const listeners = new Set();
  let seq = 0;
  socket.addEventListener("message", (event) => {
    const message = JSON.parse(event.data);
    if (message.id) {
      const entry = pending.get(message.id);
      if (!entry) return;
      pending.delete(message.id);
      if (message.error) entry.reject(new Error(`${entry.method}：${message.error.message}`));
      else entry.resolve(message.result);
      return;
    }
    for (const listener of listeners) listener(message);
  });
  return new Promise((resolve, reject) => {
    socket.addEventListener("error", () => reject(new Error("无法连接 CDP")));
    socket.addEventListener("open", () =>
      resolve({
        send(method, params = {}, sessionId) {
          const id = ++seq;
          socket.send(JSON.stringify({ id, method, params, ...(sessionId ? { sessionId } : {}) }));
          return new Promise((res, rej) => pending.set(id, { resolve: res, reject: rej, method }));
        },
        on(listener) {
          listeners.add(listener);
        },
        close() {
          socket.close();
        },
      }),
    );
  });
}

const KEYS = {
  Enter: { key: "Enter", code: "Enter", windowsVirtualKeyCode: 13, text: "\r" },
  Tab: { key: "Tab", code: "Tab", windowsVirtualKeyCode: 9 },
  Escape: { key: "Escape", code: "Escape", windowsVirtualKeyCode: 27 },
  ArrowLeft: { key: "ArrowLeft", code: "ArrowLeft", windowsVirtualKeyCode: 37 },
  ArrowRight: { key: "ArrowRight", code: "ArrowRight", windowsVirtualKeyCode: 39 },
  ArrowDown: { key: "ArrowDown", code: "ArrowDown", windowsVirtualKeyCode: 40 },
  Space: { key: " ", code: "Space", windowsVirtualKeyCode: 32, text: " " },
};

// 打开一个新标签页并返回绑定到它的操作助手。同一浏览器上下文中的标签页共享 Cookie 与 BroadcastChannel。
async function openPage(cdp, shots, prefix = "") {
  const { targetId } = await cdp.send("Target.createTarget", { url: "about:blank" });
  const { sessionId } = await cdp.send("Target.attachToTarget", { targetId, flatten: true });
  const send = (method, params) => cdp.send(method, params, sessionId);
  const problems = [];
  cdp.on((message) => {
    if (message.sessionId !== sessionId) return;
    if (message.method === "Runtime.exceptionThrown") {
      const details = message.params.exceptionDetails;
      problems.push(`未捕获异常：${details.exception?.description || details.text}`);
    }
    if (message.method === "Log.entryAdded") {
      const entry = message.params.entry;
      if (entry.source === "security" || /Content Security Policy|Refused to/i.test(entry.text)) problems.push(`安全日志：${entry.text}`);
    }
  });
  await send("Page.enable");
  await send("Runtime.enable");
  await send("Log.enable");
  await send("Page.addScriptToEvaluateOnNewDocument", {
    source: "window.__csp = []; document.addEventListener('securitypolicyviolation', (e) => window.__csp.push(e.violatedDirective + ' ' + e.blockedURI));",
  });

  const evaluate = async (expression) => {
    const result = await send("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true, userGesture: true });
    if (result.exceptionDetails) throw new Error(`页面脚本出错：${result.exceptionDetails.exception?.description || result.exceptionDetails.text}\n${expression}`);
    return result.result.value;
  };
  const waitFor = async (expression, label, timeout = 15000) => {
    const end = Date.now() + timeout;
    for (;;) {
      const value = await evaluate(expression);
      if (value) return value;
      if (Date.now() > end) {
        const snapshot = await evaluate(`JSON.stringify({ dialog: document.querySelector("dialog")?.open, options: document.querySelectorAll(".system-option").length, overlay: document.querySelector(".overlay")?.textContent.slice(0, 80), ctx: document.querySelector(".contextbar")?.textContent.slice(0, 80), csp: window.__csp })`).catch(() => "");
        throw new Error(`等待超时：${label}\n页面：${snapshot}\n问题：${problems.join(" | ")}`);
      }
      await delay(40);
    }
  };
  const waitUntil = async (check, label, timeout = 15000) => {
    const end = Date.now() + timeout;
    for (;;) {
      if (check()) return;
      if (Date.now() > end) throw new Error(`等待超时：${label}`);
      await delay(40);
    }
  };
  const press = async (name) => {
    const key = KEYS[name];
    await send("Input.dispatchKeyEvent", { type: key.text ? "keyDown" : "rawKeyDown", ...key });
    await send("Input.dispatchKeyEvent", { type: "keyUp", key: key.key, code: key.code, windowsVirtualKeyCode: key.windowsVirtualKeyCode });
  };
  const type = (text) => send("Input.insertText", { text });
  const focus = (selector) =>
    evaluate(`(() => { const el = document.querySelector(${JSON.stringify(selector)}); if (!el) throw new Error(${JSON.stringify(`找不到 ${selector}`)}); el.focus(); return true; })()`);
  const click = (selector, text = null) =>
    evaluate(`(() => {
      const list = [...document.querySelectorAll(${JSON.stringify(selector)})];
      const el = ${text === null ? "list[0]" : `list.find((item) => item.textContent.includes(${JSON.stringify(text)}))`};
      if (!el) throw new Error(${JSON.stringify(`找不到 ${selector} ${text ?? ""}`)});
      el.click();
      return true;
    })()`);
  const active = () => evaluate(`(() => { const el = document.activeElement; return el ? { tag: el.tagName, type: el.type || "", key: el.dataset.focusKey || "", name: el.name || "", text: el.textContent.trim() } : null; })()`);
  const shot = async (name) => {
    if (!shots) return;
    const { data } = await send("Page.captureScreenshot", { format: "png" });
    await writeFile(join(shots, `${prefix}${name}.png`), Buffer.from(data, "base64"));
  };
  const selectedVm = () => evaluate(`document.querySelector('.tab[aria-selected="true"]')?.dataset.focusKey.slice(4) || null`);
  const frameInfo = () =>
    evaluate(`(() => { const f = document.querySelector("iframe"); return f ? { count: document.querySelectorAll("iframe").length, sandbox: f.getAttribute("sandbox"), referrer: f.getAttribute("referrerpolicy"), allow: f.getAttribute("allow"), src: f.getAttribute("src"), title: f.title } : null; })()`);
  const createVm = async (name) => {
    await waitFor(`document.querySelector("dialog")?.open`, "新建弹窗");
    await focus('dialog input[name="vm-name"]');
    await type(name);
    await press("Enter");
    await waitFor(`!document.querySelector("dialog").open`, "弹窗关闭");
  };

  const close = () => cdp.send("Target.closeTarget", { targetId });
  return { send, problems, evaluate, waitFor, waitUntil, press, type, focus, click, active, shot, selectedVm, frameInfo, createVm, close };
}

const chrome = findChrome();

test(
  "浏览器端到端：注册、只填名称、多 VM 切换、键盘、刷新恢复、手机尺寸、错误恢复、登出与隔离",
  { skip: chrome ? false : "未找到 Chrome/Chromium（可设置 CAGE_TEST_BROWSER_PATH）", timeout: 180000 },
  async () => {
    const mock = await startMockServer({ bootDelayMs: 1200 });
    const browser = await launchChrome(chrome);
    const cdp = await connect(browser.endpoint);
    const shots = process.env.TENANT_SMOKE_SHOTS || "";
    if (shots) await mkdir(shots, { recursive: true });
    try {
      const page = await openPage(cdp, shots);
      const { send, problems, evaluate, waitFor, waitUntil, press, type, focus, click, active, shot, selectedVm, frameInfo, createVm } = page;

      /* ---- 登录页与注册（键盘） ---- */
      await send("Page.navigate", { url: `${mock.origin}/` });
      await waitFor(`!!document.querySelector(".auth-card")`, "登录页");
      assert.equal(await evaluate("document.title"), "Super Cage 门户");
      assert.equal((await active()).name, "username", "初始焦点在用户名");
      await click(".seg", "注册");
      await waitFor(`document.querySelector(".auth-title").textContent === "创建门户账户"`, "注册模式");
      await focus('input[name="username"]');
      await type("Alice");
      await focus('input[name="password"]');
      await type(PASSWORD);
      await focus('input[name="confirm"]');
      await type("mismatch password");
      await press("Enter");
      await waitFor(`document.activeElement?.name === "confirm" && document.activeElement.getAttribute("aria-invalid") === "true"`, "确认密码错误并聚焦");
      await evaluate(`document.querySelector('input[name="confirm"]').select()`);
      await type(PASSWORD);
      await press("Enter");
      await waitFor(`!!document.querySelector(".shell")`, "工作台外壳");
      await waitFor(`document.querySelector(".overlay h2")?.textContent === "创建你的第一台虚拟机"`, "空状态");
      assert.equal(await evaluate(`document.querySelector(".user-label").textContent`), "alice");
      assert.equal((await active()).key, "empty-create", "首次登录且无 VM 时焦点落在新建按钮");
      await shot("01-empty");

      /* ---- 新建第一台：名称验证与键盘提交 ---- */
      await click(".overlay button", "新建虚拟机");
      await waitFor(`document.querySelector("dialog").open && document.activeElement?.name === "vm-name"`, "焦点在名称输入框");
      assert.equal(await evaluate(`document.querySelectorAll("dialog input").length`), 1);
      await press("Enter");
      await waitFor(`document.activeElement.getAttribute("aria-invalid") === "true"`, "名称必填提示");
      await shot("02-create-dialog");
      await createVm("开发环境");
      const vmA = await selectedVm();
      assert.match(vmA, /^[0-9a-f]{32}$/);
      await waitFor(`document.querySelector(".overlay h2")?.textContent.includes("正在开机")`, "开机等待");
      assert.equal(await evaluate(`document.querySelector('.tab[aria-selected="true"] .tab-name').textContent`), "开发环境");
      assert.equal(await evaluate(`document.querySelector(".ctx-system").textContent`), "Debian 12");
      assert.match(await evaluate(`document.querySelector(".overlay .elapsed").textContent`), /^已等待 \d+:\d{2}$/);
      await shot("03-starting");

      await waitFor(`!!document.querySelector("iframe")`, "iframe 挂载", 20000);
      let frame = await frameInfo();
      assert.equal(frame.count, 1);
      assert.equal(frame.sandbox, SANDBOX);
      assert.equal(frame.referrer, "no-referrer");
      assert.equal(new URL(frame.src).origin, mock.vmOrigin(vmA));
      assert.equal(new URL(frame.src).pathname, "/_tenant/enter");
      assert.equal(frame.title, "工作台：开发环境");
      assert.equal(frame.allow, `clipboard-read ${mock.vmOrigin(vmA)}; clipboard-write ${mock.vmOrigin(vmA)}`, "剪贴板只委托给该 VM 源");
      await waitUntil(() => mock.openStreams(vmA) === 1, "VM A 工作台经票据进入并建立流");
      await waitFor(`document.querySelector(".ctx-status").textContent === "已连接" && document.querySelector(".overlay").hidden`, "已连接");
      assert.equal(await evaluate("document.title"), "开发环境 · Super Cage");
      await shot("04-workbench-light");

      /* ---- 新建第二台：弹窗重新开始；旧 iframe 立即卸载并释放流 ---- */
      await click(".add-btn");
      await waitFor(`document.querySelector("dialog").open && document.activeElement?.name === "vm-name"`, "第二次弹窗");
      assert.equal(await evaluate(`document.querySelector('dialog input[name="vm-name"]').value`), "");
      await focus('dialog input[name="vm-name"]');
      await type("测试环境");
      await click('dialog button[type="submit"]');
      await waitFor(`document.querySelectorAll(".tab").length === 2 && document.querySelector('.tab[aria-selected="true"]')?.textContent.includes("测试环境")`, "第二台被选中");
      const vmB = await selectedVm();
      assert.notEqual(vmB, vmA);
      assert.equal(await evaluate(`document.querySelectorAll("iframe").length`), 0, "切换后旧 iframe 已卸载");
      await waitUntil(() => mock.openStreams(vmA) === 0, "旧 iframe 的流已释放", 5000);

      /* ---- 启动中用键盘切回第一台；第二台在服务端继续启动 ---- */
      await waitFor(`document.querySelector(".ctx-status").textContent === "开机中"`, "第二台开机中");
      await focus('.tab[aria-selected="true"]');
      await press("ArrowLeft");
      assert.equal((await active()).key, `tab-${vmA}`, "方向键在切换器中移动焦点");
      await press("Enter");
      await waitFor(`document.querySelector('.tab[aria-selected="true"]')?.dataset.focusKey === "tab-${vmA}"`, "切回第一台");
      await waitFor(`(() => { const f = document.querySelector("iframe"); return !!f && new URL(f.src).origin === ${JSON.stringify(mock.vmOrigin(vmA))}; })()`, "第一台 iframe", 10000);
      await waitUntil(() => mock.openStreams(vmA) === 1, "第一台流重新建立");
      await waitUntil(() => mock.state.vms.get(vmB).state === "ready", "第二台在后台继续启动至就绪", 10000);
      const polls = () => mock.state.requests.filter((r) => r.method === "GET" && r.path === "/api/tenant/vms").length;
      const opens = () => mock.state.requests.filter((r) => r.method === "POST" && r.path.endsWith("/open")).length;
      await waitFor(`document.querySelector(".ctx-status").textContent === "已连接"`, "第一台已连接");
      const pollsBefore = polls();
      const opensBefore = opens();
      await delay(4500);
      assert.equal(polls(), pollsBefore, "目标就绪后不再轮询，也不轮询其他 VM");
      assert.equal(opens(), opensBefore, "不重复提交 open");

      /* ---- 刷新：从当前账户列表重新验证后恢复所选 VM ---- */
      const servedBefore = mock.state.served.filter((item) => item.vmId === vmA).length;
      await send("Page.reload");
      await waitFor(`!!document.querySelector(".shell")`, "刷新后外壳");
      await waitFor(`document.querySelector('.tab[aria-selected="true"]')?.dataset.focusKey === "tab-${vmA}"`, "刷新后恢复所选 VM");
      await waitUntil(() => mock.state.served.filter((item) => item.vmId === vmA).length > servedBefore, "刷新后以新票据重新进入");
      const stored = await evaluate(`JSON.stringify({ local: { ...localStorage }, session: { ...sessionStorage } })`);
      for (const forbidden of ["alice", "Alice", "_tenant", "http", PASSWORD, ...mock.state.issuedTickets]) {
        assert.equal(stored.includes(forbidden), false, `浏览器存储不应包含 ${forbidden.slice(0, 12)}`);
      }
      assert.deepEqual(Object.keys(JSON.parse(stored).session), ["cage-tenant.selected-vm.v1"]);

      /* ---- 跳转链接与主题 ---- */
      assert.equal(
        await evaluate(`document.querySelector(".shell").querySelector("button, [href], input, [tabindex]:not([tabindex='-1'])").textContent`),
        "跳到虚拟机切换",
        "跳转链接是外壳中第一个可聚焦元素",
      );
      await focus(".skip-link");
      assert.ok(await evaluate(`document.querySelector(".skip-link").getBoundingClientRect().top >= 0`), "聚焦时可见");
      await press("Enter");
      assert.equal((await active()).key, `tab-${vmA}`, JSON.stringify(await evaluate(`(() => { const el = document.activeElement; return { tag: el?.tagName, cls: el?.className, text: el?.textContent.slice(0, 40), selected: document.querySelector('.tab[aria-selected="true"]')?.dataset.focusKey }; })()`)));
      await click('.account [data-focus-key="theme"]');
      await click('.account [data-focus-key="theme"]');
      assert.equal(await evaluate(`document.documentElement.dataset.theme`), "dark");
      await waitFor(`document.querySelector(".ctx-status").textContent === "已连接"`, "深色下已连接");
      await shot("05-workbench-dark");

      /* ---- 手机尺寸 ---- */
      await send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
      await send("Emulation.setTouchEmulationEnabled", { enabled: true, maxTouchPoints: 5 });
      await delay(300);
      const layout = await evaluate(`(() => {
        const rect = (s) => {
          const r = document.querySelector(s)?.getBoundingClientRect();
          return r ? { left: r.left, right: r.right, top: r.top, bottom: r.bottom, width: r.width, height: r.height } : null;
        };
        return {
          scroll: document.documentElement.scrollWidth,
          width: innerWidth,
          tab: rect('.tab[aria-selected="true"]'),
          add: rect(".add-btn"),
          logout: rect(".logout-btn"),
          frame: rect("iframe"),
          tabHeight: rect(".tab").height,
        };
      })()`);
      assert.ok(layout.scroll <= layout.width, `无横向滚动（${layout.scroll} > ${layout.width}）`);
      assert.ok(layout.tab.right <= layout.width && layout.tab.left >= 0, `当前 VM 按钮在视口内 ${JSON.stringify(layout)}`);
      assert.ok(layout.add.right <= layout.width && layout.logout.right <= layout.width, "新建与退出按钮可见");
      assert.ok(layout.frame.height > 600, "工作台占满剩余高度");
      assert.ok(layout.tabHeight >= 36, "触控尺寸的切换按钮");
      await shot("06-mobile-dark");
      await click(".add-btn");
      await waitFor(`document.querySelector("dialog").open`, "手机上的新建弹窗");
      // On a phone the dialog is a bottom sheet: full width, resting on the bottom edge.
      await waitFor(`(() => { const box = document.querySelector("dialog").getBoundingClientRect(); return box.width <= innerWidth && Math.abs(box.bottom - innerHeight) <= 1; })()`, "手机上的新建弹窗贴底", 3000);
      await shot("07-mobile-create");
      await press("Escape");
      await waitFor(`!document.querySelector("dialog").open`, "Esc 关闭弹窗");
      // 回归：关闭后在同一任务内立即重新打开，上一次关闭异步派发的 close 事件不得关掉新会话。
      await evaluate(`(() => {
        document.querySelector(".add-btn").click();
        document.querySelector("dialog").dispatchEvent(new Event("cancel", { cancelable: true }));
        document.querySelector(".add-btn").click();
        return document.querySelector("dialog").open;
      })()`);
      await delay(300);
      assert.equal(await evaluate(`document.querySelector("dialog").open`), true, "过时的 close 事件不会关闭重新打开的弹窗");
      await waitFor(`document.activeElement?.name === "vm-name"`, "重新打开后聚焦名称");
      await press("Escape");
      await waitFor(`!document.querySelector("dialog").open`, "再次 Esc 关闭");

      await click('.account [data-focus-key="theme"]');
      await send("Emulation.setTouchEmulationEnabled", { enabled: false });
      await send("Emulation.clearDeviceMetricsOverride");

      /* ---- 启动失败与手动重试 ---- */
      await waitUntil(() => [...mock.state.vms.values()].every((vm) => vm.state !== "starting"), "无进行中的启动");
      mock.failNextBoot("*", "所选系统镜像缺失，请稍后重试。");
      await click(".add-btn");
      await createVm("故障演练");
      await waitFor(`document.querySelector(".overlay .error-text")?.textContent === "所选系统镜像缺失，请稍后重试。"`, "启动失败提示", 20000);
      assert.match(await evaluate(`document.querySelector(".overlay h2").textContent`), /启动失败/);
      assert.equal(await evaluate(`document.querySelector(".ctx-status").textContent`), "无法打开");
      await shot("08-error");
      await click(".overlay button", "重试");
      await waitFor(`!!document.querySelector("iframe")`, "重试后载入", 20000);

      /* ---- 总览与配额 ---- */
      await click(".home-btn");
      await waitFor(`document.querySelectorAll(".vm-card").length === 3`, "总览");
      assert.equal(await evaluate(`document.querySelectorAll("iframe").length`), 0);
      assert.equal(await evaluate(`document.querySelector(".home-quota").textContent`), "已创建 3 / 5 台");
      await shot("09-overview");
      await click(".vm-card", "测试环境");
      await waitFor(`(() => { const f = document.querySelector("iframe"); return !!f && new URL(f.src).origin === ${JSON.stringify(mock.vmOrigin(vmB))}; })()`, "从总览打开第二台", 10000);

      /* ---- 登出：立即卸载 iframe，服务端断开流 ---- */
      const framesAfterClick = await evaluate(`(() => { document.querySelector(".logout-btn").click(); return document.querySelectorAll("iframe").length; })()`);
      assert.equal(framesAfterClick, 0, "点击退出时同步卸载 iframe");
      await waitFor(`!!document.querySelector(".auth-card")`, "回到登录页");
      assert.equal(await evaluate(`document.querySelector(".auth-notice").textContent`), "已安全退出。");
      await waitUntil(() => mock.openStreams() === 0, "登出后所有流断开");

      /* ---- 跨标签同步：另一页退出 / 以新账户登录，本页随之更新 ---- */
      await focus('input[name="username"]');
      await type("alice");
      await focus('input[name="password"]');
      await type(PASSWORD);
      await press("Enter");
      await waitFor(`(() => { const f = document.querySelector("iframe"); return !!f && new URL(f.src).origin === ${JSON.stringify(mock.vmOrigin(vmB))}; })()`, "重新登录后恢复所选 VM", 10000);
      const tab2 = await openPage(cdp, shots, "tab2-");
      await tab2.send("Page.navigate", { url: `${mock.origin}/` });
      await tab2.waitFor(`document.querySelector(".user-label")?.textContent === "alice"`, "第二个标签页共享登录");
      await tab2.waitFor(`!!document.querySelector("iframe")`, "第二个标签页打开工作台", 10000);
      await tab2.click(".logout-btn");
      await tab2.waitFor(`!!document.querySelector(".auth-card")`, "第二个标签页退出");
      await waitFor(`!!document.querySelector(".auth-card")`, "本页随之回到登录页", 5000);
      assert.match(await evaluate(`document.querySelector(".auth-notice").textContent`), /其他标签页退出/);
      assert.equal(await evaluate(`document.querySelectorAll("iframe").length`), 0);
      await waitUntil(() => mock.openStreams() === 0, "两个标签页的流都已断开");

      /* ---- 第二个账户（在另一标签页注册）无法取得第一个账户的 VM ---- */
      await tab2.click(".seg", "注册");
      await tab2.focus('input[name="username"]');
      await tab2.type("bob");
      await tab2.focus('input[name="password"]');
      await tab2.type(PASSWORD);
      await tab2.focus('input[name="confirm"]');
      await tab2.type(PASSWORD);
      await tab2.press("Enter");
      await tab2.waitFor(`document.querySelector(".overlay h2")?.textContent === "创建你的第一台虚拟机"`, "bob 空状态（第二个标签页）");
      await waitFor(`document.querySelector(".user-label")?.textContent === "bob"`, "本页同步为 bob", 5000);
      assert.match(await evaluate(`document.querySelector(".banner").textContent`), /已在其他标签页登录为 bob/);
      await waitFor(`document.querySelector(".overlay h2")?.textContent === "创建你的第一台虚拟机"`, "bob 空状态");
      assert.equal(await evaluate(`document.querySelectorAll(".tab").length`), 0);
      assert.equal(await evaluate(`document.querySelectorAll("iframe").length`), 0, "不恢复其他账户的所选 VM");
      const steal = await evaluate(`fetch("/api/tenant/vms/${vmA}/open", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }).then(async (r) => ({ status: r.status, body: await r.json() }))`);
      assert.equal(steal.status, 404);
      assert.equal(steal.body.error.code, "vm_not_found");
      await shot("10-cross-tab");
      assert.deepEqual(await tab2.evaluate("window.__csp"), [], "第二个标签页无 CSP 违规");
      assert.deepEqual(tab2.problems, [], "第二个标签页无异常");
      await tab2.close();

      /* ---- 注册关闭：清楚提示并可返回登录 ---- */
      mock.setRegistrationOpen(false);
      await click(".logout-btn");
      await waitFor(`!!document.querySelector(".auth-card")`, "bob 退出");
      await click(".seg", "注册");
      await focus('input[name="username"]');
      await type("carol");
      await focus('input[name="password"]');
      await type(PASSWORD);
      await focus('input[name="confirm"]');
      await type(PASSWORD);
      await press("Enter");
      await waitFor(`!document.querySelector(".closed-panel").hidden && document.querySelector(".auth-form").hidden`, "注册关闭提示");
      assert.match(await evaluate(`document.querySelector(".closed-panel").textContent`), /暂未开放自助注册/);
      assert.equal((await active()).key, "back-to-login", "焦点移到返回登录");
      await shot("11-registration-closed");
      await press("Enter");
      await waitFor(`document.querySelector(".auth-title").textContent === "登录 Super Cage 门户" && !document.querySelector(".auth-form").hidden`, "返回登录");
      assert.equal((await active()).name, "username");
      await click(".seg", "注册");
      assert.equal(await evaluate(`document.querySelector(".closed-panel").hidden`), false, "本页内不再显示注册表单");
      assert.equal(mock.state.requests.filter((r) => r.path === "/api/tenant/register" && r.method === "POST").length, 3, "注册关闭后只尝试过一次");
      mock.setRegistrationOpen(true);

      /* ---- 安全约束汇总 ---- */
      assert.deepEqual(await evaluate("window.__csp"), [], "当前页面无 CSP 违规");
      assert.deepEqual(problems, [], "无未捕获异常或安全日志");
      assert.ok(mock.state.issuedTickets.length >= 5);
      for (const record of mock.state.requests) {
        for (const ticket of mock.state.issuedTickets) {
          assert.equal(record.path.includes(ticket), false, "票据不出现在请求路径");
          assert.equal((record.referer || "").includes(ticket), false, "票据不出现在 Referer");
        }
        if (record.host.startsWith("vm-") && record.path.startsWith("/_tenant/enter") && record.method === "GET") {
          assert.equal(record.referer, null, "iframe 导航不带 Referer");
        }
        if (record.method === "POST" && record.host === mock.portalHost) {
          assert.equal(record.origin, mock.origin, "门户 POST 携带精确 Origin");
        }
      }
    } finally {
      cdp.close();
      await browser.close();
      await mock.close();
    }
  },
);
