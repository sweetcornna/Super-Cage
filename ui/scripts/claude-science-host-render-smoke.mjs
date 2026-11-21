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
const frameOrigin = "http://localhost:17840";
const windowsWindow = { mode: "local", connection_id: "local", client_platform: "windows", server_platform: "windows", desktop_gateway: true, connection: { state: "connected" } };
const checks = [];

async function open({ mode, clientContext, claudeScience, csp = false }) {
  const native = [];
  const bridge = { backstop: false };
  const workbench = await createWorkbench({
    viewport: { width: 1280, height: 800 },
    claudeScience,
    clientContext,
    nativeRequest: async (body) => {
      if (body.action !== "scienceFrame") return null;
      native.push({ op: body.op, url: body.url });
      if (body.op === "prepare") return { backstop: bridge.backstop };
      if (body.op === "arm" && !String(body.url).startsWith(frameOrigin + "/_cage/enter#")) throw new Error("unexpected frame address");
      return true;
    },
    beforeNavigate: async (page, state) => {
      state.appearance.mode = mode;
      state.status.ready = state.status.vm_running = true;
      state.frameOrigin = frameOrigin;
      await page.route("**/api/audio", route => route.fulfill({ json: { format: "s16le", sample_rate: 48000, channels: 2 } }));
      await page.route(frameOrigin + "/**", route => route.fulfill({ contentType: "text/html; charset=utf-8",
        body: "<!doctype html><html lang=zh-CN><title>Claude Science</title><body style='margin:0;display:grid;place-items:center;height:100vh;font:16px system-ui;background:#fff;color:#1f1e1d'>Claude Science 页面（测试替身）</body></html>" }));
      if (csp) await page.route(url => url.pathname === "/" || url.pathname === "/index.html", async route => {
        const response = await route.fetch();
        await route.fulfill({ response, headers: { ...response.headers(), "content-security-policy": "frame-src 'none'" } });
      });
    },
  });
  return { ...workbench, native, bridge };
}

async function walk(mode) {
  const { browser, page, state, errors, native, bridge } = await open({ mode, clientContext: windowsWindow, claudeScience: "running-host-render-available" });
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
    assert.deepEqual(native, [], "off never calls the bridge");
    await shot("1-off");

    // 2. On, but the window still runs without the network switches: one
    //    reload of the window, asked for, never armed.
    await toggle.click();
    await pane.getByText("本机渲染已开启，需要重新载入窗口一次才能生效。").waitFor({ timeout: 15000 });
    assert.deepEqual(posts("/claude-science/host-render").map(request => request.body), [{ enabled: true }]);
    assert.deepEqual(ops(), ["prepare", "disarm"]);
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
    assert.deepEqual(ops(), ["prepare", "arm"]);
    const ticket = posts("/claude-science/ticket").at(-1).body;
    assert.deepEqual(Object.keys(ticket.host).sort(), ["languages", "offset", "timezone"]);
    assert.equal(typeof ticket.host.offset, "number");
    assert.equal(native[1].url, await frame.getAttribute("src"));
    assert.equal(await frame.getAttribute("sandbox"), "allow-scripts allow-same-origin allow-forms allow-modals");
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

    // 4. Sign-in inside the page: the server opened the guest tab; the pane
    //    shows it, and leaving the pane drops the overrides and the session.
    const releases = posts("/claude-science/release").length;
    state.tabs.push({ id: "science-login", kind: "science", title: "Claude Science", url: "http://localhost:47800/" });
    state.hostRender = { ...state.hostRender, signin: { at: new Date().toISOString(), tab: { id: "science-login", kind: "science", title: "Claude Science" } } };
    await pane.waitFor({ state: "hidden", timeout: 15000 });
    await page.getByRole("tab", { name: "Claude Science", exact: true }).and(page.locator('[aria-selected="true"]')).waitFor({ timeout: 15000 });
    await until(() => ops().at(-1) === "disarm" && posts("/claude-science/release").length === releases + 1, "disarm and release");

    // 5. Back to the pane: a new ticket and the same sign-in is not replayed.
    const tickets = posts("/claude-science/ticket").length;
    await enter();
    await frameReady();
    assert.equal(posts("/claude-science/ticket").length, tickets + 1);
    await page.waitForTimeout(2500);
    assert.ok(await pane.isVisible(), "an old sign-in event does not leave the pane again");

    // 6. The front restarted: one new ticket, then the isolated browser.
    state.hostRender = { ...state.hostRender, session: false };
    await until(() => posts("/claude-science/ticket").length === tickets + 2, "re-ticket");
    await frameReady();
    const opens = posts("/claude-science/open").length;
    state.hostRender = { ...state.hostRender, session: false };
    await pane.waitFor({ state: "hidden", timeout: 15000 });
    assert.equal(posts("/claude-science/open").length, opens + 1, "fell back to the isolated browser");
    assert.equal(posts("/claude-science/ticket").length, tickets + 2, "only one re-ticket");
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
    await until(() => native.slice(before).some(call => call.op === "disarm"), "disarm after off");
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

    // 11. Stop from the toolbar asks first.
    await pane.getByRole("toolbar").getByRole("button", { name: "停止", exact: true }).click();
    await page.getByRole("dialog", { name: "停止 Claude Science？" }).getByRole("button", { name: "停止", exact: true }).click();
    await until(() => posts("/claude-science/stop").length === 1, "stop");
    await frame.waitFor({ state: "detached", timeout: 15000 });

    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
}

