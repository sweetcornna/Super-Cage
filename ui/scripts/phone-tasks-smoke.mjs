// Phone: subagents and background tasks in a conversation, against Vite with the
// API mocked (CAGE_UI_URL, CAGE_PLAYWRIGHT_MODULE, CAGE_TEST_BROWSER_PATH as for
// the other phone smokes; CAGE_TEST_CAPTURE keeps the screenshots).
// Walks: nothing running -> one subagent running with a changing "now" line ->
// a background command still running after the reply ended (strip, header count,
// sheet, session-list marker) -> both ended, one failed (cards, sheet, no strip)
// -> five running (three rows and "还有 2 个") -> a short screen (keyboard open,
// landscape) where the strip keeps one row. At 360x640 and 390x844 the last
// message, the strip and the composer must not overlap.
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { join } from "node:path";
import { smokeCaptureDirectory } from "./smoke-captures.mjs";

const require = createRequire(import.meta.url);
const engines = require(process.env.CAGE_PLAYWRIGHT_MODULE || "playwright");
const engine = process.env.CAGE_TEST_BROWSER || "chromium";
const browser = await engines[engine].launch(engine === "chromium" && process.env.CAGE_TEST_BROWSER_PATH ? { executablePath: process.env.CAGE_TEST_BROWSER_PATH } : {});
const output = await smokeCaptureDirectory("phone-tasks");
const origin = process.env.CAGE_UI_URL || "http://127.0.0.1:5361";
const id = "5e551011-0000-4000-8000-000000000001";
const workspace = "/work/alpha";
const now = () => Date.now() / 1000;
const errors = [];
const passed = [];

function makeState() {
  let revision = 1;
  const state = { session: null, list: null };
  state.set = (session, listExtra = {}) => {
    revision += 1;
    state.session = { id, workspace, title: "整理发布流程", generation: "c0ffee00-0000-4000-8000-000000000009", revision, updated_at: new Date().toISOString(),
      provider: "", can_send: true, active: false, ...session };
    state.list = { id, workspace, title: "整理发布流程", generation: state.session.generation, revision, updated_at: state.session.updated_at, pinned: false, active: false, ...listExtra };
  };
  return state;
}

const turn1 = { id: "c0ffee00-0000-4000-8000-000000000101", prompt: "先看看项目结构", status: "completed", text: "项目分为 **src** 与 **docs** 两部分。",
  tools: [{ id: "t1", name: "Read", detail: "README.md", status: "completed" }, { id: "t2", name: "Grep", detail: "TODO", status: "completed" }] };
const prompt2 = "梳理目录，然后后台构建发布包";
const agent = (extra) => ({ id: "agent-1", name: "Agent", status: "completed", kind: "subagent", state: "running", detail: "梳理目录结构", agent_type: "Explore", started_at: now() - 12, ...extra });
const build = (extra) => ({ id: "bash-1", name: "Bash", status: "completed", kind: "background_command", state: "running", detail: "构建发布包", started_at: now() - 40, ...extra });

async function open(page, state) {
  await page.route("**/api/**", (route) => {
    const request = route.request(), url = new URL(request.url()), path = url.pathname;
    if (path.endsWith("/cli/projects")) return route.fulfill({ json: { host: "测试电脑", ready: true, shared_sessions: true, projects: [{ path: workspace, name: "alpha" }] } });
    if (path.endsWith("/cli/sessions")) return route.fulfill({ json: { sessions: [state.list] } });
    if (path.endsWith("/account")) return route.fulfill({ json: { session: state.session, supported: false, candidates: [] } });
    if (path.endsWith("/cli/sessions/" + id)) {
      if (url.searchParams.get("after") === String(state.session.revision)) return route.fulfill({ status: 204 });
      return route.fulfill({ json: state.session });
    }
    return route.fulfill({ json: {} });
  });
  // The workbench serves the phone page at "/" to phones (the phone workspace:
  // drawer, home, recent list); Vite serves it as mobile.html.
  await page.route(origin + "/", async (route) => route.fulfill({ response: await route.fetch({ url: origin + "/mobile.html" }) }));
  await page.goto(origin + "/");
  await page.locator(".recent").getByRole("button", { name: /^整理发布流程/ }).click();
  await page.getByRole("region", { name: "会话消息" }).waitFor();
}

