import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import { createWorkbench } from "./workbench-fixture.mjs";
import { claudeScienceFixtures as fixtures } from "./claude-science-fixture.mjs";

// Claude Science host rendering (phase 2): the switch, the frame and every
// way the frame gives way, against the fixture's mocked routes and a mocked
// window bridge (desktop/windows/ScienceFrame.cs answers it in the app).
// The front page itself is a stub: the real one is tested in Go.
const capture = process.env.CAGE_TEST_CAPTURE;
if (capture) await mkdir(capture, { recursive: true });
// Each ticket opens its own origin, http://<label>.localhost:<port>.
const framePort = 17840;
const frameEntry = /^http:\/\/[0-9a-f]{32}\.localhost:17840\/_cage\/enter$/;
const isFrame = (url) => url.port === String(framePort) && /^[0-9a-f]{32}\.localhost$/.test(url.hostname);
const windowsWindow = { mode: "local", connection_id: "local", client_platform: "windows", server_platform: "windows", desktop_gateway: true, connection: { state: "connected" } };
const checks = [];

async function open({ mode, clientContext, claudeScience, csp = false, routes = null, contract = 3 }) {
  const native = [];
  // contract 0: an older window, which publishes no frame contract and whose
  // bridge has no scienceFrame op.
  const bridge = { backstop: false, proxy: "verified", contract };
  // Where the frame was when the pane asked the window to detach (the pane
  // parks it first); the stub then removes it as the window would.
  const parkedAtDetach = [];
  let pageRef = null;
  // Unless a test drives the agent transport (routes), its consumer's first
  // bridge call never answers, so it does not call again every 750 ms.
  const agent = Boolean(routes);
  const workbench = await createWorkbench({
    viewport: { width: 1280, height: 800 },
    claudeScience,
    clientContext,
    nativeRequest: async (body) => {
      if (body.action === "refreshSession") return agent ? { url: new URL(process.env.CAGE_UI_URL || "http://127.0.0.1:5198").origin + "/#agent-window-ticket" } : new Promise(() => {});
      if (body.action !== "scienceFrame") return null;
      native.push({ op: body.op, url: body.url });
      if (!bridge.contract) throw new Error("未知原生操作");
      if (body.op === "detach" && pageRef) {
        parkedAtDetach.push(await pageRef.evaluate(() => [...document.querySelectorAll("iframe[data-science-frame]")]
          .map((frame) => ({ hidden: frame.hidden, inPane: Boolean(frame.closest(".claude-science")), title: frame.title }))));
        await pageRef.evaluate(() => document.querySelectorAll("iframe[data-science-frame][hidden]").forEach((frame) => frame.remove()));
      }
      if (body.op === "prepare") return { backstop: bridge.backstop, proxy: bridge.proxy };
      if (body.op === "attach" && !frameEntry.test(String(body.url))) throw new Error("unexpected frame address");
      return true;
    },
    beforeNavigate: async (page, state) => {
      pageRef = page;
      // The window's bridge script publishes its frame contract.
      if (bridge.contract) await page.addInitScript((value) => Object.defineProperty(window, "cageNativeScienceFrame", { value }), bridge.contract);
      state.appearance.mode = mode;
      state.status.ready = state.status.vm_running = true;
      state.framePort = framePort;
      if (routes) await routes(page, state);
      // Playwright answers every bridge call (the app makes one every half
      // second) with a userGesture evaluation, which the page would count as
      // a user action: the smoke sets navigator.userActivation.isActive
      // itself. (A real click in the cross-site frame does activate the
      // window's document in Chrome 157: exp-activation-frame.mjs.)
      await page.addInitScript(() => {
        let active = false;
        Object.defineProperty(window, "setActivationForTest", { value: (value) => { active = value; } });
        Object.defineProperty(Navigator.prototype, "userActivation", { configurable: true, get: () => ({ get isActive() { return active; }, hasBeenActive: true }) });
      });
      await page.route("**/api/audio", route => route.fulfill({ json: { format: "s16le", sample_rate: 48000, channels: 2 } }));
      await page.route(isFrame, route => route.fulfill({ contentType: "text/html; charset=utf-8",
        body: "<!doctype html><html lang=zh-CN><title>Claude Science</title><body style='margin:0;display:grid;place-items:center;height:100vh;font:16px system-ui;background:#fff;color:#1f1e1d'>Claude Science 页面（测试替身）</body></html>" }));
      if (csp) await page.route(url => url.pathname === "/" || url.pathname === "/index.html", async route => {
        const response = await route.fetch();
        await route.fulfill({ response, headers: { ...response.headers(), "content-security-policy": "frame-src 'none'" } });
      });
    },
  });
  return { ...workbench, native, bridge, parkedAtDetach };
}

