// Device matrix for the web on phones and tablets: the portal, the phone UI (served at / to phones) and the
// tablet workbench, on real builds behind the portal mock (ui/tenant/mock-server.mjs with phone fixtures).
// Per screen it asserts: no horizontal overflow of the document; nothing interactive outside the visual
// viewport or under a safe-area inset (insets are injected through the --safe-* variables the styles read);
// tap targets at least 44x44 CSS px or spaced as if they were; inputs at least 16 px; no clipped text in buttons
// and titles (ellipsis is a choice, clipping is not); the composer stays visible in a keyboard-sized viewport;
// Back returns to the previous screen. Contact sheets go to MOBILE_WEB_SHEETS (default: a temp directory).
//
//   npm --prefix ui run build && node ui/scripts/mobile-web-matrix.mjs            # smoke size, a few minutes
//   node ui/scripts/mobile-web-matrix.mjs --full                                   # every size, 130 % text, reduced motion
//   CAGE_TEST_BROWSER=webkit node ui/scripts/mobile-web-matrix.mjs                 # Playwright WebKit
//   --report  prints failures without failing; --only=<device substring>; --screens=<substring,...>
import { createRequire } from "node:module";
import { mkdir, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { startMockServer } from "../tenant/mock-server.mjs";

const require = createRequire(import.meta.url);
const engines = require(process.env.CAGE_PLAYWRIGHT_MODULE || "playwright");
const engine = process.env.CAGE_TEST_BROWSER || "chromium";
const args = process.argv.slice(2);
const flag = (name) => args.includes("--" + name);
const option = (name) => args.find((arg) => arg.startsWith("--" + name + "="))?.split("=")[1];
const full = flag("full");
const report = flag("report");
const sheets = process.env.MOBILE_WEB_SHEETS || join(tmpdir(), "mobile-web-sheets");
const workbenchDir = process.env.CAGE_WORKBENCH_DIR || fileURLToPath(new URL("../../internal/workbench/web/", import.meta.url));
const iPhone = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1";
const iPad = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15";
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// kind: phone devices get the phone UI, tablets the desktop workbench. insets: CSS px of a notch/home bar.
const portraitPhone = { top: 47, right: 0, bottom: 34, left: 0 };
const landscapePhone = { top: 0, right: 47, bottom: 21, left: 47 };
const none = { top: 0, right: 0, bottom: 0, left: 0 };
const DEVICES = [
  { name: "320x568", width: 320, height: 568, dpr: 2, kind: "phone", insets: { top: 20, right: 0, bottom: 0, left: 0 }, smoke: true },
  { name: "360x780", width: 360, height: 780, dpr: 3, kind: "phone", insets: { top: 24, right: 0, bottom: 16, left: 0 } },
  { name: "375x667", width: 375, height: 667, dpr: 2, kind: "phone", insets: { top: 20, right: 0, bottom: 0, left: 0 } },
  { name: "390x844", width: 390, height: 844, dpr: 3, kind: "phone", insets: portraitPhone, smoke: true },
  { name: "393x852", width: 393, height: 852, dpr: 3, kind: "phone", insets: { top: 59, right: 0, bottom: 34, left: 0 } },
  { name: "412x915", width: 412, height: 915, dpr: 2.625, kind: "phone", insets: { top: 24, right: 0, bottom: 16, left: 0 } },
  { name: "430x932", width: 430, height: 932, dpr: 3, kind: "phone", insets: { top: 59, right: 0, bottom: 34, left: 0 } },
  { name: "440x956", width: 440, height: 956, dpr: 3, kind: "phone", insets: { top: 62, right: 0, bottom: 34, left: 0 } },
  { name: "fold-344x882", width: 344, height: 882, dpr: 3, kind: "phone", insets: { top: 24, right: 0, bottom: 16, left: 0 }, unfold: { width: 690, height: 829 }, smoke: true },
  { name: "landscape-844x390", width: 844, height: 390, dpr: 3, kind: "phone", insets: landscapePhone, smoke: true },
  { name: "landscape-667x375", width: 667, height: 375, dpr: 2, kind: "phone", insets: none },
  { name: "split-507x1024", width: 507, height: 1024, dpr: 2, kind: "phone", agent: iPad, insets: none },
  { name: "tablet-744x1133", width: 744, height: 1133, dpr: 2, kind: "tablet", insets: { top: 24, right: 0, bottom: 20, left: 0 }, smoke: true },
  { name: "tablet-1133x744", width: 1133, height: 744, dpr: 2, kind: "tablet", insets: { top: 24, right: 0, bottom: 20, left: 0 } },
  { name: "tablet-820x1180", width: 820, height: 1180, dpr: 2, kind: "tablet", insets: { top: 24, right: 0, bottom: 20, left: 0 } },
  { name: "tablet-1180x820", width: 1180, height: 820, dpr: 2, kind: "tablet", insets: { top: 24, right: 0, bottom: 20, left: 0 } },
  { name: "tablet-1024x1366", width: 1024, height: 1366, dpr: 2, kind: "tablet", insets: { top: 24, right: 0, bottom: 20, left: 0 } },
  { name: "tablet-1366x1024", width: 1366, height: 1024, dpr: 2, kind: "tablet", insets: { top: 24, right: 0, bottom: 20, left: 0 } },
];
const variants = full
  ? [{ theme: "light", text: 1 }, { theme: "dark", text: 1 }, { theme: "light", text: 1.3, motion: "reduce" }]
  : [{ theme: "light", text: 1 }, { theme: "dark", text: 1 }];
const devices = DEVICES.filter((device) => (full || device.smoke) && (!option("only") || device.name.includes(option("only"))));
const screenFilter = option("screens")?.split(",");
const wanted = (name) => !screenFilter || screenFilter.some((part) => name.includes(part));

// ---- in-page checks -------------------------------------------------------------------------------------------
// Runs inside a document (top level or the workspace frame). Returns a list of failure strings.
function inspect({ insets, keyboard }) {
  const failures = [];
  const vv = window.visualViewport;
  const viewport = { width: vv ? vv.width : innerWidth, height: vv ? vv.height : innerHeight };
  const root = document.documentElement;
  if (root.scrollWidth > innerWidth + 1) failures.push(`document overflows horizontally: ${root.scrollWidth} > ${innerWidth}`);
  const describe = (el) => {
    const label = (el.getAttribute("aria-label") || el.textContent || el.getAttribute("placeholder") || el.tagName).trim().replace(/\s+/g, " ").slice(0, 32);
    return `${el.tagName.toLowerCase()}${el.classList[0] ? "." + el.classList[0] : ""} "${label}"`;
  };
  const visible = (el) => {
    const style = getComputedStyle(el);
    if (style.visibility === "hidden" || style.display === "none" || Number(style.opacity) === 0) return false;
    const rect = el.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0 && !el.closest("[hidden], [inert], [aria-hidden=true]");
  };
  // Is this element visible within its scroll containers (i.e. a user could see it without scrolling)?
  const scrollers = (el) => {
    const list = [];
    for (let node = el.parentElement; node && node !== document.body && node !== root; node = node.parentElement) {
      const style = getComputedStyle(node);
      if (/(auto|scroll|hidden|clip)/.test(style.overflowY + style.overflowX)) list.push(node);
    }
    return list;
  };
  const clippedAway = (el, rect) => scrollers(el).some((node) => {
    const box = node.getBoundingClientRect();
    return rect.bottom <= box.top + 1 || rect.top >= box.bottom - 1 || rect.right <= box.left + 1 || rect.left >= box.right - 1;
  });
  // A modal dialog makes the rest of the page inert: check only what is inside it.
  const modal = [...document.querySelectorAll("dialog[open]")].find((dialog) => { try { return dialog.matches(":modal"); } catch { return false; } });
  const scope = modal || document;
  const interactive = [...scope.querySelectorAll("iframe, button, a[href], input:not([type=hidden]), textarea, select, summary, [role=button], [role=tab], [role=option], [role=menuitem], [tabindex]:not([tabindex='-1'])")]
    .filter((el) => visible(el) && !el.closest(".xterm") && !(el.tagName === "INPUT" && el.type === "file"));
  const targets = [];
  for (const el of interactive) {
    const rect = el.getBoundingClientRect();
    if (clippedAway(el, rect)) continue;
    // Controls inside a scroller may sit below the fold; fixed chrome may not.
    const around = scrollers(el);
    const inScroller = root.scrollHeight > innerHeight + 1 || around.some((node) => node.scrollHeight > node.clientHeight + 1);
    // A row that scrolls sideways (the terminal keys) may hold keys past the edge.
    const inRow = around.some((node) => /(auto|scroll)/.test(getComputedStyle(node).overflowX) && node.scrollWidth > node.clientWidth + 1);
    const box = { left: insets.left, top: insets.top, right: viewport.width - insets.right, bottom: viewport.height - insets.bottom };
    const outside = (!inRow && (rect.left < box.left - 0.5 || rect.right > box.right + 0.5)) || (!inScroller && (rect.top < box.top - 0.5 || rect.bottom > box.bottom + 0.5));
    if (outside) failures.push(`outside the safe viewport: ${describe(el)} at ${Math.round(rect.left)},${Math.round(rect.top)} ${Math.round(rect.width)}x${Math.round(rect.height)}`);
    const inlineLink = el.tagName === "A" && getComputedStyle(el).display === "inline" && el.closest("p, li, td");
    // A framed document is content, not a tap target (its own controls are checked inside it). A control mostly
    // scrolled out of its scroller (a tab at the end of its strip) is scrolled into view before anyone taps it.
    const shown = around.reduce((r, node) => {
      const b = node.getBoundingClientRect();
      return { left: Math.max(r.left, b.left), top: Math.max(r.top, b.top), right: Math.min(r.right, b.right), bottom: Math.min(r.bottom, b.bottom) };
    }, { left: rect.left, top: rect.top, right: rect.right, bottom: rect.bottom });
    const mostlyShown = shown.right - shown.left >= rect.width / 2 && shown.bottom - shown.top >= rect.height / 2;
    if (!inlineLink && mostlyShown && !el.matches("[role=option], iframe")) targets.push({ el, rect, shown });
  }
  // A scroller that runs under the home bar must let its last control scroll clear of it.
  const bottomEdge = viewport.height - insets.bottom;
  for (const node of scope.querySelectorAll("*")) {
    if (!/(auto|scroll)/.test(getComputedStyle(node).overflowY) || !visible(node) || node.scrollHeight <= node.clientHeight + 1) continue;
    const box = node.getBoundingClientRect();
    const overlap = box.bottom - bottomEdge;
    const inside = interactive.filter((el) => node.contains(el));
    if (overlap <= 0.5 || !inside.length) continue;
    const last = Math.max(...inside.map((el) => el.getBoundingClientRect().bottom - box.top + node.scrollTop));
    const room = node.scrollHeight - last;
    if (room < overlap - 0.5) failures.push(`last control cannot scroll off the bottom inset: ${describe(node)} ${Math.round(room)}px < ${Math.round(overlap)}px`);
  }
  for (const { el, rect, shown } of targets) {
    if (rect.width >= 43.5 && rect.height >= 43.5) continue;
    // Partly scrolled out (a key at the end of the key row): it is scrolled into view before it is tapped.
    if (shown.right - shown.left < rect.width - 1 || shown.bottom - shown.top < rect.height - 1) continue;
    // Equivalent spacing: a 44 px square centred on the control touches no other control. Both are taken as
    // their scrollers show them (a code block scrolled under the top bar is not next to the menu button).
    const cx = (shown.left + shown.right) / 2, cy = (shown.top + shown.bottom) / 2;
    const zone = { left: cx - 22, right: cx + 22, top: cy - 22, bottom: cy + 22 };
    const crowded = targets.find((other) => other.el !== el && !other.el.contains(el) && !el.contains(other.el) &&
      other.shown.left < zone.right && other.shown.right > zone.left && other.shown.top < zone.bottom && other.shown.bottom > zone.top);
    if (crowded) failures.push(`small target: ${describe(el)} ${Math.round(rect.width)}x${Math.round(rect.height)} next to ${describe(crowded.el)}`);
  }
  for (const el of document.querySelectorAll("input:not([type=hidden]):not([type=checkbox]):not([type=radio]):not([type=file]):not([type=range]), textarea, select, [contenteditable=true]")) {
    if (!visible(el) || el.closest(".xterm")) continue;
    const size = parseFloat(getComputedStyle(el).fontSize);
    if (size < 16) failures.push(`input text below 16 px (zooms on iOS): ${describe(el)} ${size}px`);
  }
  for (const el of document.querySelectorAll("button, [role=button], h1, h2, .btn, .action, .tool-link")) {
    if (!visible(el)) continue;
    const style = getComputedStyle(el);
    const check = [el, ...el.querySelectorAll("span, strong")].filter((node) => node.childElementCount === 0 && node.textContent.trim());
    for (const node of check) {
      const s = getComputedStyle(node);
      if (s.textOverflow === "ellipsis" || style.textOverflow === "ellipsis") continue;
      // Screen-reader-only text is clipped to a pixel on purpose.
      if (node.getBoundingClientRect().width <= 1) continue;
      if (node.scrollWidth > node.clientWidth + 1 && /(hidden|clip)/.test(s.overflowX + style.overflowX)) failures.push(`clipped text: ${describe(el)}`);
    }
  }
  if (keyboard) {
    const compose = document.querySelector(".compose textarea, .composer textarea, .key-bar");
    if (!compose) failures.push("keyboard: no composer or key bar");
    else {
      const rect = compose.getBoundingClientRect();
      if (rect.bottom > viewport.height + 0.5 || rect.top < 0) failures.push(`keyboard: composer hidden (${Math.round(rect.top)}-${Math.round(rect.bottom)} of ${Math.round(viewport.height)})`);
    }
    const bar = document.querySelector(".topbar");
    if (bar && bar.getBoundingClientRect().bottom <= 0) failures.push("keyboard: the top bar jumped out of view");
  }
  return failures;
}

// ---- runner ---------------------------------------------------------------------------------------------------
const browserType = engines[engine];
const launch = engine === "chromium" ? { executablePath: process.env.CAGE_TEST_BROWSER_PATH } : {};
const browser = await browserType.launch(launch);
const mock = await startMockServer({ workbenchDir, bootDelayMs: 600, phoneFixture: true, globalLimit: 100000 });
const results = [];
const shots = new Map(); // sheet name -> [{ label, png }]
let failed = 0;

async function record(page, sheet, screen, failures, frame) {
  const label = screen + (failures.length ? ` (${failures.length})` : "");
  const png = await page.screenshot({ animations: "disabled" });
  if (!shots.has(sheet)) shots.set(sheet, []);
  shots.get(sheet).push({ label, png, bad: failures.length > 0 });
  results.push({ sheet, screen, failures });
  failed += failures.length;
  if (failures.length) console.log(`  ${sheet} / ${screen}:\n    ${failures.slice(0, 12).join("\n    ")}${failures.length > 12 ? `\n    … ${failures.length - 12} more` : ""}`);
  void frame;
}

async function check(page, sheet, screen, { frame, keyboard = false, insets } = {}) {
  if (!wanted(screen)) return;
  await sleep(250);
  const target = frame || page;
  const failures = await target.evaluate(inspect, { insets, keyboard }).catch((error) => [`check failed: ${error.message}`]);
  await record(page, sheet, screen, failures, frame);
}

// Inject a notch: the styles read --safe-top/right/bottom/left (default env(safe-area-inset-*)).
const insetStyle = (insets) => `:root{--safe-top:${insets.top}px !important;--safe-right:${insets.right}px !important;--safe-bottom:${insets.bottom}px !important;--safe-left:${insets.left}px !important}`;

async function newContext(device, variant, layout) {
  const context = await browser.newContext({
    viewport: { width: device.width, height: device.height }, deviceScaleFactor: device.dpr,
    isMobile: engine === "chromium" ? true : undefined, hasTouch: true, colorScheme: variant.theme,
    reducedMotion: variant.motion || "no-preference", userAgent: device.agent || (device.kind === "phone" ? iPhone : iPad),
    // The insets are a stylesheet injected into pages whose CSP allows only their own styles.
    bypassCSP: true,
    // Chromium's mobile emulation claims Sec-CH-UA-Mobile: ?1 whatever the user agent; Safari sends no
    // client hints, so drop the claim for the iPad agent.
    extraHTTPHeaders: (device.agent || (device.kind === "phone" ? iPhone : iPad)) === iPad && engine === "chromium" ? { "sec-ch-ua-mobile": "?0" } : undefined,
  });
  await context.addInitScript(({ css }) => {
    // The notch belongs to the top-level page; a framed workspace is placed clear of it by the portal.
    if (window.top !== window) return;
    const apply = () => {
      const style = document.createElement("style");
      style.dataset.matrix = "insets";
      style.textContent = css;
      document.documentElement.append(style);
    };
    if (document.documentElement) apply(); else document.addEventListener("DOMContentLoaded", apply);
  }, { css: insetStyle(device.insets) });
  if (layout) context.layout = layout;
  return context;
}

let userCount = 0;
async function signIn(context, page) {
  const user = { username: `matrix${++userCount}`, password: "fixture password only" };
  const post = (path, data) => page.request.post(mock.origin + path, { data, headers: { Origin: mock.origin } });
  await post("/api/tenant/register", user);
  return post;
}
async function enterWorkspace(page, post, name = "开发环境") {
  const { vm } = await (await post("/api/tenant/vms", { name })).json();
  let open;
  for (let i = 0; i < 300; i++) {
    open = await post(`/api/tenant/vms/${vm.id}/open`, {});
    if (open.status() === 200) break;
    await sleep(50);
  }
  const { workspace_url: entry } = await open.json();
  await page.goto(entry);
  await page.waitForURL((url) => url.pathname === "/", { timeout: 15000 });
  return { vm, origin: new URL(entry).origin };
}

async function portalScreens(device, variant, sheet) {
  const context = await newContext(device, variant);
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  const insets = device.insets;
  try {
    await page.goto(mock.origin);
    await page.locator("form").first().waitFor();
    await check(page, sheet, "portal sign-in", { insets });
    const register = page.getByRole("button", { name: "注册", exact: true });
    if (await register.count()) { await register.first().click(); await check(page, sheet, "portal register", { insets }); }
    const post = await signIn(context, page);
    await post("/api/tenant/vms", { name: "开发环境" });
    await post("/api/tenant/vms", { name: "一个名字特别长的预览环境用于测试截断和换行" });
    await page.goto(mock.origin);
    await page.locator(".vm-card").first().waitFor();
    await check(page, sheet, "portal home", { insets });
    const create = page.getByRole("button", { name: "新建虚拟机" }).first();
    await create.click();
    await page.getByRole("dialog").waitFor();
    await check(page, sheet, "portal create dialog", { insets });
    await page.keyboard.press("Escape");
    await page.locator(".vm-card").first().click();
    await page.locator(".hero, .steps").first().waitFor().catch(() => {});
    await check(page, sheet, "portal starting", { insets });
    const frame = page.frameLocator("iframe.workspace-frame");
    await page.waitForFunction(() => document.querySelector("iframe.workspace-frame:not(.is-loading)"), null, { timeout: 20000 }).catch(() => {});
    await sleep(1500);
    const inner = page.frames().find((f) => f !== page.mainFrame() && f.url().startsWith("http://vm-"));
    await check(page, sheet, "portal workspace", { insets });
    // WebKit keeps no VM cookie in the frame on *.localhost (see mobile-web-smoke.mjs): no frame checks there.
    if (inner && engine === "webkit") console.log(`  ${sheet} / portal workspace (frame): skipped on WebKit`);
    if (inner && engine !== "webkit") await check(page, sheet, "portal workspace (frame)", { insets: none, frame: inner });
    // The keyboard inside the framed phone UI: the portal sizes the frame to the visible area.
    if (inner && engine !== "webkit" && device.kind === "phone" && wanted("portal keyboard")) {
      const field = inner.locator(".composer textarea, .compose textarea").first();
      if (await field.count()) {
        await field.focus();
        const keyboardHeight = Math.round(device.height * (device.height > device.width ? 0.4 : 0.55));
        await page.setViewportSize({ width: device.width, height: device.height - keyboardHeight });
        await sleep(400);
        await check(page, sheet, "portal keyboard (frame)", { insets: none, frame: inner, keyboard: true });
        await page.setViewportSize({ width: device.width, height: device.height });
        await field.blur();
      }
    }
    void frame;
    // A VM whose start fails.
    mock.state.bootFailures.set("*", "磁盘空间不足，无法启动虚拟机。");
    const { vm } = await (await post("/api/tenant/vms", { name: "出错的环境" })).json().catch(() => ({}));
    await page.goto(mock.origin);
    if (vm) {
      await page.locator(".vm-card").filter({ hasText: "出错的环境" }).click().catch(() => {});
      await sleep(1500);
      await check(page, sheet, "portal error", { insets });
    }
    mock.state.bootFailures.delete("*");
  } finally {
    await context.close();
  }
}

async function phoneScreens(device, variant, sheet) {
  const context = await newContext(device, variant);
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  const insets = device.insets;
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  try {
    const post = await signIn(context, page);
    const { origin } = await enterWorkspace(page, post);
    await context.addCookies([{ name: "cage-layout", value: "phone", url: origin }]);
    await page.goto(origin + "/");
    await page.locator(".mobile-app").waitFor();
    await page.locator(".greeting, .recent").first().waitFor();
    await check(page, sheet, "phone home", { insets });
    const drawerButton = page.getByRole("button", { name: "展开侧栏", exact: true });
    const drawer = page.getByRole("dialog", { name: "工作区侧栏" });
    const openDrawer = async () => { if (await drawerButton.isVisible().catch(() => false)) { await drawerButton.click(); await sleep(350); } };
    await openDrawer();
    await check(page, sheet, "phone drawer", { insets });
    await drawer.getByRole("button", { name: "整理发布说明" }).first().click();
    await page.getByText("发布说明已整理好").first().waitFor();
    await check(page, sheet, "phone chat", { insets });
    // Keyboard: the viewport shrinks by a typical keyboard; the composer and the top bar stay in view.
    if (wanted("phone keyboard")) {
      await page.locator(".compose textarea").focus();
      const keyboardHeight = Math.round(device.height * (device.height > device.width ? 0.4 : 0.55));
      await page.setViewportSize({ width: device.width, height: device.height - keyboardHeight });
      await sleep(400);
      await check(page, sheet, "phone keyboard", { insets: { ...insets, bottom: 0 }, keyboard: true });
      await page.setViewportSize({ width: device.width, height: device.height });
      await page.locator(".compose textarea").blur();
    }
    await openDrawer();
    await drawer.getByRole("button", { name: /修复门户/ }).first().click();
    await page.getByText(/允许使用/).first().waitFor();
    await check(page, sheet, "phone approval", { insets });
    await openDrawer();
    await drawer.getByRole("button", { name: "文件", exact: true }).click();
    await page.getByRole("button", { name: /^README\.md/ }).waitFor();
    await check(page, sheet, "phone files", { insets });
    await page.getByRole("button", { name: /^README\.md/ }).click();
    await page.getByText("手机网页版测试项目").waitFor();
    await check(page, sheet, "phone file preview", { insets });
    // Back closes the sheet and stays on the files screen.
    await page.goBack();
    await sleep(400);
    const backFailures = [];
    if (await page.getByText("手机网页版测试项目").isVisible().catch(() => false)) backFailures.push("Back did not close the file preview");
    if (!(await page.getByRole("button", { name: /^README\.md/ }).isVisible().catch(() => false))) backFailures.push("Back left the files screen");
    if (wanted("phone back")) await record(page, sheet, "phone back (sheet)", backFailures);
    await openDrawer();
    await drawer.getByRole("button", { name: "终端", exact: true }).click();
    await page.getByRole("button", { name: /^claude/ }).first().waitFor();
    await check(page, sheet, "phone terminals", { insets });
    await page.getByRole("button", { name: /^claude/ }).first().click();
    await page.locator(".xterm").first().waitFor();
    await check(page, sheet, "phone terminal", { insets });
    // Back returns to the terminal list.
    await page.goBack();
    await sleep(400);
    if (wanted("phone back")) await record(page, sheet, "phone back (screen)", (await page.getByRole("button", { name: /^claude/ }).first().isVisible().catch(() => false)) ? [] : ["Back did not return to the terminal list"]);
    await openDrawer();
    await drawer.getByRole("button", { name: "环境", exact: true }).click();
    await page.locator(".env-hero h1").waitFor();
    await check(page, sheet, "phone environment", { insets });
    if (device.unfold) {
      await page.setViewportSize(device.unfold);
      await sleep(400);
      const kept = await page.locator(".env-hero h1").isVisible().catch(() => false);
      await check(page, sheet + "-unfolded", "phone unfolded keeps its place", { insets: none });
      if (!kept) { results.at(-1).failures.push("unfolding lost the screen"); failed++; }
      await page.setViewportSize({ width: device.width, height: device.height });
    }
    if (errors.length) { results.push({ sheet, screen: "page errors", failures: errors }); failed += errors.length; console.log(`  ${sheet} page errors: ${errors.join("; ")}`); }
  } finally {
    await context.close();
  }
}

async function tabletScreens(device, variant, sheet) {
  const context = await newContext(device, variant);
  const page = await context.newPage();
  page.setDefaultTimeout(15000);
  try {
    const post = await signIn(context, page);
    const { origin } = await enterWorkspace(page, post);
    await page.goto(origin + "/");
    await page.locator(".workbench-shell").waitFor();
    await sleep(800);
    await check(page, sheet, "tablet workbench", { insets: device.insets });
  } finally {
    await context.close();
  }
}

async function writeSheet(name, items) {
  const page = await browser.newPage({ viewport: { width: 1600, height: 900 } });
  try {
    const cells = items.map(({ label, png, bad }) => `<figure class="${bad ? "bad" : ""}"><img src="data:image/png;base64,${png.toString("base64")}"><figcaption>${label}</figcaption></figure>`).join("");
    await page.setContent(`<style>body{margin:0;padding:16px;background:#e9e6dc;font:13px -apple-system,sans-serif;color:#1f1e1d}
      h1{font:600 16px -apple-system,sans-serif;margin:0 0 12px}main{display:flex;flex-wrap:wrap;gap:14px;align-items:flex-start}
      figure{margin:0;width:300px}img{width:100%;border:1px solid #cfcab9;border-radius:6px;background:#fff}figcaption{margin-top:4px}
      .bad figcaption{color:#b3261e;font-weight:600}</style><h1>${name} · ${engine}</h1><main>${cells}</main>`);
    await page.waitForLoadState("load");
    await mkdir(join(sheets, engine), { recursive: true });
    await page.screenshot({ path: join(sheets, engine, name + ".png"), fullPage: true });
  } finally {
    await page.close();
  }
}

try {
  for (const device of devices) {
    for (const variant of variants) {
      const sheet = `${device.name}-${variant.theme}${variant.text !== 1 ? "-text" + Math.round(variant.text * 100) : ""}`;
      console.log(sheet);
      // Larger text as phones apply it (iOS text size, Android display size): everything grows, so the same
      // screen is a smaller viewport in CSS pixels at a higher pixel ratio.
      const scaled = variant.text === 1 ? device : {
        ...device, width: Math.round(device.width / variant.text), height: Math.round(device.height / variant.text), dpr: device.dpr * variant.text,
        insets: Object.fromEntries(Object.entries(device.insets).map(([side, value]) => [side, Math.round(value / variant.text)])),
        unfold: device.unfold && { width: Math.round(device.unfold.width / variant.text), height: Math.round(device.unfold.height / variant.text) },
      };
      // A tablet zoomed below 740 CSS px is phone-sized: it gets the phone UI (ui/src/entry-layout.mjs).
      if (scaled.kind === "tablet" && scaled.width < 740) scaled.kind = "phone";
      for (const run of scaled.kind === "phone" ? [portalScreens, phoneScreens] : [portalScreens, tabletScreens]) {
        try { await run(scaled, variant, sheet); }
        catch (error) { results.push({ sheet, screen: run.name, failures: [`flow broke: ${error.message.split("\n")[0]}`] }); failed++; console.log(`  ${sheet} / ${run.name} broke: ${error.message.split("\n")[0]}`); }
      }
    }
  }
  for (const [name, items] of shots) await writeSheet(name, items);
} finally {
  await browser.close();
  await mock.close();
}
await mkdir(sheets, { recursive: true });
await writeFile(join(sheets, `results-${engine}.json`), JSON.stringify(results, null, 2) + "\n");
const screens = results.length;
const bad = results.filter((r) => r.failures.length).length;
console.log(`${engine}: ${screens} screens on ${devices.length} devices x ${variants.length} variants, ${bad} with failures (${failed} findings). Sheets: ${join(sheets, engine)}`);
if (failed && !report) process.exit(1);
