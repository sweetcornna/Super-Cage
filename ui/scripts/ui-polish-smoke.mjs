// One polish round on the desktop workbench: how a terminal ended (a refused Codex launch), the sidebar's account
// chip, the account row's actions, the one destructive button, the privacy page's disclosure, and the workbench
// under a finger (icons-only status bar on a narrow tablet, a long press to move a sidebar row, a browser tab
// strip that scrolls sideways). The gate explanation has its own smoke (network-explanation-smoke).
import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

const url = process.env.CAGE_UI_URL || "http://127.0.0.1:5217";
const homeReady = (page) => page.getByRole("heading", { name: /Super Cage 环境已就绪/ }).waitFor();
const checks = [];
const ok = (text) => checks.push(text);

const dataRoutes = async (page) => {
  const settings = { defaults: { range: "day", types: ["history", "site", "cache"] }, keep: [{ domain: "github.com", added: Date.now() }], auto: { enabled: false, trigger: "stop", days: 7, types: ["history", "site", "cache"], last: 0, result: "" } };
  await page.route("**/api/browser/data-settings", (route) => route.fulfill({ json: settings }));
  await page.route("**/api/browser/data", (route) => {
    const body = route.request().postDataJSON() || {};
    if (body.mode === "capabilities") return route.fulfill({ json: { available: true } });
    if (body.mode === "summary") return route.fulfill({ json: { history: { count: 1204 }, sites: { count: 38, bytes: 12582912 }, cache: { bytes: 125829120 }, downloads: { count: 3 } } });
    return route.fulfill({ json: {} });
  });
};
const ready = async (page, state) => {
  Object.assign(state.status, { ready: true, vm_running: true });
  await page.route("**/api/audio", (route) => route.fulfill({ json: { format: "s16le", sample_rate: 48000, channels: 2 } }));
};