// Waits for sheets and drawers to finish moving (the breathing dot runs on).
const settle = (page) => page.waitForFunction(() => document.getAnimations().every((a) => a.playState !== "running" || a.effect?.getTiming?.().iterations === Infinity));
const shot = async (page, name) => { await settle(page); const { width, height } = page.viewportSize(); return page.screenshot({ path: join(output, width + "x" + height + "-" + name + ".png") }); };

// The last message, the strip and the composer do not overlap, and all lie on screen.
async function layout(page, label) {
  const transcript = page.getByRole("region", { name: "会话消息" });
  await transcript.evaluate((box) => { box.scrollTop = box.scrollHeight; });
  await page.waitForTimeout(150);
  const view = page.viewportSize();
  const region = await transcript.boundingBox();
  const last = await page.locator("article.message").last().boundingBox();
  const composer = await page.getByRole("textbox", { name: "消息", exact: true }).boundingBox();
  const strip = await page.locator(".task-strip").count() ? await page.locator(".task-strip").boundingBox() : null;
  const bottomOf = (b) => b.y + b.height;
  assert.ok(composer && composer.y >= 0 && bottomOf(composer) <= view.height + 0.5, label + ": the composer is on screen");
  assert.ok(last && bottomOf(last) > region.y + 8 && last.y < bottomOf(region) - 8, label + ": the last message is visible in the transcript");
  assert.ok(bottomOf(region) <= composer.y + 0.5, label + ": the transcript ends above the composer");
  if (strip) {
    assert.ok(bottomOf(region) <= strip.y + 0.5, label + ": the strip does not cover the transcript");
    assert.ok(bottomOf(strip) <= composer.y + 0.5, label + ": the strip does not cover the composer");
    assert.ok(region.height >= 80, label + ": the transcript keeps room (" + region.height + " px)");
  }
}