// Where the switch never shows: macOS window, a browser, a remote host.
async function elsewhere() {
  for (const clientContext of [
    { mode: "local", connection_id: "local", client_platform: "darwin", server_platform: "darwin", desktop_gateway: true, connection: { state: "connected" } },
    { ...windowsWindow, mode: "remote", connection_id: "remote-1" },
  ]) {
    const { browser, page, native, errors } = await open({ mode: "light", clientContext, claudeScience: "running-host-render-available" });
    try {
      await page.getByRole("navigation", { name: "主导航" }).getByRole("button", { name: "Claude Science", exact: true }).click();
      const pane = page.getByRole("main", { name: "Claude Science", exact: true });
      await pane.locator('.claude-science[data-state="running"]').waitFor({ timeout: 15000 });
      await page.waitForTimeout(500);
      assert.equal(await pane.getByRole("switch").count(), 0, clientContext.client_platform + " " + clientContext.mode);
      assert.deepEqual(native, []);
      assert.deepEqual(errors, []);
    } finally {
      await browser.close();
    }
  }
}

// The window's document was loaded before the front ran: CSP refuses the
// frame, and the pane asks for a reload instead of showing a blank frame.
async function blockedByPolicy() {
  const { browser, page, native, errors, bridge, state } = await open({ mode: "light", clientContext: windowsWindow, claudeScience: "running-host-render", csp: true });
  bridge.backstop = true;
  try {
    await page.getByRole("navigation", { name: "主导航" }).getByRole("button", { name: "Claude Science", exact: true }).click();
    const pane = page.getByRole("main", { name: "Claude Science", exact: true });
    await pane.getByText("窗口打开时本机渲染还没有就绪，重新载入窗口后生效。").waitFor({ timeout: 15000 });
    await pane.getByRole("button", { name: "重新载入窗口", exact: true }).waitFor();
    assert.equal(native.at(-1).op, "disarm");
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
await blockedByPolicy();
console.log(`PASS: switch off by default, restart asked without the network switches, ticket+arm+frame edge to edge, sign-in hands over to the browser pane, one re-ticket then fallback (remembered), refused probe, off from both switches, stop from the toolbar; ${checks.join(" and ")}; no switch on macOS or remote; CSP refusal asks for a reload`);