// 1. Desktop: a mouse window.
{
  let state;
  const { browser, page, errors } = await createWorkbench({ url, codexSessions: true, viewport: { width: 1440, height: 900 },
    beforeNavigate: async (page, value) => {
      state = value;
      await ready(page, state);
      await dataRoutes(page);
      await page.route("**/api/claude-accounts*", (route) => route.fulfill({ json: { accounts: [
        { id: "acc1", label: "个人订阅", enabled: true, status: "ready", email: "me@example.test", plan: "Pro" },
        { id: "acc2", label: "Studio", enabled: true, status: "ready", email: "s@example.test", plan: "Max" },
      ], detection_status: "signed_out" } }));
      state.sockets = [];
      await page.routeWebSocket("**/api/terminals/**", (socket) => {
        state.sockets.push(socket);
        socket.send(Buffer.from(socket.url().includes("term-2") && state.refusal ? "\r\n" + state.refusal + "\r\n" : "\r\n~/Projects/design-lab  $ "));
      });
    } });
  const endAll = () => { for (const socket of state.sockets.splice(0)) { try { socket.send(JSON.stringify({ type: "exit" })); socket.close(); } catch { /* already closed */ } } };
  try {
    await homeReady(page);

    // The status bar keeps its words on a mouse window, even a narrow one.
    await page.setViewportSize({ width: 744, height: 900 });
    assert.equal(await page.locator(".status-bar .status-label").evaluateAll((labels) => labels.filter((label) => label.offsetWidth <= 1).length), 0, "a mouse window keeps the status bar's words");
    await page.setViewportSize({ width: 1440, height: 900 });

    // Item 2: a Codex launch the computer refused (exit 75) says it did not open, and why, on one line.
    state.refusal = "会话账号已变化，请重新读取会话列表";
    await page.getByRole("button", { name: "整理登录流程" }).first().click();
    await page.waitForFunction(() => document.querySelector(".terminal-view .terminal-host"), null, { timeout: 15000 });
    await page.waitForTimeout(1500);
    Object.assign(state.terminals.find((t) => t.title === "整理登录流程"), { closed: true, exit_code: 75 });
    endAll();
    const note = page.locator(".terminal-view:not(.concealed) .terminal-note");
    await page.waitForFunction(() => /未能打开/.test(document.querySelector(".terminal-view:not(.concealed) .terminal-note")?.textContent || ""), null, { timeout: 15000 });
    assert.match(await note.innerText(), /^未能打开：.*会话账号已变化/);
    assert.equal(await note.evaluate((el) => el.getClientRects().length), 1, "the note is one line");
    ok("a refused Codex launch (exit 75) says 未能打开 with the reason, on one line");
    state.terminals.push({ id: "term-plain", title: "终端 2", workspace: state.settings.workspace, closed: false });
    await page.reload(); await homeReady(page);
    await page.getByRole("tab", { name: /终端 2/ }).first().click();
    await page.waitForTimeout(1500);
    Object.assign(state.terminals.find((t) => t.id === "term-plain"), { closed: true, exit_code: 0 });
    endAll();
    await note.waitFor({ state: "attached", timeout: 20000 });
    assert.doesNotMatch(await note.innerText(), /未能打开/);
    ok("a plain terminal that ended keeps the ordinary note");

    // Item 3: the account chip is the account alone, and is shown only when it fits whole.
    const workspace = state.settings.workspace;
    state.terminals = []; // the sidebar lists five rows, then folds the rest
    state.codexSessions[0].account_label = "个人订阅账号";
    state.codexSessions[0].codex_account_id = "bbbbbbbbbbbbbbbbbbbbbbbb";
    state.terminals.push({ id: "t-cl", title: "重构样式", workspace, kind: "claude", claude_account_id: "acc1", cli_session_id: "abcd1234-0000-4000-8000-000000000009", cli_starting: false });
    state.terminals.push({ id: "t-cl2", title: "写一个很长很长的会话标题来检查截断", workspace, kind: "claude", claude_account_id: "acc2", cli_session_id: "abcd1234-0000-4000-8000-000000000008", cli_starting: false });
    for (const width of [1440, 1000]) {
      await page.setViewportSize({ width, height: 800 });
      await page.reload(); await homeReady(page);
      await page.locator(".sidebar-tab .claude-tab-profile", { hasText: "Studio" }).waitFor({ state: "attached" });
      const chips = await page.locator(".sidebar-tab .claude-tab-profile").evaluateAll((nodes) => nodes.map((chip) => {
        const line = chip.closest(".session-text")?.getBoundingClientRect(), box = chip.getBoundingClientRect();
        return { text: chip.textContent, shown: Boolean(line) && box.top >= line.top - 1 && box.bottom <= line.bottom + 1 && box.right <= line.right + 1,
          whole: chip.scrollWidth <= chip.clientWidth + 1 };
      }));
      assert.ok(chips.length >= 3, "chips: " + JSON.stringify(chips));
      for (const chip of chips) {
        assert.doesNotMatch(chip.text, /^(Codex|Claude)\s*·/, "the provider is in the mark: " + chip.text);
        if (chip.shown) assert.ok(chip.whole, "a shown chip is whole: " + chip.text);
      }
      if (width === 1000) assert.ok(chips.some((chip) => !chip.shown), "a chip that does not fit is not shown at " + width + ": " + JSON.stringify(chips));
      assert.ok(chips.some((chip) => chip.shown), "a chip that fits is shown at " + width);
    }
    ok("sidebar chips: no provider word, shown whole or not at all (1440 and 1000)");
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.reload(); await homeReady(page);
    await page.locator(".sidebar-tab", { hasText: "重构样式" }).first().click({ button: "right" });
    assert.equal(await page.locator(".workspace-tab-menu").count(), 0, "a right click on a row keeps focusing its actions");

    // Item 4: the actions in an account row weigh the same.
    await page.getByRole("button", { name: "Claude 账号管理", exact: true }).click();
    const row = page.getByRole("article", { name: "个人订阅", exact: true });
    await row.waitFor();
    const actions = await row.locator(".claude-profile-actions").evaluate((box) => {
      const glyphs = [...box.querySelectorAll(".quiet-button svg")].map((svg) => {
        const style = getComputedStyle(svg), rect = svg.getBoundingClientRect();
        return { width: rect.width, height: rect.height, stroke: style.strokeWidth, color: style.color };
      });
      const kids = [...box.children].map((el) => el.getBoundingClientRect());
      return { glyphs, centres: kids.map((rect) => rect.top + rect.height / 2),
        gaps: kids.slice(1).map((rect, at) => Math.round(rect.left - kids[at].right)) };
    });
    assert.ok(actions.glyphs.length >= 2, JSON.stringify(actions));
    for (const glyph of actions.glyphs) {
      assert.deepEqual([glyph.width, glyph.height, glyph.stroke, glyph.color], [16, 16, "1.75px", actions.glyphs[0].color], JSON.stringify(glyph));
    }
    assert.ok(Math.max(...actions.centres) - Math.min(...actions.centres) <= 1, "one baseline: " + actions.centres);
    assert.ok(actions.gaps.slice(1).every((gap) => gap === 4), "one gap between the buttons: " + actions.gaps);
    ok("account row actions: one glyph size, stroke and colour, one baseline, one gap");

    // Item 5: the destructive confirm is the quiet error style, not a solid red fill.
    await page.evaluate(() => document.dispatchEvent(new CustomEvent("cage-browser-data", { detail: { view: "clear" } })));
    const clear = page.getByRole("dialog").getByRole("button", { name: "清除数据" });
    await clear.waitFor();
    const paint = await clear.evaluate((button) => {
      const probe = document.createElement("i");
      probe.style.color = "var(--error)";
      document.body.append(probe);
      const error = getComputedStyle(probe).color;
      probe.remove();
      const style = getComputedStyle(button);
      // color-mix() computes to color(srgb 0.97 0.93 0.93); plain colours to rgb(179, 38, 30).
      const light = (color) => {
        const [r, g, b] = color.match(/[\d.]+/g).slice(0, 3).map((value) => Number(value) / (color.startsWith("color(") ? 1 : 255));
        return 0.2126 * r + 0.7152 * g + 0.0722 * b;
      };
      return { error, color: style.color, background: style.backgroundColor, fill: light(style.backgroundColor), ink: light(style.color) };
    });
    assert.equal(paint.color, paint.error, "error text");
    assert.notEqual(paint.background, paint.error, "not a solid error fill");
    assert.ok(paint.fill > paint.ink + 0.5, "a faint tint under the error text: " + JSON.stringify(paint));
    ok("clear dialog: error text on a faint tint, not a solid red fill");
    await page.keyboard.press("Escape");

    state.tabs = [{ id: "browser-settings", kind: "browser-settings", title: "设置", url: "cage://settings#privacy" }];
    state.active = "browser-settings";
    await page.reload(); await homeReady(page);
    await page.getByRole("button", { name: "打开浏览器", exact: true }).click();
    const kept = page.getByRole("region", { name: "保持登录的网站" });
    await kept.waitFor({ timeout: 8000 });
    const help = kept.locator(".bd-details .field-help");
    assert.equal(await help.isVisible(), false, "the loopback note is folded");
    await kept.getByText("了解详情", { exact: true }).click();
    assert.equal(await help.isVisible(), true, "了解详情 opens it");
    assert.ok((await help.innerText()).length > 30);
    ok("privacy page: the loopback note sits behind 了解详情");
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
}

// 2. The mouse still reorders sidebar rows (drag, Alt+Arrow, Escape cancels): the touch work shares the drag code.
{
  let state;
  const { browser, page, errors } = await createWorkbench({ url, viewport: { width: 1440, height: 900 },
    beforeNavigate: async (page, value) => { state = value; await ready(page, state); } });
  try {
    state.terminals = ["A", "B", "C"].map((name) => ({ id: "t-" + name, title: "终端 " + name, workspace: state.settings.workspace }));
    await page.reload(); await homeReady(page);
    const rows = () => page.locator(".sidebar-tab > button").evaluateAll((nodes) => nodes.map((node) => node.textContent.trim()));
    const row = (name) => page.locator(".sidebar-tab", { hasText: name });
    await row("终端 C").waitFor();
    assert.deepEqual(await rows(), ["终端 A", "终端 B", "终端 C"]);
    const drag = async (from, to, release = true) => {
      const a = await row(from).boundingBox(), b = await row(to).boundingBox();
      await page.mouse.move(a.x + 60, a.y + a.height / 2);
      await page.mouse.down();
      await page.mouse.move(b.x + 60, b.y + b.height / 2, { steps: 10 });
      if (release) await page.mouse.up();
    };
    await drag("终端 C", "终端 A");
    await page.waitForFunction(() => document.querySelector(".sidebar-tab > button").textContent.trim() === "终端 C");
    assert.deepEqual(await rows(), ["终端 C", "终端 A", "终端 B"], "a mouse drag still moves a row");
    await row("终端 C").locator("> button").focus();
    await page.keyboard.press("Alt+ArrowDown");
    assert.deepEqual(await rows(), ["终端 A", "终端 C", "终端 B"], "Alt+ArrowDown still moves a row down");
    await drag("终端 B", "终端 A", false);
    await page.keyboard.press("Escape");
    await page.mouse.up();
    assert.deepEqual(await rows(), ["终端 A", "终端 C", "终端 B"], "Escape cancels a drag");
    assert.equal(await page.locator(".workspace-tab-menu").count(), 0);
    ok("mouse: drag, Alt+ArrowDown and Escape-cancel still reorder sidebar rows");
    // Browser tabs on a mouse window: shrunk to fit, never scrolled, and a drag still reorders them.
    state.tabs = Array.from({ length: 12 }, (_, i) => ({ id: "web-" + i, kind: "web", title: "网页 " + i, url: "https://example.com/" + i }));
    state.active = "web-0";
    await page.reload(); await homeReady(page);
    await page.getByRole("button", { name: "打开浏览器", exact: true }).click();
    await page.waitForFunction(() => document.querySelectorAll(".browser-tabs .browser-tab").length === 12);
    assert.equal(await page.locator(".browser-tabs.scrolls").count(), 0, "a mouse window does not scroll the strip");
    assert.equal(await page.locator(".browser-tabs").evaluate((list) => list.scrollWidth <= list.clientWidth + 1), true, "twelve tabs shrink to fit");
    await page.waitForFunction(() => document.querySelector('[data-tab-id="web-0"]').getBoundingClientRect().width > 150);
    await page.waitForTimeout(300);
    const from = await page.locator('[data-tab-id="web-0"]').boundingBox(), to = await page.locator('[data-tab-id="web-3"]').boundingBox();
    await page.mouse.move(from.x + from.width / 2, from.y + 16);
    await page.mouse.down();
    await page.mouse.move(to.x + to.width - 6, to.y + 16, { steps: 18 });
    await page.locator(".browser-tab.dragging").waitFor();
    await page.mouse.up();
    await page.waitForFunction(() => {
      const ids = [...document.querySelectorAll(".browser-tab")].map((node) => node.dataset.tabId);
      return ids.indexOf("web-0") > ids.indexOf("web-3");
    });
    ok("mouse: twelve browser tabs shrink to fit, and a drag still reorders them");
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
}

// 3. A touch tablet held upright (744 px).
{
  let state;
  const { browser, page, errors } = await createWorkbench({ url, viewport: { width: 744, height: 1133 }, deviceScaleFactor: 2,
    contextOptions: { hasTouch: true, isMobile: true },
    beforeNavigate: async (page, value) => {
      state = value;
      await ready(page, state);
      // The longest labels the bar can carry: an exit address with a city, and a closed gate.
      Object.assign(state.status, { location: { ip: "203.0.113.45", country: "United States", city: "San Francisco" },
        safety: { exit_ip: "203.0.113.45", gate: { mode: "closed", until: new Date(Date.now() - 60000).toISOString() } } });
    } });
  try {
    const workspace = state.settings.workspace;
    state.terminals.push(...["A", "B", "C"].map((name) => ({ id: "t-" + name, title: "终端 " + name, workspace })));
    state.tabs = Array.from({ length: 9 }, (_, i) => ({ id: "web-" + i, kind: "web", title: "示例网页标题 " + (i + 1), url: "https://example.com/" + i }));
    state.active = "web-0";
    await page.reload(); await homeReady(page);
    const cdp = await page.context().newCDPSession(page);
    const touch = (type, x, y) => cdp.send("Input.dispatchTouchEvent", { type, touchPoints: type === "touchEnd" ? [] : [{ x, y }] });
    const centre = (box) => ({ x: box.x + box.width / 2, y: box.y + box.height / 2 });
    assert.equal(await page.evaluate(() => matchMedia("(pointer: coarse) and (hover: none)").matches), true, "a touch context");

    // Item 6: icons only, with the words in the accessibility tree and the tooltip.
    const bar = page.locator(".status-bar");
    const foldedLabels = () => [...document.querySelectorAll(".status-bar .status-label")].filter((label) => label.offsetWidth <= 1).length;
    const folded = await page.locator(".status-bar > button:has(> .status-label)").evaluateAll((nodes) => nodes.map((node) => ({
      name: node.textContent.trim(), width: node.getBoundingClientRect().width, height: node.getBoundingClientRect().height,
      label: node.querySelector(".status-label").offsetWidth })));
    assert.ok(folded.length >= 3, JSON.stringify(folded));
    for (const item of folded) assert.ok(item.width >= 44 && item.height >= 44 && item.label <= 1, "a 44 px icon button: " + JSON.stringify(item));
    await page.getByRole("button", { name: "重启环境", exact: true }).waitFor();
    await page.getByRole("button", { name: "停止环境", exact: true }).waitFor();
    assert.equal(await bar.evaluate((node) => node.scrollWidth <= node.clientWidth + 1), true, "nothing runs off the status bar");
    const restart = await page.getByRole("button", { name: "重启环境", exact: true }).boundingBox();
    await touch("touchStart", ...Object.values(centre(restart)));
    await page.locator(".icon-hint:not([hidden])").waitFor({ timeout: 3000 });
    assert.match(await page.locator(".icon-hint").innerText(), /重新启动当前环境/);
    await touch("touchEnd");
    await page.waitForTimeout(300);
    assert.equal(await page.getByRole("dialog").count(), 0, "the long press that showed the tooltip did not restart");
    ok("744 px touch: status bar icons only (44 px buttons), names kept, long press shows the tooltip");
    // The longest labels (exit address with a city, the gate label, the sync message) and the time zone need
    // about 1150 px: folded below it, whether the sidebar is a drawer or docked, and nothing is cut at any width.
    const cutLabels = () => [...document.querySelectorAll(".status-bar > button > span, .status-feedback, .status-clock")]
      .filter((el) => el.offsetWidth > 1 && el.scrollWidth > el.clientWidth + 1).map((el) => el.textContent.trim());
    for (const [width, folds] of [[744, true], [901, true], [1024, true], [1180, true], [1440, false], [744, true]]) {
      await page.setViewportSize({ width, height: 1133 });
      await page.waitForFunction(([fold, expect]) => (eval("(" + fold + ")")() >= 3) === expect, [foldedLabels.toString(), folds]);
      if (folds) assert.deepEqual(await page.evaluate(cutLabels), [], "nothing is cut at " + width);
    }
    ok("the status bar folds its labels while it is under 1150 px wide (drawer or docked), back to words above, with nothing cut");

    // Item 6: a long press on a sidebar row opens a menu to move it.
    await page.locator(".sidebar-expand").first().tap();
    const rows = () => page.locator(".sidebar-tab > button").evaluateAll((nodes) => nodes.map((node) => node.textContent.trim()));
    const rowOf = (name) => page.locator(".sidebar-tab", { hasText: name });
    await rowOf("终端 C").waitFor();
    assert.deepEqual((await rows()).filter((text) => /^终端 [ABC]$/.test(text)), ["终端 A", "终端 B", "终端 C"]);
    const press = async (name, hold = 700) => {
      const box = await rowOf(name).boundingBox();
      await touch("touchStart", box.x + 60, box.y + box.height / 2);
      await page.waitForTimeout(hold);
      await touch("touchEnd");
      await page.waitForTimeout(500);
    };
    await press("终端 C");
    const menu = page.getByRole("menu", { name: "调整顺序" });
    assert.equal(await menu.count(), 1, "the menu stays open after the finger lifts");
    assert.equal(await menu.getByRole("menuitem", { name: "下移" }).isDisabled(), true, "the last row cannot move down");
    await menu.getByRole("menuitem", { name: "上移" }).tap();
    await menu.waitFor({ state: "detached" });
    assert.deepEqual((await rows()).filter((text) => /^终端 [ABC]$/.test(text)), ["终端 A", "终端 C", "终端 B"]);
    await press("终端 A");
    assert.equal(await menu.getByRole("menuitem", { name: "上移" }).isDisabled(), true, "the first row cannot move up");
    await page.keyboard.press("Escape");
    await menu.waitFor({ state: "detached" });
    await press("终端 B", 150);
    assert.equal(await menu.count(), 0, "a short press is a tap, not the menu");
    ok("sidebar rows: a long press opens 上移 / 下移, the lift does not close it, a tap still opens the row");

    // Item 6: a browser tab strip that does not fit keeps readable tabs and scrolls sideways.
    await page.locator(".sidebar-expand").first().tap();
    await page.getByRole("button", { name: "打开浏览器", exact: true }).tap();
    const strip = page.locator(".browser-tabs");
    await page.waitForFunction(() => document.querySelectorAll(".browser-tabs .browser-tab").length === 9);
    await page.waitForFunction(() => document.querySelector(".browser-tabs.scrolls"));
    const widths = await page.locator(".browser-tab").evaluateAll((nodes) => nodes.map((node) => Math.round(node.getBoundingClientRect().width)));
    assert.ok(widths.every((width) => width >= 156), "readable tabs: " + widths);
    const box = await strip.boundingBox();
    await touch("touchStart", box.x + 400, box.y + box.height / 2);
    for (let step = 1; step <= 10; step++) { await touch("touchMove", box.x + 400 - step * 30, box.y + box.height / 2); await page.waitForTimeout(16); }
    await touch("touchEnd");
    await page.waitForFunction(() => document.querySelector(".browser-tabs").scrollLeft > 100);
    await page.getByRole("button", { name: "搜索标签页", exact: true }).tap();
    await page.getByRole("dialog", { name: "搜索标签页", exact: true }).getByRole("button", { name: "示例网页标题 9", exact: true }).tap();
    await page.waitForFunction(() => {
      const list = document.querySelector(".browser-tabs").getBoundingClientRect(), tab = document.querySelector('.browser-tabs [aria-selected="true"]');
      const rect = tab?.getBoundingClientRect();
      return tab?.textContent.includes("示例网页标题 9") && rect.left >= list.left - 1 && rect.right <= list.right + 1;
    });
    ok("browser tabs: 156 px minimum, a swipe scrolls the strip, a tab chosen from the list scrolls into view");
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
}
console.log(checks.map((line) => "ok - " + line).join("\n"));