try {
  for (const size of [{ width: 390, height: 844 }, { width: 360, height: 640 }]) {
    const tag = size.width + "x" + size.height;
    const page = await browser.newPage({ viewport: size, isMobile: true, hasTouch: true, colorScheme: "light" });
    page.on("pageerror", (error) => errors.push(error.message));
    const state = makeState();
    state.set({ turns: [turn1] });
    await open(page, state);

    // 1. Nothing runs: no strip, the ordinary calls in today's collapsed list.
    await page.locator("summary").filter({ hasText: "2 次工具调用" }).waitFor();
    assert.equal(await page.locator(".task-strip").count(), 0, "no strip while nothing runs");
    assert.equal(await page.locator(".task-cards").count(), 0);
    await layout(page, tag + " idle");
    await shot(page, "1-idle");

    // 2. A subagent runs, its "now" line changes.
    const running = (extra) => ({ ...turn1, id: "c0ffee00-0000-4000-8000-000000000102", prompt: prompt2, status: "running", text: "",
      tools: [agent(extra), { id: "t3", name: "Read", detail: "package.json", status: "completed" }] });
    state.set({ active: true, turns: [turn1, running({ now: { name: "Read", detail: "src/app.py" }, calls: 3 })], tasks: { count: 1, running: [agent({ now: { name: "Read", detail: "src/app.py" }, calls: 3 })] } });
    const row = page.locator(".task-strip").getByRole("button", { name: /子智能体：梳理目录结构，运行中/ });
    await row.waitFor();
    assert.match(await row.getAttribute("aria-label"), /正在 Read · src\/app\.py/);
    await page.getByRole("list", { name: "本轮启动的任务" }).getByRole("button", { name: /梳理目录结构，运行中/ }).waitFor();
    await page.locator("summary").filter({ hasText: "1 次工具调用" }).waitFor();
    state.set({ active: true, turns: [turn1, running({ now: { name: "Grep", detail: "TODO" }, calls: 5 })], tasks: { count: 1, running: [agent({ now: { name: "Grep", detail: "TODO" }, calls: 5 })] } });
    await page.locator(".task-strip .task-now").filter({ hasText: "Grep · TODO" }).waitFor({ timeout: 8000 });
    await layout(page, tag + " subagent");
    await shot(page, "2-subagent-running");

    // 3. The reply has ended; a background command still runs.
    const t = Math.floor(now());
    const replied = { ...running({}), status: "completed", text: "目录已梳理完，发布包在后台构建。",
      tools: [agent({ state: "completed", started_at: t - 12, ended_at: t - 2 }), build({}), { id: "t3", name: "Read", detail: "package.json", status: "completed" }] };
    state.set({ turns: [turn1, replied], tasks: { count: 1, running: [build({})] } }, { running_tasks: 1 });
    const buildRow = page.locator(".task-strip").getByRole("button", { name: /后台命令：构建发布包，运行中/ });
    await buildRow.waitFor({ timeout: 8000 });
    await page.getByRole("status").filter({ hasText: "1 个任务运行中" }).first().waitFor();
    const before = await page.locator(".task-strip .task-time").first().innerText();
    await page.waitForTimeout(2200);
    const after = await page.locator(".task-strip .task-time").first().innerText();
    assert.notEqual(before, after, "the elapsed time moves while the command runs (" + before + ")");
    await layout(page, tag + " background");
    await shot(page, "3-background-after-reply");
    await buildRow.click();
    const sheet = page.getByRole("dialog", { name: "子智能体与后台任务" });
    await sheet.waitFor();
    await sheet.getByRole("listitem", { name: /后台命令：构建发布包，运行中，已运行/ }).waitFor();
    await sheet.getByRole("listitem", { name: /子智能体：梳理目录结构，已完成，用时 1\d 秒/ }).waitFor();
    await shot(page, "3-sheet");
    await sheet.getByRole("button", { name: "关闭", exact: true }).click();
    await sheet.waitFor({ state: "hidden" });
    // The session list says so too.
    await page.getByRole("button", { name: "展开侧栏", exact: true }).click();
    const drawer = page.getByRole("dialog", { name: "工作区侧栏" });
    await drawer.getByRole("button", { name: "整理发布流程，1 个任务运行中" }).first().waitFor({ timeout: 8000 });
    await drawer.locator(".task-marker").filter({ hasText: "1 个任务运行中" }).first().waitFor();
    await shot(page, "3-session-list");
    await drawer.getByRole("button", { name: "新对话", exact: true }).click();
    await drawer.waitFor({ state: "hidden" });
    // And the recent list on the start page.
    const recent = page.locator(".recent").getByRole("button", { name: "整理发布流程，1 个任务运行中" });
    await recent.waitFor();
    await page.locator(".recent .task-marker").filter({ hasText: "1 个任务运行中" }).waitFor();
    await shot(page, "3-home-recent");
    await recent.click();
    await page.getByRole("region", { name: "会话消息" }).waitFor();

    // 4. Both ended, the command failed: no strip, cards with their outcome.
    const ended = { ...replied, tools: [replied.tools[0], build({ state: "failed", started_at: t - 40, ended_at: t - 5 }), replied.tools[2]] };
    state.set({ turns: [turn1, ended, { id: "c0ffee00-0000-4000-8000-000000000103", prompt: "<task-notification>\n<task-id>bk1</task-id>\n<status>failed</status>\n<summary>Background command failed with exit code 3</summary>\n</task-notification>", status: "completed", text: "构建失败了，退出码 3。" }] });
    await page.locator(".task-strip").waitFor({ state: "detached", timeout: 8000 });
    const cards = page.getByRole("list", { name: "本轮启动的任务" });
    await cards.getByRole("button", { name: /后台命令：构建发布包，失败，用时 3\d 秒/ }).waitFor();
    await cards.getByRole("button", { name: /子智能体：梳理目录结构，已完成/ }).waitFor();
    assert.equal(await page.getByRole("status").filter({ hasText: "任务运行中" }).count(), 0);
    await layout(page, tag + " ended");
    await shot(page, "4-ended");
    await cards.getByRole("button", { name: /后台命令：构建发布包/ }).click();
    await sheet.getByRole("listitem", { name: /后台命令：构建发布包，失败/ }).waitFor();
    assert.equal(await sheet.getByRole("region", { name: "运行中" }).count(), 0);
    await shot(page, "4-sheet");
    await sheet.getByRole("button", { name: "关闭", exact: true }).click();

    // 5. Five run: three rows and "还有 2 个".
    const many = [0, 1, 2, 3, 4].map((i) => agent({ id: "agent-m" + i, detail: "子任务 " + (i + 1), started_at: now() - 5 * (i + 1), now: { name: "Bash", detail: "运行测试" } }));
    state.set({ turns: [turn1, { ...replied, tools: [...many, ...replied.tools.slice(2)] }], tasks: { count: 5, running: many } }, { running_tasks: 5 });
    await page.locator(".task-strip .task-more.full").filter({ hasText: "还有 2 个" }).waitFor({ timeout: 8000 });
    assert.equal(await page.locator(".task-strip li").count(), 3);
    await layout(page, tag + " five");
    await shot(page, "5-five-running");

    // 6. Keyboard open, then landscape: one row, nothing covered.
    for (const [name, small] of [["keyboard", { width: size.width, height: Math.round(size.height * 0.5) }], ["landscape", { width: size.height, height: size.width }]]) {
      await page.setViewportSize(small);
      await page.waitForTimeout(300);
      if (name === "keyboard") await page.getByRole("textbox", { name: "消息", exact: true }).focus();
      assert.equal(await page.locator(".task-strip li:visible").count(), 1, name + ": one row");
      await page.locator(".task-strip .task-more.compact").filter({ hasText: "还有 4 个" }).waitFor();
      await layout(page, tag + " " + name);
      await shot(page, "6-" + name);
    }
    await page.close();
    passed.push(tag);
  }
  // An older server's session (no task fields) looks as before.
  const page = await browser.newPage({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, colorScheme: "dark" });
  page.on("pageerror", (error) => errors.push(error.message));
  const state = makeState();
  state.set({ turns: [turn1, { ...turn1, id: "c0ffee00-0000-4000-8000-000000000104", tools: [{ id: "x1", name: "Agent", detail: "梳理目录结构", status: "called" }, { id: "x2", name: "Bash", detail: "构建发布包", status: "completed" }] }] });
  await open(page, state);
  await page.locator("summary").filter({ hasText: "2 次工具调用" }).nth(1).waitFor();
  assert.equal(await page.locator(".task-strip, .task-cards, .task-count, .task-marker").count(), 0, "an older server's answer shows no task parts");
  await shot(page, "7-older-server-dark");
  await page.close();
  passed.push("older server");
  assert.deepEqual(errors, []);
  console.log(engine + " phone tasks smoke passed: " + passed.join(", ") + "; screenshots in " + output);
} catch (error) {
  for (const page of browser.contexts().flatMap((c) => c.pages())) {
    await page.screenshot({ path: join(output, "failure.png") }).catch(() => {});
    console.error("viewport", page.viewportSize(), "labels:", await page.locator("[aria-label]").evaluateAll((list) => list.map((e) => e.getAttribute("aria-label")).filter((l) => /：/.test(l))).catch(() => []));
  }
  throw error;
} finally {
  await browser.close();
}
