// Codex sessions in the workbench, against the isolated fixture
// (codex-session-fixture.mjs, one session per owner state):
// list (four owner states, held list) -> a refused open shown under its row
// -> open an idle one (POST /open, bound tab, no Claude veil or account reads)
// -> the phone-owned one read-only with its reason (a stale stop refused in
// place, then stopped) -> stop a desktop turn -> archive -> restore and purge
// from the archived list (a refused purge shown in place) -> the menu makes a
// Codex session -> an older server keeps the plain Codex terminal.
// Light and dark screenshots of each state at 1440x900 and 1000x700.
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import { createWorkbench } from "./workbench-fixture.mjs";
import { READONLY_REASONS } from "../src/session-provider.mjs";

const output = process.env.CAGE_TEST_CAPTURE || "/tmp/cage-codex-sessions";
await mkdir(output, { recursive: true });
const url = process.env.CAGE_UI_URL || "http://127.0.0.1:5235";
const title = { idle: "整理登录流程", desktop: "电脑上打开的会话", phone: "手机上运行的一轮", external: "其他进程中打开" };
const rowName = (text) => new RegExp("^" + text);
const checks = [];

for (const mode of ["light", "dark"]) {
  let state;
  const lists = [];
  const { browser, page, errors } = await createWorkbench({
    url, codexSessions: true, viewport: { width: 1440, height: 900 },
    beforeNavigate: async (page, value) => {
      state = value;
      state.appearance.mode = mode;
      page.on("request", (request) => {
        const address = new URL(request.url());
        if (address.pathname === "/api/cli/sessions") lists.push(address.search);
      });
      Object.assign(state.status, { ready: true, vm_running: true,
        safety: { gate: { mode: "open", until: new Date(Date.now() + 6 * 3600000).toISOString() } } });
      const workspace = state.status.projects[0];
      state.claudeSessions = [{ id: "claude-1", workspace, title: "重构桌面工作台的视觉语言与组件层", updated_at: new Date(Date.now() - 180000).toISOString() }];
      await page.route("**/api/audio", (route) => route.fulfill({ json: { format: "s16le", sample_rate: 48000, channels: 2 } }));
    },
  });
  const shot = async (name, size) => {
    if (size) await page.setViewportSize(size);
    await page.waitForTimeout(350);
    const { width, height } = page.viewportSize();
    await page.screenshot({ path: `${output}/${mode}-${width}x${height}-${name}.png` });
  };
  const both = async (name) => { await shot(name, { width: 1000, height: 700 }); await shot(name, { width: 1440, height: 900 }); };
  const sidebar = page.locator(".workspace-sidebar");
  const requests = (match) => state.requests.filter(match);
  try {
    // 1. The list: four owner states beside a Claude session, held by the server.
    await page.getByRole("heading", { name: "Super Cage 环境已就绪" }).waitFor();
    for (const name of [title.idle, title.phone, title.external]) await sidebar.getByRole("button", { name: rowName(name) }).waitFor();
    await sidebar.getByRole("tab", { name: rowName(title.desktop) }).waitFor();
    assert.equal(await sidebar.getByRole("button", { name: rowName(title.phone) }).isDisabled(), false, "a phone-owned Codex row stays clickable");
    assert.equal(await sidebar.getByRole("button", { name: rowName(title.external) }).isDisabled(), false, "an external Codex row stays clickable");
    assert.equal(await sidebar.locator('[data-tab-id$="c0d3c0d3-0000-4000-8000-000000000003"] .session-owner-chip').textContent(), "手机");
    assert.equal(await sidebar.locator('[data-tab-id$="c0d3c0d3-0000-4000-8000-000000000004"] .session-owner-chip').textContent(), "外部");
    assert.ok(await sidebar.locator('[data-tab-id$="c0d3c0d3-0000-4000-8000-000000000003"] .provider-mark-codex').count(), "Codex rows carry the Codex mark");
    await page.waitForTimeout(1500);
    assert.ok(lists[0].includes("providers=claude%2Ccodex"), "the desktop asks for both providers");
    assert.ok(lists.some((search) => /after=[0-9a-f]{32}/.test(search) && search.includes("wait=25")), "the list is then held by the server");
    const home = page.locator(".home-recent");
    await home.getByRole("button", { name: "继续会话「" + title.phone + "」" }).waitFor();
    await both("list");
    checks.push(mode + ": list with four owner states");

    // 2. A refusal shows under the row in the server's words; the row stays usable.
    state.codexRefusals = { "c0d3c0d3-0000-4000-8000-000000000001/open": [409, "这段会话所用的 Codex 账号已移除，无法继续"] };
    await sidebar.getByRole("button", { name: rowName(title.idle) }).click();
    await sidebar.locator("#session-open-error-c0d3c0d3-0000-4000-8000-000000000001")
      .filter({ hasText: "无法打开 Codex 会话：这段会话所用的 Codex 账号已移除，无法继续" }).waitFor();
    await shot("open-refused");
    checks.push(mode + ": a refused open is shown under its row");
    // Open the idle one: POST /open, the bound tab, no Claude veil, no account reads.
    const opened = page.waitForRequest((request) => request.method() === "POST" && request.url().includes("/cli/sessions/c0d3c0d3-0000-4000-8000-000000000001/open"));
    await sidebar.getByRole("button", { name: rowName(title.idle) }).click();
    await opened;
    const tab = sidebar.getByRole("tab", { name: rowName(title.idle) });
    await tab.waitFor();
    assert.equal(await tab.getAttribute("aria-selected"), "true", "the bound tab is the active tab");
    await page.locator(".terminal-lane:not([hidden]) .xterm").first().waitFor();
    assert.equal(await page.locator(".terminal-startup").count(), 0, "no Claude startup veil on a Codex terminal");
    await page.waitForTimeout(1700);
    assert.equal(await page.locator(".codex-opening").count(), 0, "no opening line once Codex has drawn");
    await page.waitForTimeout(600);
    assert.equal(requests((r) => /\/cli\/sessions\/c0d3c0d3-0000-4000-8000-000000000001\/account$/.test(r.path)).length, 0, "no Claude account reads for a Codex tab");
    const detail = page.locator(".terminal-lane:not([hidden]) .terminal-detail");
    await assert.doesNotReject(detail.filter({ hasText: /^Codex · 默认登录 · / }).waitFor());
    await both("bound-terminal");
    checks.push(mode + ": idle session opens as a bound Codex terminal");

    // 3. The phone-owned one opens read-only with the reason and the transcript.
    await sidebar.getByRole("button", { name: rowName(title.phone) }).click();
    const pane = page.locator(".codex-session-page");
    await pane.getByRole("heading", { name: title.phone }).waitFor();
    await pane.getByText(READONLY_REASONS.phone).waitFor();
    await pane.getByText("跑一遍测试").waitFor();
    assert.equal(await pane.getByRole("button", { name: "在终端中继续" }).isDisabled(), true, "continue waits for the turn");
    await pane.getByRole("button", { name: "停止本轮" }).waitFor();
    assert.equal(requests((r) => r.method === "POST" && r.path.endsWith("000000000003/open")).length, 0, "no /open for a phone-owned session");
    await both("readonly-phone");
    // The external one: its reason and a re-check, no stop.
    await sidebar.getByRole("button", { name: rowName(title.external) }).click();
    await pane.getByRole("heading", { name: title.external }).waitFor();
    await pane.getByText(READONLY_REASONS.external).waitFor();
    await pane.getByRole("button", { name: "重新检查" }).waitFor();
    assert.equal(await pane.getByRole("button", { name: "停止本轮" }).count(), 0);
    await both("readonly-external");
    // Stop the phone's turn from the pane; the pane follows the view.
    await sidebar.getByRole("button", { name: rowName(title.phone) }).click();
    await pane.getByRole("heading", { name: title.phone }).waitFor();
    // A stale turn: the server's refusal in the pane, the turn untouched.
    state.codexRefusals = { "c0d3c0d3-0000-4000-8000-000000000003/stop": [409, "本轮运行状态已变化，请重新读取"] };
    await pane.getByRole("button", { name: "停止本轮" }).click();
    await pane.getByRole("alert").filter({ hasText: "本轮运行状态已变化，请重新读取" }).waitFor();
    await shot("stop-refused");
    const phoneStop = page.waitForRequest((request) => request.method() === "POST" && request.url().endsWith("000000000003/stop"));
    await pane.getByRole("button", { name: "停止本轮" }).click();
    assert.match((await phoneStop).postData(), /c0d3c0d3-1000-4000-8000-000000000003/);
    await page.waitForFunction(() => !document.querySelector(".codex-session-page button.primary")?.disabled);
    await shot("readonly-after-stop");
    checks.push(mode + ": phone-owned and external sessions are read-only with their reasons");

    // 4. Stop a desktop turn from the bound terminal's header (Esc through the server).
    await sidebar.getByRole("tab", { name: rowName(title.desktop) }).click();
    const stopButton = page.locator(".terminal-lane:not([hidden])").getByRole("button", { name: "停止本轮" });
    await stopButton.waitFor();
    const deskStop = page.waitForRequest((request) => request.method() === "POST" && request.url().endsWith("000000000002/stop"));
    await stopButton.click();
    assert.match((await deskStop).postData(), /c0d3c0d3-1000-4000-8000-000000000002/, "the stop names the running turn");
    await shot("desktop-turn");
    checks.push(mode + ": desktop turn stopped from the header");

    // 5. Archive the idle session by closing its live tab (the design's confirm).
    await sidebar.getByRole("tab", { name: rowName(title.idle) }).click();
    await page.locator(".terminal-lane:not([hidden])").getByRole("button", { name: "归档当前会话" }).click();
    const dialog = page.getByRole("dialog");
    await dialog.getByText("关闭并归档这段会话？").waitFor();
    await dialog.getByText(/关闭后这段会话会归档/).waitFor();
    await shot("archive-confirm");
    const archived = page.waitForRequest((request) => request.method() === "POST" && request.url().endsWith("000000000001/archive"));
    await dialog.getByRole("button", { name: "关闭并归档" }).click();
    await archived;
    await sidebar.getByRole("button", { name: rowName(title.idle) }).waitFor({ state: "detached" });
    await sidebar.getByRole("tab", { name: rowName(title.idle) }).waitFor({ state: "detached" });
    checks.push(mode + ": closing a live bound tab archives after the confirm");

    // 6. Restore it, and purge the external one, from the archived list.
    await sidebar.locator('[data-tab-id$="000000000004"]').hover();
    await sidebar.locator('[data-tab-id$="000000000004"]').getByRole("button", { name: "归档" }).click();
    await page.getByRole("dialog").getByText(/不会停止其他进程中的 Codex/).waitFor();
    await page.getByRole("dialog").getByRole("button", { name: "归档会话" }).click();
    await sidebar.getByRole("button", { name: rowName(title.external) }).waitFor({ state: "detached" });
    await page.getByRole("button", { name: "设置", exact: true }).click();
    await page.getByRole("navigation", { name: "设置分组" }).getByRole("button", { name: "已归档会话", exact: true }).click();
    const archive = page.getByRole("region", { name: "已归档会话", exact: true });
    await archive.getByRole("heading", { name: title.idle }).waitFor();
    assert.ok(await archive.locator(".provider-mark-codex").count() >= 2, "archived Codex rows carry the Codex mark");
    await shot("archived");
    const externalRow = archive.locator("article", { has: page.getByRole("heading", { name: title.external }) });
    await externalRow.getByRole("button", { name: "删除会话" }).click();
    await archive.getByText(`永久删除“${title.external}”及其 Codex 原始对话？此操作无法撤销。`).waitFor();
    await shot("purge-confirm");
    // The guest refuses while another process holds it: the reason in place, the row kept.
    state.codexRefusals = { "c0d3c0d3-0000-4000-8000-000000000004/purge": [400, "这段会话正在电脑的 Codex 终端中打开，请先关闭该终端再删除"] };
    await archive.getByRole("group", { name: "确认永久删除会话" }).getByRole("button", { name: "永久删除" }).click();
    await archive.getByRole("alert").filter({ hasText: "这段会话正在电脑的 Codex 终端中打开，请先关闭该终端再删除" }).waitFor();
    await archive.getByRole("heading", { name: title.external }).waitFor();
    await shot("purge-refused");
    const purged = page.waitForRequest((request) => request.method() === "POST" && request.url().endsWith("000000000004/purge"));
    await archive.getByRole("group", { name: "确认永久删除会话" }).getByRole("button", { name: "永久删除" }).click();
    assert.match((await purged).postData(), /"confirm":true/);
    const idleRow = archive.locator("article", { has: page.getByRole("heading", { name: title.idle }) });
    const restored = page.waitForRequest((request) => request.method() === "POST" && request.url().endsWith("000000000001/restore"));
    await idleRow.getByRole("button", { name: "取消归档" }).click();
    await restored;
    // Escape closes the settings; wait for them to go rather than probing a button that may be leaving.
    await page.keyboard.press("Escape");
    await archive.waitFor({ state: "detached" });
    await sidebar.getByRole("button", { name: rowName(title.idle) }).waitFor();
    checks.push(mode + ": restore and purge with the neutral copy");

    // 7. The menu makes a Codex session where the server says it can: a bound tab at once.
    await sidebar.getByRole("button", { name: "工作台", exact: true }).click();
    await page.locator('[data-terminal-location="start"]').click();
    const sessionMenu = page.getByRole("menu", { name: "新建终端" });
    await sessionMenu.getByRole("menuitem", { name: "Codex 会话" }).waitFor();
    await shot("menu-session");
    const made = page.waitForRequest((request) => request.method() === "POST" && new URL(request.url()).pathname === "/api/terminals");
    // Codex draws its first screen after 4 s: from 1.5 s a quiet line says it is opening.
    state.terminalDrawMs = 4000;
    await sessionMenu.getByRole("menuitem", { name: "Codex 会话" }).click();
    const making = await made;
    assert.equal(new URL(making.url()).searchParams.get("providers"), "claude,codex", "the menu opts in to sessions");
    assert.equal(JSON.parse(making.postData()).kind, "codex");
    const madeTab = sidebar.getByRole("tab", { name: rowName("新 Codex 会话") });
    await madeTab.waitFor();
    assert.equal(await madeTab.getAttribute("aria-selected"), "true");
    await page.locator(".terminal-lane:not([hidden]) .terminal-detail").filter({ hasText: /^Codex · / }).waitFor();
    const opening = page.locator(".terminal-lane:not([hidden]) .codex-opening");
    await opening.filter({ hasText: "正在打开 Codex…" }).waitFor();
    await shot("codex-opening");
    await opening.waitFor({ state: "detached" });
    state.terminalDrawMs = 0;
    checks.push(mode + ": the menu makes a Codex session");

    // 8. An older server: no capabilities, no Codex rows, today's plain Codex terminal.
    state.codexOlderServer = true;
    await page.reload();
    await page.getByRole("heading", { name: "Super Cage 环境已就绪" }).waitFor();
    await page.waitForTimeout(800);
    assert.equal(await sidebar.getByRole("button", { name: rowName(title.phone) }).count(), 0, "no Codex rows from an older server");
    const before = lists.length;
    await page.locator('[data-terminal-location="start"]').click();
    const menu = page.getByRole("menu", { name: "新建终端" });
    await menu.getByRole("menuitem", { name: "Codex 终端" }).waitFor();
    await shot("older-server-menu");
    const created = page.waitForRequest((request) => request.method() === "POST" && new URL(request.url()).pathname === "/api/terminals");
    await menu.getByRole("menuitem", { name: "Codex 终端" }).click();
    const request = await created;
    assert.equal(new URL(request.url()).search, "", "an older server gets today's request");
    assert.equal(JSON.parse(request.postData()).kind, "codex");
    assert.ok(lists.slice(before).every((search) => !search.includes("wait=")), "no held list for an older server");
    checks.push(mode + ": older server keeps the plain Codex terminal");
    assert.deepEqual(errors, []);
  } catch (error) {
    console.error(mode + ": failed after", checks.at(-1) || "the start");
    await page.screenshot({ path: `${output}/${mode}-FAILED.png` }).catch(() => {});
    throw error;
  } finally {
    await browser.close();
  }
}
console.log(JSON.stringify({ passed: true, checks, output }));
