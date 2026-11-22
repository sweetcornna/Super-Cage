// Settings → 其他客户端 against the isolated workbench fixture (design 8, step 1.6):
//   - the row: absent until something allowed is listed (nothing, 404, hidden
//     entries only, nothing allowed, a policy that allows none), one plain
//     GET /api/agents when the settings open and none before, never at load;
//   - the section's code loads only when its row is chosen; ?verify=1 goes out
//     once per opening and is never repeated by polling;
//   - the states: available, installing with progress and 取消, failed (each
//     failure line of design 4.4, with or without 重试), installed with the
//     one-time line, update available, drifted, own install, unsupported, not
//     allowed, withdrawn, a long name, the environment stopped, another job
//     running;
//   - the add and remove dialogs, and the requests they make;
//   - both themes at 1440x900 and 1000x700, no overflow, no page error.
// Screenshots go to CAGE_SHOTS (default output/other-clients/). Needs a Vite dev
// server (CAGE_UI_URL).
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createWorkbench } from "./workbench-fixture.mjs";
import { antigravity, codex, createAgents, entry, environment, failures, installAgents, job, progress, retryable } from "./other-clients-fixture.mjs";

const output = process.env.CAGE_SHOTS ? new URL("file://" + process.env.CAGE_SHOTS.replace(/\/?$/, "/")) : new URL("../../output/other-clients/", import.meta.url);
await fs.mkdir(output, { recursive: true });
const MB = 1048576;

const present = (version = "0.171.0", extra = {}) => ({ by: "cage", version, state: "ok", ...extra });
const mono = (id, name, text, over = {}) => codex({ id, name, mark: { monogram: text }, ...over });
const gallery = [
  [
    codex(),
    antigravity(),
    mono("goose", "Goose", "Go", { vendor: "Block", present: present("1.4.0"), pinned_version: "1.4.0" }),
    mono("aider", "Aider", "Ai", { vendor: "Aider", present: present("0.171.0"), update_available: true, pinned_version: "0.180.0" }),
    mono("qwen", "Qwen Code", "Qw", { vendor: "阿里云", present: present("1.0.2", { state: "drifted" }), pinned_version: "1.0.2" }),
  ],
  [
    mono("opencode", "OpenCode", "Op", { vendor: "SST", present: { by: "own", version: "0.171.0", state: "ok" } }),
    mono("warp", "Warp", "Wa", { vendor: "Warp", supported: false, unsupported_code: "guest", unsupported_reason: "macOS 客机暂不支持" }),
    mono("crush", "Crush", "Cr", { vendor: "Charm", allowed: false }),
    mono("amp", "Amp", "Am", { vendor: "Sourcegraph", status: "withdrawn", present: present("0.9.0"), pinned_version: "0.9.0" }),
    mono("long", "一个名字特别长的编程客户端 Extended Edition for Teams", "Lo", { vendor: "某某科技（深圳）有限公司", download_mb: 1240, disk_mb: 3100 }),
  ],
];
// The title's pieces (the separators are drawn by CSS).
const titleOf = (row) => row.locator(".other-client-title").evaluate((node) => [...node.children].map((child) => child.textContent));
const rowOf = (page, id) => page.locator('.other-client[data-id="' + id + '"]');

async function pollNow(page) {
  await page.evaluate(async () => {
    window.cageWindowVisible = false;
    window.dispatchEvent(new Event("cage-window-visibility"));
    await new Promise((resolve) => setTimeout(resolve, 40));
    window.cageWindowVisible = true;
    window.dispatchEvent(new Event("cage-window-visibility"));
  });
}
async function openSettings(page) {
  await page.getByRole("button", { name: "设置", exact: true }).click();
  await page.getByRole("navigation", { name: "设置分组" }).getByRole("button", { name: "通用", exact: true }).waitFor();
}
const settingsNav = (page) => page.getByRole("navigation", { name: "设置分组" });
async function closeSettings(page) {
  await page.locator("#settings-back").click();
  await page.locator(".settings-page").waitFor({ state: "detached" });
}
async function openSection(page) {
  await settingsNav(page).getByRole("button", { name: "其他客户端", exact: true }).click();
  await page.locator(".other-clients-list").waitFor();
}
// Leave the section and come back: a new opening, a new ?verify=1 read.
async function reopenSection(page) {
  await settingsNav(page).getByRole("button", { name: "通用", exact: true }).click();
  await page.locator(".other-clients-page").waitFor({ state: "detached" });
  await openSection(page);
}

