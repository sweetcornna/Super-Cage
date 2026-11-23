// Walks the real phone UI (the build the app bundles) through the example workspace's script in headless Chrome and
// fails on any uncaught error, console error, unanswered request, request to another origin or unhandled path.
//
//   node mobile/build-ui.mjs   # once, after: cd ui && npm ci
//   CAGE_PLAYWRIGHT_MODULE=<playwright> CAGE_TEST_BROWSER_PATH=<chrome> node mobile/scripts/example-workspace-check.mjs [--idle-seconds=N] [--shots=DIR] [--no-idle] [--keep-awake]
//
// The script is taken from mobile/ios/CageMobile/ExampleWorkspace.swift (the raw string after "static let fixture"),
// injected at document start after the device list, exactly as the app does. Chrome stands in for WKWebView: it cannot
// serve the app's cage-preview scheme, so the page is plain http on 127.0.0.1 (ports 5350-5359) and the phone UI's own
// "preview scheme" branch (remote-browser.jsx) is off; the script's answer for browser and desktop requests is checked instead.
import assert from "node:assert/strict";
import { createReadStream, existsSync, mkdirSync, readFileSync, statSync } from "node:fs";
import { createServer } from "node:http";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { extname, join, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

const require = createRequire(import.meta.url);
const root = fileURLToPath(new URL("../../", import.meta.url));
const args = Object.fromEntries(process.argv.slice(2).map(a => a.replace(/^--/, "").split("=")).map(([k, v]) => [k, v ?? "1"]));
const idleSeconds = Number(args["idle-seconds"] || 95);
const shots = resolve(args.shots || process.env.CAGE_EXAMPLE_SHOTS || join(tmpdir(), "example-workspace-shots"));
mkdirSync(shots, { recursive: true });

// ---- the script, from the Swift source
const swift = readFileSync(join(root, "mobile/ios/CageMobile/ExampleWorkspace.swift"), "utf8").split("\n");
const open = swift.findIndex(line => /static let fixture = #"""\s*$/.test(line));
const close = swift.findIndex((line, index) => index > open && line.trim() === '"""#');
assert.ok(open >= 0 && close > open, "the fixture string was not found in ExampleWorkspace.swift");
const indent = swift[close].match(/^\s*/)[0];
const fixture = swift.slice(open + 1, close).map(line => line.startsWith(indent) ? line.slice(indent.length) : line.trim() === "" ? "" : (assert.fail("a fixture line is indented less than the closing delimiter"))).join("\n");
const swiftSource = swift.join("\n");
const deviceName = swiftSource.match(/static let deviceName = "([^"]+)"/)[1];
const deviceID = swiftSource.match(/static let deviceID = "([^"]+)"/)[1];
const SORRY = "示例里不能这样做，连接自己的 Super Cage 后可用";
assert.ok(fixture.includes(SORRY) && deviceName === "示例工作台 · 演示数据");

// ---- the built phone UI, served as the app serves it
const ui = join(root, "mobile/.build/resources/mobile-ui");
assert.ok(existsSync(join(ui, "mobile.html")), "build the phone UI first: node mobile/build-ui.mjs (after: cd ui && npm ci)");
const types = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".png": "image/png", ".svg": "image/svg+xml", ".woff2": "font/woff2" };
const served = [];
const server = createServer((request, response) => {
  const path = decodeURIComponent(new URL(request.url, "http://x").pathname);
  served.push(path);
  const file = resolve(ui, path === "/" ? "mobile.html" : "." + path);
  if (!file.startsWith(ui + sep) || !existsSync(file) || !statSync(file).isFile()) { response.writeHead(404).end(); return; }
  response.writeHead(200, { "Content-Type": types[extname(file)] || "application/octet-stream" });
  createReadStream(file).pipe(response);
});
let port = 0;
for (const candidate of [5350, 5351, 5352, 5353, 5354, 5355, 5356, 5357, 5358, 5359]) {
  const ok = await new Promise(done => { server.once("error", () => done(false)); server.listen(candidate, "127.0.0.1", () => done(true)); });
  if (ok) { port = candidate; break; }
}
assert.ok(port, "no free port in 5350-5359");
const origin = "http://127.0.0.1:" + port;

