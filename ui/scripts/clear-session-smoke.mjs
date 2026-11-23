// 清除前的对话: what /clear left behind in a session, on the desktop sidebar and
// the phone (list row actions and the conversation), against Vite with the API
// mocked (CAGE_UI_URL, CAGE_PLAYWRIGHT_MODULE, CAGE_TEST_BROWSER_PATH;
// CAGE_TEST_CAPTURE keeps the screenshots). A session with two earlier parts
// offers the list, each part opens read only, and 恢复为单独会话 asks the server
// to separate it; a session that was never cleared offers nothing new.
import assert from "node:assert/strict";
import { join } from "node:path";
import { smokeCaptureDirectory } from "./smoke-captures.mjs";
import { createWorkbench } from "./workbench-fixture.mjs";

const base = process.env.CAGE_UI_URL || "http://127.0.0.1:5331";
const output = await smokeCaptureDirectory("clear-session");
const id = "5e551011-0000-4000-8000-0000000000c1";
const plain = "5e551011-0000-4000-8000-0000000000c2";
const parts = () => [
  { cli_id: "c1ea4ed0-0000-4000-8000-000000000002", cleared_at: new Date(Date.now() - 20 * 60000).toISOString(), prompt: "把发布脚本改成先跑测试", turns: 2 },
  { cli_id: "c1ea4ed0-0000-4000-8000-000000000001", cleared_at: new Date(Date.now() - 26 * 3600000).toISOString(), prompt: "先看看项目结构", turns: 1 },
];
const view = (cli) => ({ ...parts().find((part) => part.cli_id === cli), read_only: true, items: [
  { id: "t1", prompt: cli.endsWith("2") ? "把发布脚本改成先跑测试" : "先看看项目结构", status: "completed", text: "已调整：发布前先运行 npm test，失败时停止。" },
  ...(cli.endsWith("2") ? [{ id: "t2", prompt: "顺便把日志写到文件", status: "completed", text: "日志现在写到 release.log。" }] : []),
] });

// One mocked API for both pages: the sessions, their earlier parts and the separation.
function earlierRoutes(state, workspace) {
  return (path, method) => {
    const match = path.match(/\/sessions\/([^/]+)\/earlier(?:\/([^/]+))?(?:\/(separate))?$/);
    if (!match) return null;
    const [, session, cli, action] = match;
    state.requests.push({ method, session, cli: cli || "", action: action || "" });
    if (session !== id) return { status: 404, body: "没有这段会话" };
    if (action === "separate") {
      state.parts = state.parts.filter((part) => part.cli_id !== cli);
      state.sessions[0].earlier_parts = state.parts.length;
      const separated = { id: "5e551011-0000-4000-8000-0000000000c3", workspace, title: "先看看项目结构", revision: 1, updated_at: new Date().toISOString(), pinned: false };
      state.sessions.push(separated);
      return { json: separated };
    }
    if (cli) return { json: view(cli) };
    return { json: { parts: state.parts } };
  };
}

function makeState(workspace) {
  const state = { parts: parts(), requests: [], sessions: [
    { id, workspace, title: "整理发布流程", custom_title: true, pinned: true, revision: 3, generation: id, updated_at: new Date().toISOString(), earlier_parts: 2 },
    { id: plain, workspace, title: "另一段会话", revision: 1, generation: plain, updated_at: new Date(Date.now() - 3600000).toISOString(), pinned: false },
  ] };
  state.earlier = earlierRoutes(state, workspace);
  return state;
}

const errors = [];
const settle = (page) => page.waitForFunction(() => document.getAnimations().every((a) => a.playState !== "running" || a.effect?.getTiming?.().iterations === Infinity));
const shot = async (page, name) => { await settle(page); await page.screenshot({ path: join(output, name + ".png") }); };