async function run({ theme, viewport }) {
  const tag = theme + "-" + viewport.width + "x" + viewport.height;
  // A failed job also toasts (main.jsx): outside the failure shots, wait for it to go.
  const shot = async (page, name) => {
    if (!name.startsWith("failed-")) await page.locator(".toast").waitFor({ state: "detached", timeout: 8000 }).catch(() => {});
    await page.screenshot({ path: fileURLToPath(new URL(tag + "-" + name + ".png", output)) });
  };
  const model = createAgents([codex(), antigravity()]);
  const chunks = [];
  const fixture = await createWorkbench({
    viewport,
    beforeNavigate: async (page) => {
      await page.emulateMedia({ colorScheme: theme });
      await page.addInitScript(() => localStorage.setItem("cage.audio", JSON.stringify({ muted: true })));
      await installAgents(page, model);
      page.on("request", (request) => { if (request.url().includes("other-clients-section")) chunks.push(request.url()); });
    },
  });
  const { page, state, errors } = fixture;
  const agentCalls = () => model.calls.filter((call) => call.method === "GET").map((call) => call.query);
  try {
    // Not at load: no agents request and no section code before the settings open.
    state.status.vm_running = true;
    state.status.ready = true;
    await page.reload();
    await page.locator('button[data-terminal-location="/Users/shihao/.claude-cage-workspace"]:not([disabled])').waitFor();
    assert.equal(model.calls.length, 0, "no /api/agents request at load");
    assert.equal(chunks.length, 0);

    // Opening the settings asks once, plainly; the row appears; the section's code does not load yet.
    await openSettings(page);
    await settingsNav(page).getByRole("button", { name: "其他客户端", exact: true }).waitFor();
    assert.deepEqual(agentCalls(), [""], "one plain read when the settings open");
    assert.equal(chunks.length, 0, "the section's code loads only when its row is chosen");
    const labels = await settingsNav(page).getByRole("button").allTextContents();
    assert.deepEqual(labels.slice(labels.indexOf("环境与恢复"), labels.indexOf("环境与恢复") + 3), ["环境与恢复", "其他客户端", "关于"], "the row sits after 环境与恢复");

    // The section: one read with verify, then still while it idles (no polling).
    model.entries = gallery[0];
    await openSection(page);
    assert.ok(chunks.length > 0);
    assert.deepEqual(agentCalls(), ["", "?verify=1"]);
    assert.equal(await page.getByRole("heading", { name: "其他客户端", level: 3 }).count(), 1);
    await page.getByText("Claude Code 随环境提供。需要时，可以在这个环境中添加其他编程客户端。", { exact: true }).waitFor();
    await page.getByText("添加和更新通过所选代理下载，不会放行业务网络。安装期间不能新建终端，通常一分钟左右。", { exact: true }).waitFor();
    // Not a fieldset child: the buttons stay alive while a job runs.
    assert.equal(await page.locator("fieldset.settings-fields .other-clients-page").count(), 0);
    const states = await page.locator(".other-client").evaluateAll((items) => items.map((item) => item.dataset.id + ":" + item.dataset.state));
    assert.deepEqual(states, ["codex:available", "antigravity-cli:available", "goose:installed", "aider:update", "qwen:drifted"]);
    assert.equal(await rowOf(page, "codex").locator(".other-client-meta").innerText(), "下载约 124 MB，安装后约 310 MB");
    assert.equal(await rowOf(page, "goose").locator(".other-client-meta").count(), 0);
    assert.equal(await rowOf(page, "aider").locator(".other-client-meta").innerText(), "有新版本 0.180.0");
    assert.equal(await rowOf(page, "qwen").locator(".other-client-meta").innerText(), "程序文件与安装时不同");
    await rowOf(page, "qwen").getByRole("button", { name: "修复", exact: true }).waitFor();
    await rowOf(page, "aider").getByRole("button", { name: "更新", exact: true }).waitFor();
    assert.equal(await rowOf(page, "goose").getByRole("button", { name: "添加" }).count(), 0);
    await page.waitForTimeout(500);
    await shot(page, "states-a");
    await page.waitForTimeout(6000);
    assert.deepEqual(agentCalls(), ["", "?verify=1"], "never repeated by polling");

    model.entries = gallery[1];
    await reopenSection(page);
    assert.deepEqual(agentCalls().slice(2), ["?verify=1"]);
    const second = await page.locator(".other-client").evaluateAll((items) => items.map((item) => item.dataset.id + ":" + item.dataset.state));
    assert.deepEqual(second, ["opencode:own", "warp:unsupported", "crush:not-allowed", "amp:withdrawn", "long:available"]);
    assert.deepEqual(await titleOf(rowOf(page, "opencode")), ["OpenCode", "已安装（自行安装）", "0.171.0"]);
    assert.equal(await rowOf(page, "opencode").getByRole("button").count(), 0, "an own install has no button");
    assert.equal(await rowOf(page, "warp").locator(".other-client-meta").innerText(), "macOS 客机暂不支持");
    assert.equal(await rowOf(page, "warp").getByRole("button").count(), 0);
    assert.equal(await rowOf(page, "crush").locator(".other-client-meta").innerText(), "这个服务器的管理员未开放");
    assert.equal(await rowOf(page, "crush").getByRole("button").count(), 0);
    assert.equal(await rowOf(page, "amp").locator(".other-client-meta").innerText(), "不再受支持");
    await rowOf(page, "amp").getByRole("button", { name: "更多操作：Amp" }).click();
    await page.getByRole("menuitem", { name: "移除", exact: true }).waitFor();
    await page.keyboard.press("Escape");
    await page.getByRole("menuitem", { name: "移除", exact: true }).waitFor({ state: "detached" });
    assert.ok((await titleOf(rowOf(page, "long")))[0].startsWith("一个名字特别长"));
    await page.waitForTimeout(300);
    await shot(page, "states-b");
    const overflow = () => page.evaluate(() => { const box = document.querySelector(".settings-content"); return box.scrollWidth > box.clientWidth; });
    assert.equal(await overflow(), false, "no horizontal overflow");

    // One entry: add, installing, cancel, failed, again, added.
    model.entries = [codex()];
    await reopenSection(page);
    const codexRow = rowOf(page, "codex");
    await codexRow.getByRole("button", { name: "添加", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "添加 Codex？" });
    await dialog.waitFor();
    const facts = await dialog.locator(".other-clients-fact").evaluateAll((items) => items.map((item) => item.innerText.replace(/\s+/g, " ").trim()));
    assert.deepEqual(facts.map((text) => text.split(" ")[0]), ["来源", "占用", "联网", "遥测", "地区", "安装期间"]);
    assert.match(facts[0], /OpenAI 在 GitHub 发布的 0\.171\.0，已核对校验值/);
    assert.match(facts[1], /下载约 124 MB，安装后约 310 MB（客机剩余 18\.2 GB）/);
    assert.match(facts[2], /下载 github\.com · 登录 auth\.openai\.com、chatgpt\.com · 使用 chatgpt\.com、api\.openai\.com 等/);
    assert.match(facts[5], /不能新建终端（包括 Claude 终端），约一分钟。/);
    await dialog.getByText("展开查看全部").click();
    await page.waitForTimeout(250);
    await shot(page, "add-dialog");
    await dialog.getByRole("button", { name: "取消", exact: true }).click();
    await dialog.waitFor({ state: "detached" });
    assert.equal(model.calls.filter((call) => call.method === "POST").length, 0, "取消 asks nothing");
    await codexRow.getByRole("button", { name: "添加", exact: true }).click();
    await dialog.getByRole("button", { name: "添加", exact: true }).click();
    await dialog.waitFor({ state: "detached" });
    // The accepted job shows at once, before the next poll.
    await codexRow.getByRole("button", { name: "取消", exact: true }).waitFor();
    assert.deepEqual(model.calls.filter((call) => call.method === "POST").map((call) => [call.path, call.body]), [["/agents/codex/install", {}]]);
    const accepted = model.lastJob;
    state.status.job = { ...accepted, running: true, setup_progress: progress("download", "下载", 34 * MB, 124 * MB) };
    await pollNow(page);
    await codexRow.getByText("正在下载 34 / 124 MB", { exact: true }).waitFor();
    assert.equal(await codexRow.getByRole("progressbar").getAttribute("aria-valuenow"), "27");
    // Single slot: the other rows wait.
    assert.equal(await rowOf(page, "codex").getByRole("button", { name: "取消" }).isEnabled(), true);
    await page.getByText("另一个操作仍在进行，完成后可以继续。").count();
    await page.waitForTimeout(300);
    await shot(page, "installing");
    state.status.job = { ...accepted, running: true, setup_progress: progress("verify", "验证", 0, 0) };
    await pollNow(page);
    await codexRow.getByText("正在验证", { exact: true }).waitFor();
    await codexRow.getByRole("button", { name: "取消", exact: true }).click();
    assert.deepEqual(model.calls.filter((call) => call.method === "POST").at(-1).body, { job: accepted.id });
    // Canceled: not an error colour, 重试 offered.
    state.status.job = { ...accepted, running: false, canceled: true, error: failures.canceled };
    await pollNow(page);
    await codexRow.getByText(failures.canceled, { exact: true }).waitFor();
    assert.ok(await codexRow.locator(".other-client-meta.muted").count());
    await codexRow.getByRole("button", { name: "重试", exact: true }).waitFor();

    // Failed, each line of design 4.4.
    let sequence = 0;
    for (const [code, line] of Object.entries(failures)) {
      if (code === "canceled") continue;
      state.status.job = job("agent-install:codex", "fail-" + code + "-" + ++sequence, { running: false, error: line });
      await pollNow(page);
      await codexRow.getByText(line, { exact: true }).waitFor();
      assert.ok(await codexRow.locator(".other-client-meta.error").count(), code + " reads as an error");
      assert.equal(await codexRow.getByRole("button", { name: "重试", exact: true }).count(), retryable[code] ? 1 : 0, code + " retry");
      if ((theme === "light" && viewport.width === 1440) || ["digest", "own-install"].includes(code)) { await page.waitForTimeout(250); await shot(page, "failed-" + code); }
    }
    // 重试 repeats the operation that failed.
    state.status.job = job("agent-install:codex", "fail-retry", { running: false, error: failures.download });
    await pollNow(page);
    await codexRow.getByRole("button", { name: "重试", exact: true }).click();
    assert.deepEqual(model.calls.filter((call) => call.method === "POST").at(-1).path, "/agents/codex/install");
    // It finishes: the list is read again (without verify) and the row is installed, once with its line.
    const before = agentCalls().length;
    const retry = model.lastJob;
    state.status.job = { ...retry, running: true, setup_progress: progress("unpack", "解包", 150 * MB, 310 * MB) };
    await pollNow(page);
    await codexRow.getByText("正在解包", { exact: true }).waitFor();
    model.entries = [codex({ present: present() })];
    state.status.job = { ...retry, running: false, error: "" };
    await pollNow(page);
    await codexRow.getByText("已添加。可以在 新建 中打开。", { exact: true }).waitFor();
    assert.deepEqual(agentCalls().slice(before), [""], "one plain read after the job");
    assert.deepEqual(await titleOf(codexRow), ["Codex", "0.171.0"]);
    assert.equal(await codexRow.getByRole("button", { name: "添加" }).count(), 0);
    await page.waitForTimeout(300);
    await shot(page, "installed");

    // Update and remove.
    model.entries = [codex({ present: present(), update_available: true, pinned_version: "0.180.0" })];
    await reopenSection(page);
    await codexRow.getByRole("button", { name: "更新", exact: true }).click();
    assert.deepEqual(model.calls.filter((call) => call.method === "POST").at(-1).path, "/agents/codex/update");
    state.status.job = { ...model.lastJob, running: false, error: "" };
    await pollNow(page);
    model.entries = [codex({ present: present("0.180.0"), pinned_version: "0.180.0" })];
    await reopenSection(page);
    await codexRow.getByRole("button", { name: "更多操作：Codex" }).click();
    await page.getByRole("menuitem", { name: "移除", exact: true }).click();
    const removal = page.getByRole("dialog", { name: "移除 Codex？" });
    await removal.getByText("程序文件会被删除。登录信息、账号和会话记录默认保留，下次添加后可以继续使用。").waitFor();
    assert.equal(await removal.getByRole("checkbox", { name: "同时删除登录信息和设置" }).isChecked(), false);
    await page.waitForTimeout(250);
    await shot(page, "remove-dialog");
    await removal.getByRole("checkbox", { name: "同时删除登录信息和设置" }).check();
    await removal.getByRole("button", { name: "移除", exact: true }).click();
    assert.deepEqual(model.calls.filter((call) => call.method === "POST").at(-1), { method: "POST", path: "/agents/codex/remove", query: "", body: { remove_data: true } });
    const removing = model.lastJob;
    state.status.job = { ...removing, running: true, setup_progress: progress("check", "检查环境", 0, 0) };
    await pollNow(page);
    await codexRow.getByText("正在移除", { exact: true }).waitFor();
    assert.equal(await codexRow.getByRole("button", { name: "取消" }).count(), 0, "no cancel for a removal");
    state.status.job = { ...removing, running: false, error: "" };
    model.entries = [codex()];
    await pollNow(page);
    await codexRow.getByRole("button", { name: "添加", exact: true }).waitFor();

    // A refused request shows in the row; another operation running disables the buttons.
    model.replies.push({ status: 409, body: "另一个操作仍在进行，请稍候" });
    await reopenSection(page);
    await codexRow.getByRole("button", { name: "添加", exact: true }).click();
    await page.getByRole("dialog").getByRole("button", { name: "添加", exact: true }).click();
    await codexRow.getByText("另一个操作仍在进行，请稍候", { exact: true }).waitFor();
    state.status.job = { id: "other-op", kind: "settings", running: true, message: "正在保存" };
    await pollNow(page);
    await page.getByText("另一个操作仍在进行，完成后可以继续。", { exact: true }).waitFor();
    state.status.job = { id: "other-op", kind: "settings", running: false };
    await pollNow(page);

    // The environment stopped: the catalog is listed, the buttons are off, one line says why.
    model.environment = { ...environment, running: false };
    model.entries = gallery[0];
    await reopenSection(page);
    await page.getByText("启动环境后可以添加。", { exact: true }).waitFor();
    assert.equal(await rowOf(page, "codex").getByRole("button", { name: "添加", exact: true }).isDisabled(), true);
    assert.equal(await rowOf(page, "aider").getByRole("button", { name: "更新", exact: true }).isDisabled(), true);
    await page.waitForTimeout(300);
    await shot(page, "stopped");
    model.environment = environment;

    // The row is absent unless something allowed is listed.
    await closeSettings(page);
    for (const [name, answer] of [["nothing listed", { agents: [] }], ["no routes", { status: 404, body: "not found" }], ["hidden entries only", { agents: [codex({ status: "hidden" })] }], ["nothing allowed", { agents: [codex({ allowed: false })] }]]) {
      if (answer.status) { model.failList = answer.body; } else { model.failList = ""; model.entries = answer.agents; }
      await openSettings(page);
      await page.waitForTimeout(500);
      assert.equal(await settingsNav(page).getByRole("button", { name: "其他客户端", exact: true }).count(), 0, name + ": no row");
      await closeSettings(page);
    }
    assert.deepEqual(errors, []);
    assert.equal(await overflow().catch(() => false), false);
    console.log("PASS: " + tag + " the row, lazy section, states, failure lines, dialogs and requests");
  } finally { await fixture.browser.close(); }
}

// A server's policy that allows nothing hides the row without a request.
async function policyRun() {
  const model = createAgents([codex()]);
  const fixture = await createWorkbench({
    viewport: { width: 1440, height: 900 },
    clientContext: { mode: "local", connection_id: "local", client_platform: "darwin", server_platform: "darwin", connection: { state: "disconnected" }, agents_policy: { allow: [] } },
    beforeNavigate: (page) => installAgents(page, model),
  });
  try {
    await openSettings(fixture.page);
    await fixture.page.waitForTimeout(500);
    assert.equal(await settingsNav(fixture.page).getByRole("button", { name: "其他客户端", exact: true }).count(), 0);
    assert.equal(model.calls.length, 0, "a policy that allows none asks nothing");
    assert.deepEqual(fixture.errors, []);
    console.log("PASS: a policy that allows none hides the row and asks nothing");
  } finally { await fixture.browser.close(); }
}

for (const theme of ["light", "dark"]) {
  for (const viewport of [{ width: 1440, height: 900 }, { width: 1000, height: 700 }]) await run({ theme, viewport });
}
await policyRun();