// ---- the browser
const engines = require(process.env.CAGE_PLAYWRIGHT_MODULE || "playwright");
const browser = await engines.chromium.launch({ executablePath: process.env.CAGE_TEST_BROWSER_PATH || undefined });
const problems = [];
const note = (message) => { problems.push(message); console.log("PROBLEM", message); };

// What the app puts in the page before the script: the device list and the bridge. The hidden calls make the page
// behave as one on a custom scheme, where the secure-context crypto calls may be missing.
const prelude = hide => `
  window.__bridge = [];
  window.webkit = { messageHandlers: { cageDevices: { postMessage: message => window.__bridge.push(message) } } };
  window.__cageDevices = { current: ${JSON.stringify(deviceID)}, devices: [{ id: ${JSON.stringify(deviceID)}, name: ${JSON.stringify(deviceName)}, kind: "computer" }] };
  if (${hide}) {
    Object.defineProperty(crypto, "randomUUID", { configurable: true, writable: true, value: undefined });
    Object.defineProperty(crypto, "subtle", { configurable: true, value: undefined });
  }
`;
const recorder = `
  (() => {
    const inner = window.fetch;
    window.__net = [];
    window.fetch = (input, options = {}) => {
      const url = new URL(typeof input === "string" ? input : input.url, location.href);
      const entry = { method: (options.method || "GET").toUpperCase(), path: url.pathname, query: url.search, start: performance.now(), end: 0, status: 0, unhandled: false, aborted: false, error: "", body: options.body ? String(options.body).slice(0, 200) : "" };
      window.__net.push(entry);
      return inner(input, options).then(response => {
        entry.end = performance.now(); entry.status = response.status; entry.unhandled = response.headers.get("X-Example-Unhandled") === "1";
        return response;
      }, error => {
        entry.end = performance.now(); entry.aborted = error?.name === "AbortError" || options.signal?.aborted === true; entry.error = String(error?.message || error);
        throw error;
      });
    };
  })();
`;

async function newPage(hide = true) {
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true,
    userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 CageMobile/1" });
  await context.addInitScript({ content: prelude(hide) });
  await context.addInitScript({ content: fixture });
  await context.addInitScript({ content: recorder });
  const page = await context.newPage();
  lastPage = page;
  page.on("pageerror", error => note("uncaught error: " + error.message));
  page.on("console", message => { if (message.type() === "error") note("console error: " + message.text()); });
  page.on("requestfailed", request => { if (!/ERR_ABORTED/.test(request.failure()?.errorText || "")) note("request failed: " + request.url()); });
  page.on("request", request => {
    const url = new URL(request.url());
    if (url.protocol === "blob:" || url.protocol === "data:") return;
    if (url.origin !== origin) note("request to another origin: " + request.url());
    else if (url.pathname.startsWith("/api/")) note("an API request reached the network: " + url.pathname);
  });
  page.on("dialog", dialog => { note("browser dialog: " + dialog.message()); dialog.dismiss(); });
  return page;
}
const net = page => page.evaluate(() => window.__net);
const bridge = page => page.evaluate(() => window.__bridge);
let shotNumber = 0;
// Taken once the page has settled: a closing drawer or an opening sheet is waited out, except the drawer shot itself.
const shot = async (page, name, { opened = false } = {}) => {
  if (!opened) await page.waitForFunction(() => !document.querySelector("dialog.workspace-drawer")?.open);
  await page.waitForTimeout(600);
  await page.screenshot({ path: join(shots, String(++shotNumber).padStart(2, "0") + "-" + name + ".png") });
};
let lastPage = null;
const step = async (name, run) => {
  process.stdout.write("  " + name + " ... ");
  try { await run(); console.log("ok"); }
  catch (error) { console.log("FAILED"); await lastPage?.screenshot({ path: join(shots, "failed-" + name.replace(/\W+/g, "-") + ".png") }).catch(() => {}); throw error; }
};
const see = (page, text, options = {}) => page.getByText(text, { exact: false }).first().waitFor({ timeout: 8000, ...options });
const drawer = page => page.locator("dialog.workspace-drawer");
async function openDrawer(page) {
  // Either fully closed or fully open: a drawer on its way in or out is waited for.
  await page.waitForFunction(() => { const d = document.querySelector("dialog.workspace-drawer"); return !d.open || d.dataset.visible === "true"; });
  if (await drawer(page).evaluate(el => el.open)) return;
  await page.getByRole("button", { name: "展开侧栏" }).tap();
  await page.waitForFunction(() => document.querySelector("dialog.workspace-drawer")?.dataset.visible === "true");
}
const drawerClosed = page => page.waitForFunction(() => !document.querySelector("dialog.workspace-drawer")?.open);
async function tool(page, name) {
  await openDrawer(page);
  await drawer(page).getByRole("button", { name, exact: true }).tap();
}
async function openSession(page, title) {
  await openDrawer(page);
  await drawer(page).getByRole("button", { name: title, exact: true }).tap();
  await drawerClosed(page);
  await page.getByRole("textbox", { name: "消息", exact: true }).waitFor();
}
const start = async (hide = true) => {
  const page = await newPage(hide);
  await page.goto(origin + "/");
  await see(page, "今天想在");
  return page;
};

