import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import { createWorkbench } from "./workbench-fixture.mjs";
import { claudeScienceFixtures as fixtures } from "./claude-science-fixture.mjs";

// Claude Science pane: unsupported, not installed (with a refused install),
// installing, stopped, running, open (POST and tab switch), update available.
// Runs against the fixture's mocked /api/claude-science in light and dark.
const capture = process.env.CAGE_TEST_CAPTURE;
if (capture) await mkdir(capture, { recursive: true });
const checks = [];

async function walk(mode) {
  const { browser, page, state, errors } = await createWorkbench({
    viewport: { width: 1280, height: 800 },
    claudeScience: "unsupported-architecture",
    beforeNavigate: async (page, state) => {
      state.appearance.mode = mode;
      state.status.ready = state.status.vm_running = true;
      // Headless Chrome has no audio device; a valid format keeps the probe quiet.
      await page.route("**/api/audio", route => route.fulfill({ json: { format: "s16le", sample_rate: 48000, channels: 2 } }));
    },
  });
  const consoleLines = [];
  page.on("console", message => consoleLines.push(message.text()));
  const posts = () => state.requests.filter(request => request.method === "POST" && request.path.startsWith("/claude-science/"));
  const shot = async name => {
    if (!capture) return;
    await page.waitForTimeout(300);
    await page.screenshot({ path: `${capture}/${mode}-${name}.png` });
  };
  try {
    const nav = page.getByRole("navigation", { name: "主导航" });
    const row = nav.getByRole("button", { name: "Claude Science", exact: true });
    const pane = page.getByRole("main", { name: "Claude Science", exact: true });
    const card = pane.locator(".claude-science");
    const enter = async () => {
      if (await pane.isVisible()) await nav.getByRole("button", { name: "工作台", exact: true }).click();
      await row.click();
      await pane.waitFor();
    };
    const stateIs = name => card.and(page.locator(`[data-state="${name}"]`)).waitFor({ timeout: 15000 });
    const button = name => card.getByRole("button", { name, exact: true });
    const alertText = text => pane.getByRole("alert").filter({ hasText: text }).waitFor({ timeout: 15000 });

    // The row sits right after Claude Desktop in the first group.
    await row.waitFor();
    const group = await nav.locator(".sidebar-nav-group").first().getByRole("button").allTextContents();
    assert.deepEqual(group.map(text => text.trim()).filter(Boolean).slice(0, 4), ["工作台", "浏览器", "Claude Desktop", "Claude Science"]);

    // 1. Unsupported: the server's reason, nothing to press.
    await row.click();
    await pane.waitFor();
    assert.equal(await row.getAttribute("aria-current"), "page");
    await stateIs("unsupported");
    await card.getByText(fixtures["unsupported-architecture"].body.unsupported_reason, { exact: true }).waitFor();
    assert.equal(await card.getByRole("button").count(), 0, "unsupported offers no action");
    assert.ok(await card.locator(".drawing").count() > 0, "hero draws a brush drawing");
    await shot("1-unsupported");

    // 2. A guest read failure (502) says so and retries.
    state.claudeScience = "unreachable";
    await enter();
    await stateIs("unreachable");
    await card.getByText(fixtures.unreachable.text, { exact: true }).waitFor();
    const reads = () => state.requests.filter(request => request.method === "GET" && request.path === "/claude-science").length;
    const before = reads();
    await button("重新读取状态").click();
    await stateIs("unreachable");
    assert.ok(reads() > before, "retry reads the status again");
    // Once the guest answers, the next read recovers the pane by itself.
    state.claudeScience = "not-installed";

    // 3. Not installed: disk needed against disk free.
    await stateIs("not-installed");
    await card.getByText("至少 11 GB", { exact: true }).waitFor();
    await card.getByText("38 GB", { exact: true }).waitFor();
    assert.ok(await button("安装 Claude Science").isEnabled());
    await shot("2-not-installed");

    // 4. A refused install shows the server's reason, the last line of the log.
    const reason = "Claude Science 的沙箱需要 bubblewrap 0.8 或更高版本，这个客机系统自带的版本过旧（Ubuntu 22.04 即是如此）。请改用 Debian 13 客机。";
    let installs = 0;
    const installBodies = [];
    await page.route("**/api/claude-science/install", route => {
      installBodies.push(route.request().postDataJSON());
      installs++;
      if (installs === 1) {
        state.status.job = { id: "sci-1", kind: "claude-science-install", running: false, error: "维护操作失败", message: "安装 Claude Science 失败", log: "检查客机环境\n读取系统版本\n" + reason + "\n" };
      } else {
        state.claudeScience = "installing";
        state.status.job = { id: "sci-2", kind: "claude-science-install", running: true, message: "正在下载 Claude Science 0.1.73（62%）",
          log: "检查客机环境\n磁盘空间足够：38 GB 可用\n下载 claude-science-linux-x64 0.1.73\n校验 sha256\n正在准备 Python 与 R 环境…\n" };
      }
      return route.fulfill({ json: { id: state.status.job.id, kind: state.status.job.kind, running: false } });
    });
    await button("安装 Claude Science").click();
    await alertText(reason);
    await pane.getByRole("alert").getByText("安装未完成", { exact: true }).waitFor();
    assert.ok(await button("安装 Claude Science").isEnabled(), "a refusal leaves the install available");
    await pane.getByRole("alert").getByRole("button", { name: "操作详情", exact: true }).click();
    await page.getByRole("dialog").getByText(reason).waitFor();
    await page.getByRole("dialog").getByRole("button", { name: "关闭", exact: true }).click().catch(() => page.keyboard.press("Escape"));
    await page.getByRole("dialog").waitFor({ state: "hidden" });
    await shot("3-install-refused");

    // 5. Installing: the job's progress and log, no action to press.
    await button("安装 Claude Science").click();
    await stateIs("installing");
    await card.getByText("正在下载 Claude Science 0.1.73（62%）", { exact: true }).waitFor({ timeout: 15000 });
    await card.getByLabel("操作日志").getByText("校验 sha256").waitFor();
    assert.equal(await card.locator(".clay").count(), 0, "no call to action while installing");
    assert.ok(await row.isEnabled(), "the row stays reachable while a job runs, so its progress can be reopened");
    await shot("4-installing");
    await button("查看进度").click();
    await page.getByRole("dialog", { name: "操作详情" }).getByText("正在准备 Python 与 R 环境…").waitFor();
    await page.keyboard.press("Escape");
    await page.getByRole("dialog").waitFor({ state: "hidden" });
    assert.deepEqual(installBodies, [{}, {}], "install posts an empty body");

    // 6. The job ends: installed and stopped.
    state.claudeScience = "stopped";
    state.status.job = { id: "sci-2", kind: "claude-science-install", running: false, message: "Claude Science 已安装" };
    await button("启动").waitFor({ timeout: 15000 });
    await stateIs("stopped");
    await card.getByText("v0.1.73", { exact: true }).waitFor();
    await shot("5-stopped");

    // 7. Start: the fixture runs it signed out.
    await button("启动").click();
    await stateIs("running");
    assert.deepEqual(posts().map(request => [request.path, request.body]), [["/claude-science/start", {}]]);
    await card.getByText("登录在打开的页面中完成，整个过程发生在隔离浏览器内，与 Claude Desktop 的登录方式相同。", { exact: true }).waitFor();
    assert.ok(await button("停止").evaluate(element => element.classList.contains("quiet-button")), "stop is the quiet action");
    assert.equal(await card.locator(".clay").count(), 1, "one call to action");
    await shot("6-running");

    // 8. Open: POST, then the browser pane shows the returned tab.
    await button("打开 Claude Science").click();
    await pane.waitFor({ state: "hidden" });
    assert.deepEqual(posts().at(-1), { path: "/claude-science/open", method: "POST", body: {} });
    const tab = state.tabs.find(item => item.kind === "science");
    assert.ok(tab, "the server made a science tab");
    assert.equal(state.tabs.filter(item => item.kind === "science").length, 1);
    await page.getByRole("tab", { name: "Claude Science", exact: true }).and(page.locator('[aria-selected="true"]')).waitFor({ timeout: 15000 });
    assert.equal(await nav.getByRole("button", { name: "Claude Science", exact: true }).getAttribute("aria-current"), null);
    await shot("7-opened");
    // Opening again brings the same tab forward.
    await enter();
    await stateIs("running");
    await button("打开 Claude Science").click();
    await pane.waitFor({ state: "hidden" });
    assert.equal(state.tabs.filter(item => item.kind === "science").length, 1, "a second open reuses the tab");
    assert.equal(state.active, tab.id);

    // 9. A refused open (the browser is not ready) shows the server's words.
    await enter();
    await page.route("**/api/claude-science/open", route => route.fulfill({ status: 409, contentType: "text/plain; charset=utf-8", body: "浏览器尚未就绪，请稍后再试。" }));
    await button("打开 Claude Science").click();
    await alertText("浏览器尚未就绪，请稍后再试。");
    assert.ok(await button("打开 Claude Science").isEnabled());
    await page.unroute("**/api/claude-science/open");

    // 10. An update is offered next to the version.
    await page.route("**/api/claude-science", route => route.request().method() === "GET"
      ? route.fulfill({ json: { ...fixtures["running-signed-out"].body, version: "0.1.72", update_available: true } })
      : route.fallback());
    await enter();
    await card.getByText("有新版本", { exact: true }).waitFor();
    await card.getByText("v0.1.72", { exact: true }).waitFor();
    await button("更新到 0.1.73").waitFor();
    assert.ok(await button("打开 Claude Science").isEnabled(), "an update never blocks opening");
    await shot("8-update-available");
    await button("更新到 0.1.73").click();
    await page.getByRole("dialog", { name: "更新 Claude Science？" }).waitFor();
    await page.getByRole("dialog").getByRole("button", { name: "取消", exact: true }).click();
    assert.equal(posts().filter(request => request.path === "/claude-science/update").length, 0, "cancel posts nothing");
    await button("更新到 0.1.73").click();
    await page.getByRole("dialog").getByRole("button", { name: "更新", exact: true }).click();
    for (let waited = 0; posts().at(-1)?.path !== "/claude-science/update" && waited < 10000; waited += 100) await page.waitForTimeout(100);
    assert.deepEqual(posts().at(-1), { path: "/claude-science/update", method: "POST", body: {} });
    await page.unroute("**/api/claude-science");
    await stateIs("stopped");

    // 11. A job running elsewhere keeps the pane readable and its actions off.
    state.status.job = { id: "other-1", kind: "settings", running: true, message: "正在应用资源配置", log: "…" };
    await card.locator(".science-progress").getByText("正在应用资源配置").waitFor({ timeout: 15000 });
    assert.ok(await button("启动").isDisabled());
    await shot("9-elsewhere");
    state.status.job = { id: "other-1", kind: "settings", running: false };
    await button("启动").waitFor();

    assert.deepEqual(errors, []);
    for (const line of consoleLines) assert.ok(!/reused|science-1|47800|token/i.test(line), "no response body in the console: " + line);
  } finally {
    await browser.close();
  }
}

for (const mode of ["light", "dark"]) {
  await walk(mode);
  checks.push(mode);
}
console.log(`PASS: unsupported, 502 retry, not installed, refused install with the log's last line, installing with progress and log, stopped, start, running, open (POST and tab switch, reused), refused open, update available with confirm, other job running; ${checks.join(" and ")}`);
