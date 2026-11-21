// The closed gate explains itself: for each explanation state (and none, an
// older server) the status bar item, its popover and the home card's block;
// then a blocked navigation (the popover opens by itself) and the block in the
// network settings. Light and dark screenshots at 1440x900 and 1000x700.
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import { createWorkbench } from "./workbench-fixture.mjs";

const output = process.env.CAGE_TEST_CAPTURE || "/tmp/cage-network-explanation";
await mkdir(output, { recursive: true });
const url = process.env.CAGE_UI_URL || "http://127.0.0.1:5235";
const later = (seconds) => new Date(Date.now() + seconds * 1000).toISOString();
const closed = () => ({ mode: "closed", until: later(-60) });
const open = () => ({ mode: "open", until: later(3600) });
const advice = ["在隔离浏览器中关闭或刷新占用内存较多的标签页，或结束环境里不需要的程序", "在资源设置中查看环境内存；需要时调高内存上限（重启环境后生效）"];
const cases = [
  { name: "none", gate: closed, label: "外网已关闭" },
  { name: "unconfirmed", gate: open, label: "已联网 · 监测重试中", recheck: false, explanation: () => ({ state: "unconfirmed",
    reason: "联网监测暂时无法读取系统时区和语言", next: "外网暂时保持开放，但联网许可没有延长。", lease_until: later(45), memory_short: false, updated_at: later(0) }) },
  { name: "waiting_memory", gate: closed, label: "外网未放行 · 内存不足", recheck: true, explanation: () => ({ state: "waiting_memory",
    reason: "环境内存不足（可用约 46 MB，共 2.8 GB），联网许可到期前仍无法读取浏览器时区和语言，外网已暂停",
    next: "外网暂停中。自动完整重检正在等待内存压力缓解，最迟约 60 秒后也会进行，通过后自动恢复联网。", recheck_at: later(60), memory_short: true, advice, updated_at: later(0) }) },
  { name: "waiting_memory_browser", gate: closed, label: "外网未放行 · 内存不足", recheck: true, title: "外网已暂停：隔离浏览器内存不足", explanation: () => ({ state: "waiting_memory",
    reason: "隔离浏览器占用的内存超过了环境能分给它的上限（上限约 1.2 GB），联网许可到期前仍无法读取浏览器时区和语言，外网已暂停",
    next: "外网暂停中。自动完整重检正在等待浏览器内存回落，最迟约 60 秒后也会进行，通过后自动恢复联网。", recheck_at: later(60), memory_short: true, memory_cause: "browser",
    advice: ["在隔离浏览器中关闭或刷新占用内存较多的标签页", "在资源设置中可以把内存调到 6 GB，需要重启环境"], updated_at: later(0) }) },
  { name: "recheck_scheduled", gate: closed, label: "外网未放行 · 等待自动重检", recheck: true, explanation: () => ({ state: "recheck_scheduled",
    reason: "联网许可到期前未能确认浏览器配置，外网已暂停", next: "外网暂停中，稍后自动完整重检，通过后自动恢复联网。", recheck_at: later(125), updated_at: later(0) }) },
  { name: "rechecking", gate: () => ({ mode: "checking", until: later(120) }), label: "外网未放行 · 正在自动重检", recheck: false, explanation: () => ({ state: "rechecking",
    reason: "联网许可到期前未能确认浏览器配置，外网已暂停", next: "正在自动完整重检，通过后自动恢复联网。", updated_at: later(0) }) },
  { name: "check_required", gate: closed, label: "外网未放行 · 需要重新检测", recheck: true, explanation: () => ({ state: "check_required",
    reason: "出口地区与设置不符：检测到的出口位于日本，设置要求美国", next: "外网保持关闭，需要你确认代理节点后重新检测。", advice: ["在网络设置中换到美国的代理节点", "换好后点「立即重新检测」"], updated_at: later(0) }) },
  { name: "paused", gate: closed, label: "外网已手动暂停", recheck: true, explanation: () => ({ state: "paused",
    reason: "你已手动暂停外网，离线终端和文件仍可使用。", next: "重新检测通过后恢复联网。", updated_at: later(0) }) },
];
const checks = [];
for (const mode of ["light", "dark"]) {
  let state;
  const { browser, page, errors } = await createWorkbench({
    url, viewport: { width: 1440, height: 900 },
    beforeNavigate: async (page, value) => {
      state = value;
      state.appearance.mode = mode;
      Object.assign(state.status, { ready: true, vm_running: true });
      await page.route("**/api/audio", (route) => route.fulfill({ json: { format: "s16le", sample_rate: 48000, channels: 2 } }));
    },
  });
  const bar = page.locator(".status-bar");
  const shot = async (name) => {
    for (const size of [{ width: 1000, height: 700 }, { width: 1440, height: 900 }]) {
      await page.setViewportSize(size);
      await page.waitForTimeout(250);
      await page.screenshot({ path: `${output}/${mode}-${size.width}x${size.height}-${name}.png` });
    }
  };
  try {
    for (const item of cases) {
      state.status.safety = { gate: item.gate(), ...(item.explanation && { explanation: item.explanation() }) };
      await page.reload();
      await page.getByRole("heading", { name: "Super Cage 环境已就绪" }).waitFor();
      const button = bar.getByRole("button", { name: item.label });
      await button.waitFor();
      if (!item.explanation) {
        // An older server: today's text, and the click opens the checklist.
        assert.equal(await page.locator(".network-explanation").count(), 0);
        await shot("gate-" + item.name);
        await button.click();
        await page.getByRole("dialog", { name: "网络与隔离检查" }).waitFor();
        await page.keyboard.press("Escape");
        checks.push(mode + ": none keeps today's label and checklist");
        continue;
      }
      // The home card's block shows the reason without opening anything.
      const block = page.locator(".home-environment .network-explanation");
      await block.getByText(item.explanation().reason).waitFor();
      await page.locator(".home-environment").getByText(item.label, { exact: true }).waitFor();
      assert.equal(await block.getByRole("button", { name: "立即重新检测" }).count(), item.recheck ? 1 : 0, item.name + " recheck in the card");
      await button.click();
      const popover = page.getByRole("dialog", { name: /./ }).and(page.locator(".network-popover"));
      await popover.waitFor();
      await popover.getByText(item.explanation().reason).waitFor();
      if (item.explanation().advice) for (const line of item.explanation().advice) await popover.getByText(line).waitFor();
      const timer = popover.getByRole("timer");
      if (item.explanation().recheck_at || item.explanation().lease_until) {
        const first = await timer.textContent();
        await page.waitForTimeout(1300);
        assert.notEqual(await timer.textContent(), first, item.name + " countdown is live");
      } else assert.equal(await timer.count(), 0);
      assert.equal(await popover.getByRole("button", { name: "立即重新检测" }).count(), item.recheck ? 1 : 0, item.name + " recheck in the popover");
      if (item.name === "check_required") assert.ok(await popover.locator(".tone-alarm").count(), "check_required is the only alarm");
      if (item.title) await popover.getByText(item.title, { exact: true }).waitFor();
      if (item.name === "unconfirmed") assert.ok(await bar.locator(".network-tone-dot.tone-quiet").count(), "unconfirmed is a quiet amber hint");
      await shot("gate-" + item.name);
      if (item.name === "waiting_memory_browser") {
        // Advice about the memory setting links to 设置 → 环境与恢复 → 计算资源.
        assert.equal(await popover.getByRole("button", { name: "打开计算资源设置" }).count(), 1, "one link, after the sizing advice only");
        await popover.getByRole("button", { name: "打开计算资源设置" }).click();
        await popover.waitFor({ state: "detached" });
        await page.locator('.settings-content section[aria-label="计算资源"]').waitFor();
        await shot("resources-link");
        checks.push(mode + ": sizing advice opens 计算资源");
        continue;
      }
      if (item.name === "waiting_memory") {
        const check = page.waitForRequest((request) => request.method() === "POST" && new URL(request.url()).pathname === "/api/safety/check");
        await popover.getByRole("button", { name: "立即重新检测" }).click();
        await check;
        await popover.waitFor({ state: "detached" });
        checks.push(mode + ": 立即重新检测 posts /api/safety/check");
      } else {
        await page.keyboard.press("Escape");
        await popover.waitFor({ state: "detached" });
        assert.equal(await button.evaluate((node) => node === document.activeElement), true, "focus returns to the status item");
      }
      checks.push(mode + ": " + item.name);
    }
    // A blocked navigation opens the explanation instead of a bare toast.
    const memory = cases.find((item) => item.name === "waiting_memory");
    state.status.safety = { gate: memory.gate(), explanation: memory.explanation() };
    state.tabs = [{ id: "first", title: "Local page", kind: "web", url: "http://localhost:3000/" }];
    state.active = "first";
    await page.reload();
    await page.getByRole("button", { name: "打开浏览器", exact: true }).click();
    const address = page.getByRole("textbox", { name: "网页地址或搜索" });
    await address.fill("https://example.com/");
    await address.press("Enter");
    const explained = page.locator(".network-popover");
    await explained.getByText(memory.explanation().reason).waitFor();
    await page.getByRole("status").filter({ hasText: "外网暂未放行，暂时不能打开外部网站。" }).first().waitFor();
    assert.equal(await page.getByRole("dialog", { name: "网络与隔离检查" }).count(), 0, "the explanation, not the checklist");
    await shot("blocked-navigation");
    await page.keyboard.press("Escape");
    await explained.waitFor({ state: "detached" });
    checks.push(mode + ": a blocked navigation opens the explanation");
    // The network settings carry the same block under the proxy state.
    await page.getByRole("button", { name: "设置", exact: true }).click();
    await page.getByRole("button", { name: "网络与代理", exact: true }).click();
    const settingsBlock = page.locator(".settings-content .network-explanation");
    await settingsBlock.getByText(memory.explanation().reason).waitFor();
    await settingsBlock.getByRole("button", { name: "立即重新检测" }).waitFor();
    await shot("settings-network");
    checks.push(mode + ": the network settings explain the gate");
    assert.deepEqual(errors, []);
  } catch (error) {
    await page.screenshot({ path: `${output}/${mode}-FAILED.png` }).catch(() => {});
    throw error;
  } finally {
    await browser.close();
  }
}
console.log(JSON.stringify({ passed: true, checks, output }));