try {
console.log("example workspace walk, " + origin + ", device name " + deviceName);
const page = await start();
const walkStart = Date.now();

await step("start screen", async () => {
  await see(page, "示例网站");
  await shot(page, "home");
  const names = (await bridge(page)).filter(m => m.action === "name");
  assert.ok(names.length > 0 && names.every(m => m.name === deviceName), "the page names the device " + JSON.stringify(names));
});
await step("home composer: type, send, the session is created and answered", async () => {
  const box = page.getByRole("textbox", { name: "消息", exact: true });
  await box.fill("先看看这个项目里有什么。");
  await page.getByRole("button", { name: "发送", exact: true }).tap();
  await page.locator('button[aria-label="复制回复"]').first().waitFor({ timeout: 8000 });
  await see(page, "先看看这个项目里有什么。");
  await shot(page, "home-composer");
});
await step("new conversation, then a recent row", async () => {
  await page.getByRole("button", { name: "新对话", exact: true }).first().tap();
  await see(page, "今天想在");
  await see(page, "先看看这个项目里有什么。");
  await page.locator(".recent").getByRole("button", { name: "把首页标题改得更醒目" }).tap();
  await see(page, "已经把首页标题调大");
});
await step("side panel shows the device name, projects and sessions", async () => {
  await openDrawer(page);
  assert.match(await drawer(page).locator(".drawer-brand small").innerText(), /示例工作台 · 演示数据/);
  for (const title of ["把首页标题改得更醒目", "运行测试（审批示例）"]) await drawer(page).getByRole("button", { name: title, exact: true }).waitFor();
  await shot(page, "drawer", { opened: true });
  await drawer(page).getByRole("button", { name: "项目 读书笔记", exact: true }).tap();
  await drawer(page).getByRole("button", { name: "整理本周读书计划", exact: true }).waitFor();
});
await step("open a session, expand its tool calls", async () => {
  await openSession(page, "把首页标题改得更醒目");
  await see(page, "已经把首页标题调大");
  await page.getByText("2 次工具调用").tap();
  await see(page, "styles.css");
  await shot(page, "session");
});
await step("send a message and read the canned reply", async () => {
  const box = page.getByRole("textbox", { name: "消息", exact: true });
  await box.fill("你好，请介绍一下这个示例。");
  await page.getByRole("button", { name: "发送", exact: true }).tap();
  await see(page, "正在思考");
  await page.waitForFunction(() => !document.querySelector(".thinking") && document.querySelectorAll('button[aria-label="复制回复"]').length >= 2, null, { timeout: 8000 });
  await shot(page, "reply");
});
await step("stop a running reply", async () => {
  await page.getByRole("textbox", { name: "消息", exact: true }).fill("再来一条，然后停止它。");
  await page.getByRole("button", { name: "发送", exact: true }).tap();
  await page.getByRole("button", { name: "停止运行", exact: true }).tap({ timeout: 1500 });
  await see(page, "已停止");
});
await step("attach a file to a message", async () => {
  await page.locator(".composer input[type=file]").setInputFiles({ name: "示例图片.txt", mimeType: "text/plain", buffer: Buffer.from("示例内容") });
  await see(page, "示例图片.txt");
  await page.waitForFunction(() => document.querySelector(".attachment.done"));
  await page.getByRole("textbox", { name: "消息", exact: true }).fill("请看附件。");
  await page.getByRole("button", { name: "发送", exact: true }).tap();
  await see(page, ".super-cage/attachments");
  await page.waitForFunction(() => document.querySelectorAll('button[aria-label="复制回复"]').length >= 3, null, { timeout: 8000 });
});
await step("attach a picture, send it, open it inline", async () => {
  const replies = () => page.locator('button[aria-label="复制回复"]').count();
  const count = await replies();
  const png = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==", "base64");
  await page.locator(".composer input[type=file]").setInputFiles({ name: "示例截图.png", mimeType: "image/png", buffer: png });
  await page.waitForFunction(() => document.querySelector(".attachment.done"));
  await page.getByRole("textbox", { name: "消息", exact: true }).fill("请看这张图。");
  await page.getByRole("button", { name: "发送", exact: true }).tap();
  const trigger = page.locator(".project-media button.media-trigger", { hasText: "示例截图.png" });
  await trigger.waitFor({ timeout: 8000 });
  await trigger.tap();
  await page.locator(".project-media img").waitFor({ timeout: 8000 });
  assert.equal(await page.locator(".media-error").count(), 0, "the picture was not shown");
  await page.waitForFunction(n => document.querySelectorAll('button[aria-label="复制回复"]').length > n, count, { timeout: 8000 });
  await shot(page, "picture");
});
await step("a picture that is too large for the example: a calm sentence", async () => {
  await page.locator(".composer input[type=file]").setInputFiles({ name: "大文件.bin", mimeType: "application/octet-stream", buffer: Buffer.alloc(3 << 20, 1) });
  await see(page, "示例里最多上传 2 MB");
  await page.getByRole("button", { name: /移除 大文件\.bin/ }).tap();
});
await step("approve a tool call", async () => {
  await openSession(page, "运行测试（审批示例）");
  await see(page, "允许使用 Bash？");
  await shot(page, "approval");
  await page.getByRole("button", { name: "允许一次", exact: true }).tap();
  await see(page, "12 个测试通过");
});
await step("new session from the side panel, rename, pin, archive", async () => {
  await openDrawer(page);
  await drawer(page).getByRole("button", { name: "在 读书笔记 新建会话", exact: true }).tap();
  await see(page, "发送第一条消息");
  await drawerClosed(page);
  const box = page.getByRole("textbox", { name: "消息", exact: true });
  await box.fill("列一份下周的读书清单");
  await page.getByRole("button", { name: "发送", exact: true }).tap();
  await page.locator('button[aria-label="复制回复"]').first().waitFor({ timeout: 8000 });
  await openDrawer(page);
  const title = "列一份下周的读书清单";
  const reveal = async name => { if (!(await drawer(page).getByRole("button", { name: "重命名", exact: true }).isVisible())) await drawer(page).getByRole("button", { name: name + "的更多操作", exact: true }).tap(); };
  await reveal(title);
  await drawer(page).getByRole("button", { name: "置顶", exact: true }).tap();
  await reveal(title);
  await drawer(page).getByRole("button", { name: "重命名", exact: true }).tap();
  await page.getByRole("dialog", { name: "重命名会话" }).locator("input").fill("下周书单");
  await page.getByRole("button", { name: "保存名称", exact: true }).tap();
  await drawer(page).getByRole("button", { name: "下周书单", exact: true }).waitFor();
  await reveal("下周书单");
  await drawer(page).getByRole("button", { name: "归档", exact: true }).tap();
  await page.getByRole("button", { name: "归档会话", exact: true }).tap();
  await drawer(page).getByRole("button", { name: "下周书单", exact: true }).waitFor({ state: "detached" });
});
await step("archived sessions: restore and delete", async () => {
  await openDrawer(page);
  await drawer(page).getByRole("button", { name: "设置", exact: true }).tap();
  await page.getByRole("button", { name: "已归档会话" }).tap();
  await see(page, "旧的实验记录");
  await see(page, "下周书单");
  await shot(page, "archived");
  await page.getByRole("button", { name: "取消归档", exact: true }).first().tap();
  await see(page, "已取消归档 1 个会话");
  await page.getByRole("button", { name: "删除会话", exact: true }).first().tap();
  await page.getByRole("button", { name: "永久删除", exact: true }).tap();
  await see(page, "已永久删除 1 个会话");
});
await step("terminals: open the sample, run commands, new terminal, close", async () => {
  await tool(page, "终端");
  await see(page, "终端 1");
  await shot(page, "terminals");
  await page.getByRole("button", { name: /终端 1/ }).first().tap();
  const host = page.locator(".terminal-host");
  await host.waitFor();
  await see(page, "示例终端");
  await host.tap();
  await page.keyboard.type("ls");
  await page.keyboard.press("Enter");
  await page.waitForFunction(() => /README\.md/.test(document.querySelector(".terminal-host")?.innerText || ""));
  await page.keyboard.type("cat styles.css");
  await page.keyboard.press("Enter");
  await page.waitForFunction(() => /max-width/.test(document.querySelector(".terminal-host")?.innerText || ""));
  await page.keyboard.type("rm -rf demo");
  await page.keyboard.press("Enter");
  await page.waitForFunction(() => /示例里不能这样做/.test(document.querySelector(".terminal-host")?.innerText || ""));
  await page.getByRole("button", { name: "Esc", exact: true }).tap();
  await shot(page, "terminal");
  await page.getByRole("button", { name: "返回终端列表" }).tap();
  await page.getByRole("button", { name: /Shell/ }).tap();
  await page.locator(".terminal-host").waitFor();
  await see(page, "示例终端");
  await page.getByRole("button", { name: "关闭终端" }).tap();
  await page.getByRole("button", { name: "关闭终端", exact: true }).last().tap();
  await see(page, "还没有打开的终端", { timeout: 1500 }).catch(() => {});
});
await step("files: browse, preview, export, new folder, upload", async () => {
  await openSession(page, "把首页标题改得更醒目");
  await tool(page, "文件");
  await see(page, "README.md");
  await shot(page, "files");
  await page.getByRole("button", { name: /^src/ }).tap();
  await page.getByRole("button", { name: /^main\.js/ }).tap();
  await see(page, "import { greet }");
  await page.getByRole("button", { name: "导出", exact: true }).tap();
  assert.ok((await bridge(page)).some(m => m.action === "share-file" && m.name === "main.js"), "export reached the bridge");
  await page.getByRole("button", { name: "关闭", exact: true }).tap();
  await page.getByRole("button", { name: "新建文件夹", exact: true }).tap();
  await page.getByLabel("名称").fill("草稿");
  await page.getByRole("button", { name: "创建", exact: true }).tap();
  await see(page, "草稿");
  await page.locator(".files-actions input[type=file]").setInputFiles({ name: "上传示例.txt", mimeType: "text/plain", buffer: Buffer.from("上传的内容") });
  await see(page, "上传示例.txt");
  await page.getByRole("button", { name: /^上传示例\.txt/ }).tap();
  await see(page, "上传的内容");
  await page.getByRole("button", { name: "关闭", exact: true }).tap();
  await page.getByRole("button", { name: "上一级" }).tap();
  await page.getByRole("button", { name: /^assets/ }).tap();
  await page.getByRole("button", { name: /^logo\.svg/ }).tap();
  await page.locator(".preview-image").waitFor();
  await shot(page, "image-preview");
  await page.getByRole("button", { name: "关闭", exact: true }).tap();
});
await step("environment: restart, stop, start, switch node", async () => {
  await tool(page, "环境");
  await see(page, "环境运行中");
  await shot(page, "environment");
  await page.getByRole("button", { name: "重启", exact: true }).tap();
  await page.getByRole("button", { name: "重启环境", exact: true }).tap();
  await see(page, "正在重启");
  await see(page, "环境运行中");
  await page.getByRole("button", { name: "停止", exact: true }).tap();
  await page.getByRole("button", { name: "停止环境", exact: true }).tap();
  await see(page, "环境已停止");
  await shot(page, "environment-stopped");
  await page.getByRole("button", { name: "启动环境", exact: true }).tap();
  await see(page, "环境运行中");
  await page.getByRole("button", { name: "切换节点", exact: true }).tap();
  await see(page, "示例节点 C");
  await page.getByRole("button", { name: "测速" }).tap();
  await see(page, "60 ms");
  await page.getByRole("button", { name: /示例节点 C/ }).tap();
  await page.getByRole("button", { name: "切换", exact: true }).tap();
  await see(page, "示例节点 C", { state: "visible" });
  await shot(page, "environment-node");
});
await step("remote browser, Claude.ai, Claude Desktop: a calm sentence", async () => {
  for (const name of ["浏览器", "Claude.ai", "Claude Desktop"]) {
    await tool(page, name);
    await see(page, SORRY);
    if (name === "浏览器") { await drawerClosed(page); await shot(page, "remote-browser"); }
    await page.getByRole("button", { name: /^(关闭远程浏览器|断开远程桌面)$/ }).tap();
  }
});
await step("settings page", async () => {
  await openDrawer(page);
  await drawer(page).getByRole("button", { name: "设置", exact: true }).tap();
  await see(page, "已归档会话");
  await shot(page, "settings");
});
await step("leave: the exit control tells the app to go home", async () => {
  await openDrawer(page);
  await drawer(page).getByRole("link", { name: "退出" }).tap();
  await page.waitForFunction(() => window.__bridge.some(m => m.action === "home"));
  assert.equal(new URL(page.url()).origin, origin, "the page did not navigate");
});
await step("a second visit: deny a tool call", async () => {
  const second = await start();
  await openSession(second, "运行测试（审批示例）");
  await second.getByRole("button", { name: "拒绝", exact: true }).tap();
  await see(second, "已拒绝这次调用");
  await second.context().close();
});

// ---- every request the walk made
const log = await net(page);
const unhandled = log.filter(r => r.unhandled);
if (unhandled.length) note("unhandled paths: " + [...new Set(unhandled.map(r => r.method + " " + r.path))].join(", "));
const polls = r => /^\/api\/mobile(\/cli\/sessions(\/[^/]+)?|\/environment|\/terminals|\/terminals\/[0-9a-f]{16}\/output|\/cli\/sessions\/[^/]+\/account)$/.test(r.path) && r.method !== "POST" || /\/account$/.test(r.path);
const slowOnes = log.filter(r => r.end && !r.aborted && r.end - r.start > 3000 && !polls(r));
if (slowOnes.length) note("requests that took over 3 s: " + slowOnes.map(r => r.method + " " + r.path + " " + Math.round(r.end - r.start)).join(", "));
const hanging = log.filter(r => !r.end && !polls(r));
if (hanging.length) note("requests with no answer: " + hanging.map(r => r.method + " " + r.path).join(", "));
const refused = log.filter(r => r.status === 422).map(r => r.method + " " + r.path);
console.log("walk: " + log.length + " requests in " + Math.round((Date.now() - walkStart) / 1000) + " s; refusals with the calm sentence: " + (refused.length ? refused.join(", ") : "none"));
await page.context().close();

// ---- idle: nothing is touched, how often does the page ask?
const scenarios = {
  "start screen": async p => {},
  "side panel open": async p => { await openDrawer(p); },
  "a session": async p => { await openSession(p, "把首页标题改得更醒目"); },
  "terminal": async p => { await tool(p, "终端"); await p.getByRole("button", { name: /终端 1/ }).first().tap(); await p.locator(".terminal-host").waitFor(); },
  "terminal list": async p => { await tool(p, "终端"); await see(p, "终端 1"); },
  "environment": async p => { await tool(p, "环境"); await see(p, "环境运行中"); },
  "archived sessions": async p => { await openDrawer(p); await drawer(p).getByRole("button", { name: "设置", exact: true }).tap(); await p.getByRole("button", { name: "已归档会话" }).tap(); await see(p, "旧的实验记录"); },
  "files": async p => { await tool(p, "文件"); await see(p, "README.md"); },
};
const idle = [];
if (!args["no-idle"]) {
  const keepAwake = Boolean(args["keep-awake"]);
  console.log("idle for " + idleSeconds + " s per screen" + (keepAwake ? ", kept awake with a synthetic touch every 10 s (the page's own loops, without the example's rest)" : "") + ", " + Object.keys(scenarios).length + " screens in batches of 4");
  const names = Object.keys(scenarios);
  for (let at = 0; at < names.length; at += 4) {
    await Promise.all(names.slice(at, at + 4).map(async name => {
      const p = await start();
      await scenarios[name](p);
      await p.waitForTimeout(2500);
      const session = await p.context().newCDPSession(p);
      await session.send("Performance.enable");
      const busy = async () => (await session.send("Performance.getMetrics")).metrics.find(m => m.name === "TaskDuration").value;
      const from = await p.evaluate(() => performance.now()), busyFrom = await busy();
      for (let waited = 0; waited < idleSeconds; waited += 10) {
        if (keepAwake) await p.evaluate(() => window.dispatchEvent(new Event("pointerdown")));
        await p.waitForTimeout(Math.min(10, idleSeconds - waited) * 1000);
      }
      const to = await p.evaluate(() => performance.now()), busyTo = await busy();
      const hidden = await p.evaluate(() => document.hidden);
      const all = await net(p);
      const inside = all.filter(r => r.start >= from && r.start < to);
      // The example rests 30 s after the last touch; "after rest" counts what still starts after 32 s.
      const quiet = inside.filter(r => r.start >= from + 32000);
      const keys = {};
      for (const r of inside) { const key = r.method + " " + r.path.replace("/api/mobile", "").replace(/[0-9a-f]{16}/, ":id").replace(/\/cli\/sessions\/[^/]+/, "/cli/sessions/:id"); keys[key] = (keys[key] || 0) + 1; }
      idle.push({ name, seconds: (to - from) / 1000, requests: inside.length, per20: inside.length / ((to - from) / 20000), quiet: quiet.length, hidden, cpu: (busyTo - busyFrom) / ((to - from) / 1000), keys });
      const early = all.filter(r => r.end && r.end - r.start < 1000 && r.start >= from && r.start < to);
      if (early.length > 4) note(name + ": " + early.length + " requests answered within 1 s while idle, e.g. " + early[0].method + " " + early[0].path);
      const unanswered = all.filter(r => !r.end && r.start < to - 70000);
      if (unanswered.length) note(name + ": " + unanswered.length + " requests unanswered for over 70 s: " + unanswered[0].path);
      const bad = all.filter(r => r.unhandled);
      if (bad.length) note(name + ": unhandled path " + bad[0].path);
      if (!keepAwake) {
        // The archive list and the session's account panel keep their own slow loops (one answer per 60 s hold); they are
        // inside the budget below, so they are reported, not failed.
        if (!hidden) note(name + ": the example did not rest");
        if (inside.length / ((to - from) / 20000) > 1) note(name + ": " + inside.length + " requests in " + Math.round((to - from) / 1000) + " s is more than one per 20 s");
        if (name === "start screen") {
          // A touch brings the page back and it reads again.
          await openDrawer(p);
          await p.waitForTimeout(1500);
          if (await p.evaluate(() => document.hidden)) note("a touch did not wake the resting example");
          if ((await net(p)).filter(r => r.start > to).length < 2) note("the page did not read again after a touch");
          await see(p, "把首页标题改得更醒目");
          // A bare click (no pointer, as VoiceOver sends it) wakes the example too, and the page reads again.
          await p.keyboard.press("Escape");
          await p.waitForTimeout(33000);
          if (!(await p.evaluate(() => document.hidden))) note("the example did not rest again after the touch");
          const seenBefore = (await net(p)).length;
          await p.evaluate(() => document.querySelector('button[aria-label="展开侧栏"]').click());
          await p.waitForTimeout(1500);
          if (await p.evaluate(() => document.hidden)) note("a bare click did not wake the resting example");
          if ((await net(p)).length - seenBefore < 2) note("the page did not read again after a bare click");
        }
      }
      await p.context().close();
    }));
  }
  console.log("screen".padEnd(20) + "requests".padStart(9) + "per 20 s".padStart(10) + "after rest".padStart(11) + "rested".padStart(8) + "cpu busy".padStart(10) + "  what asks");
  for (const row of idle) console.log(row.name.padEnd(20) + String(row.requests).padStart(9) + row.per20.toFixed(2).padStart(10) + String(row.quiet).padStart(11) + String(row.hidden).padStart(8) + (row.cpu * 100).toFixed(2).padStart(9) + "%  " + Object.entries(row.keys).map(([k, v]) => k + " x" + v).join("; "));
}

} catch (error) {
  console.log(error.stack || error);
  problems.push("the walk stopped: " + (error.message || error).toString().split("\n")[0]);
} finally {
  await browser.close();
  server.close();
}
console.log("images: " + shots);
if (problems.length) { console.log(problems.length + " problem(s)"); process.exit(1); }
console.log("example workspace check passed");