async function walk(mode) {
  const { browser, page, state, errors, native, bridge, parkedAtDetach } = await open({ mode, clientContext: windowsWindow, claudeScience: "running-host-render-available" });
  const shot = async name => {
    if (!capture) return;
    await page.waitForTimeout(300);
    await page.screenshot({ path: `${capture}/host-${mode}-${name}.png` });
  };
  const posts = path => state.requests.filter(request => request.method === "POST" && request.path === path);
  const ops = () => native.map(call => call.op);
  try {
    const nav = page.getByRole("navigation", { name: "主导航" });
    const row = nav.getByRole("button", { name: "Claude Science", exact: true });
    const pane = page.getByRole("main", { name: "Claude Science", exact: true });
    const science = pane.locator(".claude-science");
    const toggle = pane.getByRole("switch", { name: /本机渲染（实验性）/ });
    const frame = pane.locator("iframe[title='Claude Science']");
    const enter = async () => {
      if (await pane.isVisible()) await nav.getByRole("button", { name: "工作台", exact: true }).click();
      await row.click();
      await pane.waitFor();
    };
    const frameReady = async () => {
      await frame.waitFor({ timeout: 15000 });
      state.hostRender = { ...state.hostRender, session: true };
      await pane.getByRole("toolbar").getByText("时区、语言与浏览器标识来自环境", { exact: true }).waitFor({ timeout: 15000 });
    };
    const until = async (check, what) => {
      for (let waited = 0; !check(); waited += 100) {
        if (waited > 15000) throw new Error("timed out: " + what);
        await page.waitForTimeout(100);
      }
    };

    // 1. Off: a quiet switch under the card, with one line and a link.
    await enter();
    await science.and(page.locator('[data-state="running"]')).waitFor({ timeout: 15000 });
    await toggle.waitFor();
    assert.equal(await toggle.isChecked(), false);
    await pane.getByText("在这个窗口中直接显示 Claude Science，页面看到的仍是环境的时区、语言和浏览器标识。").waitFor();
    assert.equal(await pane.getByRole("link", { name: "了解详情" }).count(), 1);
    assert.equal(await frame.count(), 0);
    assert.deepEqual(ops(), [], "off asks the bridge nothing");
    await shot("1-off");

    // 2. On, but the window still runs without the network switches: one
    //    reload of the window, asked for, never armed.
    await toggle.click();
    await pane.getByText("本机渲染已开启，需要重新载入窗口一次才能生效。").waitFor({ timeout: 15000 });
    assert.deepEqual(posts("/claude-science/host-render").map(request => request.body), [{ enabled: true, contract: 3 }]);
    assert.deepEqual(ops(), ["prepare", "detach"]);
    assert.equal(posts("/claude-science/ticket").length, 0, "no ticket without the switches");
    await shot("2-restart");
    await pane.getByRole("button", { name: "重新载入窗口", exact: true }).click();
    await until(() => ops().includes("restart"), "restart");

    // 3. After the restart the window has them: ticket, arm, frame.
    bridge.backstop = true;
    native.length = 0;
    await page.reload();
    await enter();
    await frameReady();
    assert.deepEqual(ops(), ["prepare", "attach"]);
    const ticket = posts("/claude-science/ticket").at(-1).body;
    assert.deepEqual(Object.keys(ticket.host).sort(), ["languages", "offset", "timezone"]);
    assert.equal(ticket.proxy, "verified");
    assert.equal(ticket.contract, 3);
    assert.equal(typeof ticket.host.offset, "number");
    assert.equal(native[1].url, await frame.getAttribute("src"));
    assert.equal(await frame.getAttribute("sandbox"), "allow-scripts allow-same-origin allow-forms");
    assert.equal(await frame.getAttribute("allow"), "clipboard-write");
    assert.equal(await frame.getAttribute("referrerpolicy"), "no-referrer");
    await page.frameLocator("iframe[title='Claude Science']").getByText("Claude Science 页面（测试替身）").waitFor();
    // Edge to edge under the toolbar.
    const box = await frame.boundingBox();
    const content = await pane.locator(".management-content").boundingBox();
    assert.ok(Math.abs(box.width - content.width) <= 1 && box.height > content.height - 80, JSON.stringify({ box, content }));
    const bar = pane.getByRole("toolbar");
    await bar.getByText("v0.1.73", { exact: true }).waitFor();
    await bar.getByText("已登录", { exact: true }).waitFor();
    assert.equal(await bar.getByRole("button", { name: "登录", exact: true }).count(), 0, "signed in: no sign-in button");
    await bar.getByRole("button", { name: "在隔离浏览器中打开", exact: true }).waitFor();
    await bar.getByRole("button", { name: "停止", exact: true }).waitFor();
    assert.equal(await bar.getByRole("switch").isChecked(), true);
    await shot("3-frame");

    // 4. Sign-in inside the page: the server opened the guest tab. Without
    //    a user action the pane only asks; its button shows the tab, and
    //    leaving the pane drops the overrides and the session.
    const releases = posts("/claude-science/release").length;
    state.tabs.push({ id: "science-login", kind: "science", title: "Claude Science", url: "http://localhost:47800/" });
    const signInTab = { id: "science-login", kind: "science", title: "Claude Science" };
    state.hostRender = { ...state.hostRender, signin: { at: new Date().toISOString(), tab: signInTab } };
    await pane.getByText("Claude Science 页面请求登录，登录在隔离浏览器中完成。").waitFor({ timeout: 15000 });
    await page.waitForTimeout(2500);
    assert.ok(await pane.isVisible(), "no focus move without a user action");
    await shot("3b-signin-asks");
    await pane.getByRole("button", { name: "去登录", exact: true }).click();
    await pane.waitFor({ state: "hidden", timeout: 15000 });
    await page.getByRole("tab", { name: "Claude Science", exact: true }).and(page.locator('[aria-selected="true"]')).waitFor({ timeout: 15000 });
    await until(() => ops().at(-1) === "detach" && posts("/claude-science/release").length === releases + 1, "detach and release");
    // Round-2 N1: when the window was asked to detach, the frame was parked
    // (alive, hidden, out of the pane) for the window's teardown.
    assert.deepEqual(parkedAtDetach.at(-1), [{ hidden: true, inPane: false, title: "" }], "the frame parks before the window's teardown");

    // 5. Back to the pane: a new ticket and the same sign-in is not replayed.
    const tickets = posts("/claude-science/ticket").length;
    await enter();
    await frameReady();
    assert.equal(posts("/claude-science/ticket").length, tickets + 1);
    await page.waitForTimeout(2500);
    assert.ok(await pane.isVisible(), "an old sign-in event does not leave the pane again");
    assert.equal(await pane.getByText("Claude Science 页面请求登录，登录在隔离浏览器中完成。").count(), 0);
    // Right after a user action (a click in the frame): the next sign-in
    // moves at once.
    await page.evaluate(() => window.setActivationForTest(true));
    state.hostRender = { ...state.hostRender, signin: { at: new Date(Date.now() + 1000).toISOString(), tab: signInTab } };
    await pane.waitFor({ state: "hidden", timeout: 15000 });
    await page.evaluate(() => window.setActivationForTest(false));
    await enter();
    await frameReady();

    // 6. The front restarted: one new ticket, then the isolated browser.
    state.hostRender = { ...state.hostRender, session: false };
    await until(() => posts("/claude-science/ticket").length === tickets + 3, "re-ticket");
    await frameReady();
    const opens = posts("/claude-science/open").length;
    state.hostRender = { ...state.hostRender, session: false };
    await pane.waitFor({ state: "hidden", timeout: 15000 });
    assert.equal(posts("/claude-science/open").length, opens + 1, "fell back to the isolated browser");
    assert.equal(posts("/claude-science/ticket").length, tickets + 3, "only one re-ticket");
    await enter();
    await pane.getByText("与本机渲染页面的连接已结束，已改用隔离浏览器。").waitFor();
    assert.equal(await frame.count(), 0, "the fallback is remembered");
    await shot("4-fallback");

    // 7. Retry, then a refused probe: the server's words, the isolated browser.
    await pane.getByRole("button", { name: "重试本机渲染", exact: true }).click();
    await frame.waitFor({ timeout: 15000 });
    const refusal = "身份核对未通过：页面看到的时区不是环境时区。已改用隔离浏览器打开。";
    state.hostRender = { ...state.hostRender, refused: refusal, refused_at: new Date().toISOString() };
    await pane.waitFor({ state: "hidden", timeout: 15000 });
    assert.equal(posts("/claude-science/open").length, opens + 2);
    await enter();
    await pane.getByText(refusal).waitFor();

    // 8. Off from the switch under the card: the frame does not come back.
    await toggle.click();
    await until(() => posts("/claude-science/host-render").length === 2, "switch off");
    assert.deepEqual(posts("/claude-science/host-render").at(-1).body, { enabled: false });
    await pane.getByText(refusal).waitFor({ state: "hidden" });
    await pane.locator('input[role="switch"]:not(:checked)').waitFor({ timeout: 15000 });
    assert.equal(await frame.count(), 0);

    // 9. On again, then off from the toolbar's switch.
    await toggle.click();
    await frameReady();
    const before = native.length;
    await pane.getByRole("toolbar").getByRole("switch").click();
    await science.and(page.locator('[data-state="running"]')).waitFor({ timeout: 15000 });
    await until(() => native.slice(before).some(call => call.op === "detach"), "detach after off");
    assert.deepEqual(posts("/claude-science/host-render").at(-1).body, { enabled: false });

    // 10. A refused ticket (the guest browser's identity unreadable): the
    //     server's words and a retry, no browser tab.
    const opensBefore = posts("/claude-science/open").length;
    await page.route("**/api/claude-science/ticket", route => route.fulfill({ status: 409, contentType: "text/plain; charset=utf-8", body: "无法读取环境浏览器的时区与语言，请稍后重试。" }));
    await toggle.click();
    await pane.getByText("无法读取环境浏览器的时区与语言，请稍后重试。").waitFor({ timeout: 15000 });
    assert.equal(posts("/claude-science/open").length, opensBefore, "no browser tab for a refused ticket");
    await page.unroute("**/api/claude-science/ticket");
    await pane.getByRole("button", { name: "重试本机渲染", exact: true }).click();
    await frameReady();

    // 11. The window ends the frame itself (a worker refused the identity).
    const workerRefusal = "页面的一个后台线程没能使用环境的时区与语言，已停止本机渲染。";
    await page.evaluate(text => window.dispatchEvent(new CustomEvent("cage-science-frame", { detail: { error: text } })), workerRefusal);
    await pane.getByText(workerRefusal).waitFor({ timeout: 15000 });
    assert.equal(await frame.count(), 0);
    assert.equal(posts("/claude-science/open").length, opensBefore, "no browser tab when the window ends the frame");
    await pane.getByRole("button", { name: "重试本机渲染", exact: true }).click();
    await frameReady();

    // The audit reports (a tripwire): a note in the toolbar and one
    // diagnostics event with the category only.
    state.hostRender = { ...state.hostRender, audit_hit: "timezone", audit_hits: 1 };
    await pane.getByRole("toolbar").getByText("页面发出过 1 次带有本机时区或语言的请求（已记录，未拦截）").waitFor({ timeout: 15000 });
    await until(() => state.requests.some(request => request.path === "/diagnostics/events" && request.body?.source === "claude-science"), "the audit's diagnostics event");
    state.hostRender = { ...state.hostRender, audit_hit: undefined, audit_hits: undefined };

    // 12. Stop from the toolbar asks first.
    await pane.getByRole("toolbar").getByRole("button", { name: "停止", exact: true }).click();
    await page.getByRole("dialog", { name: "停止 Claude Science？" }).getByRole("button", { name: "停止", exact: true }).click();
    await until(() => posts("/claude-science/stop").length === 1, "stop");
    await frame.waitFor({ state: "detached", timeout: 15000 });

    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
}

// Where the switch never shows, even when it is on: macOS window, a browser
// (no desktop gateway), a remote host.
async function elsewhere() {
  for (const [clientContext, claudeScience] of [
    [{ mode: "local", connection_id: "local", client_platform: "darwin", server_platform: "darwin", desktop_gateway: true, connection: { state: "connected" } }, "running-host-render-available"],
    [{ ...windowsWindow, mode: "remote", connection_id: "remote-1" }, "running-host-render-available"],
    [{ ...windowsWindow, mode: "remote", connection_id: "remote-1" }, "running-host-render"],
    [{ mode: "local", connection_id: "local", client_platform: "windows", server_platform: "windows", connection: { state: "connected" } }, "running-host-render"],
  ]) {
    const { browser, page, native, errors } = await open({ mode: "light", clientContext, claudeScience });
    try {
      await page.getByRole("navigation", { name: "主导航" }).getByRole("button", { name: "Claude Science", exact: true }).click();
      const pane = page.getByRole("main", { name: "Claude Science", exact: true });
      await pane.locator('.claude-science[data-state="running"]').waitFor({ timeout: 15000 });
      await page.waitForTimeout(500);
      assert.equal(await pane.getByRole("switch").count(), 0, clientContext.client_platform + " " + clientContext.mode + " " + claudeScience);
      assert.equal(await pane.locator("iframe").count(), 0);
      assert.deepEqual(native, []);
      assert.deepEqual(errors, []);
    } finally {
      await browser.close();
    }
  }
}

const waitUntil = async (page, check, what) => {
  for (let waited = 0; !check(); waited += 100) {
    if (waited > 15000) throw new Error("timed out: " + what);
    await page.waitForTimeout(100);
  }
};

// F8: a window whose bridge predates the frame contract (it publishes
// none). Off: no switch. On (set from a newer window): the switch stays so
// it can be turned off, says why, and the frame never starts. The bridge is
// never asked: an older window files no error for a hidden feature.
async function olderWindow() {
  for (const claudeScience of ["running-host-render-available", "running-host-render"]) {
    const { browser, page, native, errors, state } = await open({ mode: "light", clientContext: windowsWindow, claudeScience, contract: 0 });
    try {
      await page.getByRole("navigation", { name: "主导航" }).getByRole("button", { name: "Claude Science", exact: true }).click();
      const pane = page.getByRole("main", { name: "Claude Science", exact: true });
      await pane.locator('.claude-science[data-state="running"]').waitFor({ timeout: 15000 });
      await page.waitForTimeout(1000);
      if (claudeScience === "running-host-render-available") {
        assert.equal(await pane.getByRole("switch").count(), 0, "no switch for an older window");
      } else {
        const toggle = pane.getByRole("switch", { name: /本机渲染（实验性）/ });
        assert.equal(await toggle.isChecked(), true);
        await pane.getByText("请更新 Windows 应用后再使用本机渲染：这个窗口还不能把环境的身份只交给 Claude Science 页面。").waitFor();
        await shot(page, "older-window");
        await toggle.click();
        await waitUntil(page, () => state.requests.some(request => request.method === "POST" && request.path === "/claude-science/host-render"), "switch off");
        assert.deepEqual(state.requests.filter(request => request.path === "/claude-science/host-render" && request.method === "POST").map(request => request.body), [{ enabled: false }]);
      }
      assert.equal(await pane.locator("iframe").count(), 0);
      assert.deepEqual(native.map(call => call.op), []);
      assert.deepEqual(errors, []);
    } finally {
      await browser.close();
    }
  }
}

// The agent transport's consumer runs in this window, but an agent's
// scienceFrame request is answered with an error and never reaches the
// bridge (agent-native.js agentNativeAccess; the server refuses it before
// queueing too).
async function agentRefused() {
  const completions = [];
  let handed = false;
  const routes = async (page) => {
    await page.route(url => url.pathname === "/api/agent/native/window", route => route.fulfill({ json: { window_proof: "proof" } }));
    await page.route(url => url.pathname === "/api/agent/native", route => {
      if (handed) return route.fulfill({ json: {} });
      handed = true;
      return route.fulfill({ json: { id: "agent-1", connection_id: "local", request: { action: "scienceFrame", op: "attach", url: `http://${"a".repeat(32)}.localhost:${framePort}/_cage/enter` } } });
    });
    await page.route(url => url.pathname === "/api/agent/native/agent-1/complete", async route => {
      completions.push(route.request().postDataJSON());
      await route.fulfill({ json: {} });
    });
  };
  const { browser, page, native, errors } = await open({ mode: "light", clientContext: windowsWindow, claudeScience: "running-host-render-available", routes });
  try {
    await waitUntil(page, () => completions.length > 0, "the agent's completion");
    assert.deepEqual(completions[0], { error: "这个原生操作只能在桌面窗口中由用户进行。" });
    assert.ok(handed);
    assert.ok(!native.some(call => call.op === "attach"), "the agent reached the bridge");
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
}

const shot = async (page, name) => {
  if (!capture) return;
  await page.waitForTimeout(300);
  await page.screenshot({ path: `${capture}/host-${name}.png` });
};

// The window's document was loaded before the front ran: CSP refuses the
// frame. The pane reloads the window once by itself; a second refusal asks
// instead of showing a blank frame.
async function blockedByPolicy() {
  const { browser, page, native, errors, bridge, state } = await open({ mode: "light", clientContext: windowsWindow, claudeScience: "running-host-render", csp: true });
  bridge.backstop = true;
  try {
    const row = page.getByRole("navigation", { name: "主导航" }).getByRole("button", { name: "Claude Science", exact: true });
    const pane = page.getByRole("main", { name: "Claude Science", exact: true });
    const loaded = page.waitForEvent("load", { timeout: 15000 });
    await row.click();
    await loaded;
    assert.ok(await page.evaluate(() => Number(sessionStorage.getItem("cage-science-frame-reload")) > 0), "reloaded once");
    // The window comes back to the Science page by itself (main.jsx).
    await pane.getByText("窗口打开时本机渲染还没有就绪，重新载入窗口后生效。").waitFor({ timeout: 15000 });
    await pane.getByRole("button", { name: "重新载入窗口", exact: true }).waitFor();
    assert.equal(native.at(-1).op, "detach");
    assert.equal(state.requests.filter(request => request.path === "/claude-science/open").length, 0, "no browser tab for a reload");
    assert.deepEqual(errors.filter(error => !/Content Security Policy/.test(error)), []);
  } finally {
    await browser.close();
  }
}

for (const mode of ["light", "dark"]) {
  await walk(mode);
  checks.push(mode);
}
await elsewhere();
await olderWindow();
await agentRefused();
await blockedByPolicy();
console.log(`PASS: switch off by default (no bridge call), restart asked without the network switches, ticket(proxy, contract)+attach+frame edge to edge, sign-in asks without a user action and moves after a click in the frame, one re-ticket then fallback (remembered), refused probe, off from both switches, audit hits noted in the toolbar and the diagnostics, stop from the toolbar; ${checks.join(" and ")}; no switch on macOS or remote; an older window gets no switch (on: only off, with why); the agent transport cannot drive the frame; CSP refusal reloads once, comes back to the page, then asks`);