// The desktop sidebar: the row's actions offer 清除前的对话 only where something was cleared.
// The fixture's workspace (workbench-fixture.mjs settings.workspace).
const desktopState = makeState("/Users/shihao/.claude-cage-workspace");
const desktop = await createWorkbench({ url: base, cliRequest: async (route, path) => {
  const method = route.request().method();
  const answer = desktopState.earlier(path, method);
  if (answer) return route.fulfill(answer);
  if (path === "/cli/sessions") return route.fulfill({ json: { sessions: desktopState.sessions } });
  const account = path.match(/^\/cli\/sessions\/([^/]+)\/account$/);
  if (account) return route.fulfill({ json: { session: { id: account[1] }, supported: false, candidates: [] } });
  return route.fulfill({ json: { sessions: [], notice: "" } });
} });
const { page, state, browser } = desktop;
assert.equal(state.settings.workspace, desktopState.sessions[0].workspace);
page.on("pageerror", (error) => errors.push("desktop: " + error.message));
try {
  state.status.ready = state.status.vm_running = true;
  const project = page.getByRole("region", { name: "项目 .claude-cage-workspace", exact: true });
  for (const width of [1280, 820]) {
    await page.setViewportSize({ width, height: 850 });
    const row = project.getByRole("button", { name: "整理发布流程", exact: true });
    await row.waitFor();
    await row.hover();
    const actions = project.getByRole("group", { name: "整理发布流程 的操作", exact: true });
    const earlier = actions.getByRole("button", { name: "清除前的对话", exact: true });
    await earlier.waitFor();
    await shot(page, "desktop-" + width + "-1-row-actions");
    // The row's actions stay inside the sidebar row once they have moved in.
    const rowBox = await actions.locator("xpath=..").boundingBox();
    const actionsBox = await actions.boundingBox();
    assert.ok(actionsBox.x >= rowBox.x - 0.5 && actionsBox.x + actionsBox.width <= rowBox.x + rowBox.width + 0.5, "the actions fit the row at " + width + ": " + JSON.stringify({ rowBox, actionsBox }));
    await project.getByRole("button", { name: "另一段会话", exact: true }).hover();
    const other = project.getByRole("group", { name: "另一段会话 的操作", exact: true });
    await other.getByRole("button", { name: "重命名", exact: true }).waitFor();
    assert.equal(await other.getByRole("button", { name: "清除前的对话" }).count(), 0, "a session never cleared offers no earlier parts");
  }
  await page.setViewportSize({ width: 1280, height: 850 });
  await project.getByRole("button", { name: "整理发布流程", exact: true }).hover();
  await project.getByRole("group", { name: "整理发布流程 的操作", exact: true }).getByRole("button", { name: "清除前的对话", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "清除前的对话" });
  await dialog.getByRole("button", { name: /^清除于 今天 .*把发布脚本改成先跑测试，2 轮$/ }).waitFor();
  await dialog.getByRole("button", { name: /^清除于 昨天 .*先看看项目结构，1 轮$/ }).waitFor();
  await shot(page, "desktop-2-earlier-list");
  await dialog.getByRole("button", { name: /先看看项目结构/ }).click();
  const part = dialog.getByRole("region", { name: "清除前的对话：先看看项目结构" });
  await part.getByText("已调整：发布前先运行 npm test，失败时停止。").waitFor();
  await part.getByText("只读", { exact: false }).waitFor();
  assert.equal(await part.getByRole("textbox").count(), 0, "an earlier part takes no input");
  await shot(page, "desktop-3-earlier-part");
  await part.getByRole("button", { name: "恢复为单独会话" }).click();
  await dialog.getByRole("status").filter({ hasText: "已恢复为单独会话" }).waitFor();
  assert.equal(await dialog.getByRole("button", { name: /^清除于/ }).count(), 1, "the separated part left the list");
  assert.deepEqual(desktopState.requests.filter((r) => r.action).map((r) => [r.method, r.cli]), [["POST", "c1ea4ed0-0000-4000-8000-000000000001"]]);
  await shot(page, "desktop-4-separated");
  await dialog.getByRole("button", { name: "关闭面板" }).click();
  // The list's next answer brings the separated session as its own row.
  await project.getByRole("button", { name: "先看看项目结构", exact: true }).waitFor({ timeout: 20000 });
  await shot(page, "desktop-5-separated-row");

  // The phone: the drawer row's actions and the conversation.
  const phoneState = makeState("/work/alpha");
  for (const size of [{ width: 390, height: 844 }, { width: 360, height: 640 }]) {
    const tag = size.width + "x" + size.height;
    phoneState.parts = parts(); phoneState.sessions[0].earlier_parts = 2; phoneState.sessions.splice(2);
    const context = await browser.newContext({ viewport: size, isMobile: true, hasTouch: true, userAgent: "CageMobile/1", colorScheme: "light" });
    const phone = await context.newPage();
    phone.on("pageerror", (error) => errors.push("phone: " + error.message));
    await phone.route("**/api/**", async (route) => {
      const request = route.request(), url = new URL(request.url()), path = url.pathname;
      const answer = phoneState.earlier(path, request.method());
      if (answer) return route.fulfill(answer);
      if (path.endsWith("/projects")) return route.fulfill({ json: { host: "测试电脑", ready: true, shared_sessions: true, projects: [{ path: "/work/alpha", name: "alpha" }] } });
      if (path.endsWith("/sessions")) return route.fulfill({ json: { sessions: phoneState.sessions, notice: "" } });
      const target = phoneState.sessions.find((s) => path.includes(s.id));
      if (!target) return route.fulfill({ status: 404 });
      if (path.endsWith("/account")) return route.fulfill({ json: { session: target, supported: false, candidates: [] } });
      if (url.searchParams.get("after") === String(target.revision)) return route.fulfill({ status: 204 });
      return route.fulfill({ json: { ...target, can_send: true, turns: [{ id: "t9", prompt: "/clear 之后继续：检查发布清单", status: "completed", text: "清单已检查，缺少版本说明。" }] } });
    });
    await phone.goto(base + "/mobile.html");
    const drawer = phone.getByRole("dialog", { name: "工作区侧栏" });
    // The phone workspace opens on its home page (the recent list); the drawer holds the session rows.
    await phone.locator(".recent").getByRole("button", { name: /^整理发布流程/ }).waitFor();
    await phone.getByRole("button", { name: "展开侧栏" }).click();
    await drawer.getByRole("button", { name: "整理发布流程", exact: true }).waitFor();
    await phone.waitForFunction(() => document.querySelector(".workspace-drawer")?.dataset.visible === "true" && document.querySelector(".workspace-drawer").getBoundingClientRect().left >= 0);
    await drawer.getByRole("button", { name: "整理发布流程的更多操作" }).click();
    const rowActions = drawer.getByRole("group", { name: "整理发布流程的操作" });
    await rowActions.getByRole("button", { name: "清除前的对话" }).waitFor();
    for (const name of ["取消置顶", "重命名", "清除前的对话", "归档"]) {
      const box = await rowActions.getByRole("button", { name, exact: true }).boundingBox();
      assert.ok(box && box.width >= 32 && box.x >= 0 && box.x + box.width <= size.width + 0.5, tag + ": " + name + " is on screen and tappable");
    }
    await shot(phone, "phone-" + tag + "-1-row-actions");
    await drawer.getByRole("button", { name: "另一段会话的更多操作" }).click();
    await drawer.getByRole("group", { name: "另一段会话的操作" }).getByRole("button", { name: "归档" }).waitFor();
    assert.equal(await drawer.getByRole("group", { name: "另一段会话的操作" }).getByRole("button", { name: "清除前的对话" }).count(), 0, tag + ": never cleared, nothing offered");
    await rowActions.getByRole("button", { name: "清除前的对话" }).click();
    const sheet = phone.getByRole("dialog", { name: "清除前的对话" });
    await sheet.getByRole("button", { name: /把发布脚本改成先跑测试，2 轮$/ }).waitFor();
    await shot(phone, "phone-" + tag + "-2-earlier-list");
    await sheet.getByRole("button", { name: /把发布脚本改成先跑测试/ }).click();
    const part = sheet.getByRole("region", { name: "清除前的对话：把发布脚本改成先跑测试" });
    await part.getByText("日志现在写到 release.log。").waitFor();
    await shot(phone, "phone-" + tag + "-3-earlier-part");
    await part.getByRole("button", { name: "恢复为单独会话" }).click();
    await sheet.getByRole("status").filter({ hasText: "已恢复为单独会话" }).waitFor();
    await shot(phone, "phone-" + tag + "-4-separated");
    await phone.keyboard.press("Escape");
    await sheet.waitFor({ state: "hidden" });
    // Inside the conversation (opened afresh from the recent list): the entry above the transcript.
    await phone.goto(base + "/mobile.html");
    await phone.locator(".recent").getByRole("button", { name: /^整理发布流程/ }).click();
    await phone.getByRole("region", { name: "会话消息" }).waitFor();
    const entry = phone.getByRole("button", { name: /^清除前的对话 · \d$/ });
    await entry.waitFor();
    await shot(phone, "phone-" + tag + "-5-conversation");
    await entry.click();
    await phone.getByRole("dialog", { name: "清除前的对话" }).getByRole("button", { name: /^清除于/ }).first().waitFor();
    await shot(phone, "phone-" + tag + "-6-conversation-sheet");
    await context.close();
  }
  assert.deepEqual(errors, []);
  console.log("clear-session smoke: ok, screenshots in " + output);
} finally {
  await browser.close();
}
