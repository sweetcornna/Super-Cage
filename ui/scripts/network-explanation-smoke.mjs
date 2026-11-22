// The closed gate explains itself: for each explanation state (and none, an
// older server) the status bar item, its popover and the home card's block;
// then a blocked navigation (the popover opens by itself) and the block in the
// network settings. Light and dark screenshots at 1440x900 and 1000x700.
// The server's reason and next sentences are stable and carry no countdown; the
// one live countdown (deadline_ms) sits in the popover's next-step sentence, and
// nothing ticks unless that popover is open. An older server's payload (no
// deadline_ms) keeps its reading.
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import { createWorkbench } from "./workbench-fixture.mjs";

const output = process.env.CAGE_TEST_CAPTURE || "/tmp/cage-network-explanation";
await mkdir(output, { recursive: true });
const url = process.env.CAGE_UI_URL || "http://127.0.0.1:5235";
const later = (seconds) => new Date(Date.now() + seconds * 1000).toISOString();
const at = (seconds) => Date.now() + seconds * 1000;
const closed = () => ({ mode: "closed", until: later(-60) });
const open = () => ({ mode: "open", until: later(3600) });
const advice = ["在隔离浏览器中关闭或刷新占用内存较多的标签页，或结束环境里不需要的程序", "在资源设置中查看环境内存；需要时调高内存上限（重启环境后生效）"];
// next: what the popover's next-step sentence reads, with the countdown as N.
const cases = [
  { name: "none", gate: closed, label: "外网已关闭" },
  { name: "unconfirmed", gate: open, label: "已联网 · 监测重试中", recheck: false, deadline: true, next: /^监测恢复后会自动延长联网许可，否则外网将在 \d+ 秒后自动暂停。$/, explanation: () => ({ state: "unconfirmed",
    reason: "安全检查暂时读不到浏览器的时区和语言，现有联网许可没有延长。", next: "监测恢复后会自动延长联网许可；许可到期前仍无法确认，外网会自动暂停并重检。", deadline_ms: at(45), lease_until: later(45), memory_short: false, updated_at: later(0) }) },
  { name: "waiting_memory", gate: closed, label: "外网未放行 · 内存不足", recheck: true, deadline: true, next: /^环境内存回落后会自动重检并恢复联网，最迟 \d+ 秒后。$/, explanation: () => ({ state: "waiting_memory",
    reason: "环境内存不足（共 2.8 GB），安全检查读不到系统的时区和语言，所以暂时关闭了外网。",
    next: "环境内存回落后会自动重检并恢复联网，最迟 1 分钟内也会重检。", deadline_ms: at(58), recheck_at: later(58), memory_short: true, memory_cause: "environment", advice, updated_at: later(0) }) },
  { name: "waiting_memory_browser", gate: closed, label: "外网未放行 · 内存不足", recheck: true, deadline: true, next: /^浏览器内存回落后会自动重检并恢复联网，最迟 \d+ 秒后。$/, title: "外网已暂停：隔离浏览器内存不足", explanation: () => ({ state: "waiting_memory",
    reason: "隔离浏览器内存已到上限（约 1.2 GB），安全检查读不到它的时区和语言，所以暂时关闭了外网。",
    next: "浏览器内存回落后会自动重检并恢复联网，最迟 1 分钟内也会重检。", deadline_ms: at(58), recheck_at: later(58), memory_short: true, memory_cause: "browser",
    advice: ["在隔离浏览器中关闭或刷新占用内存较多的标签页", "在资源设置中可以把内存调到 6 GB，需要重启环境"], updated_at: later(0) }) },
  { name: "recheck_scheduled", gate: closed, label: "外网未放行 · 等待自动重检", recheck: true, deadline: true, next: /^将在 (\d+ 分 )?\d+ 秒后自动重检，通过后恢复联网。$/, explanation: () => ({ state: "recheck_scheduled",
    reason: "安全检查读不到浏览器的时区和语言，所以暂时关闭了外网。", next: "稍后会自动重检，通过后恢复联网。", deadline_ms: at(125), recheck_at: later(125), updated_at: later(0) }) },
  { name: "rechecking", gate: () => ({ mode: "checking", until: later(120) }), label: "外网未放行 · 正在自动重检", recheck: false, explanation: () => ({ state: "rechecking",
    reason: "安全检查读不到浏览器的时区和语言，所以暂时关闭了外网。", next: "正在自动重检，通过后恢复联网。", updated_at: later(0) }) },
  { name: "check_required", gate: closed, label: "外网未放行 · 需要重新检测", recheck: true, explanation: () => ({ state: "check_required",
    reason: "出口地区与设置不符：检测到的出口位于日本，设置要求美国。", next: "外网保持关闭，目前没有安排自动重检。处理上述问题后请重新检测。", advice: ["在网络设置中换到美国的代理节点", "换好后点「立即重新检测」"], updated_at: later(0) }) },
  { name: "paused", gate: closed, label: "外网已手动暂停", recheck: true, explanation: () => ({ state: "paused",
    reason: "外网已手动暂停，离线终端和文件仍可使用。", next: "重新检测通过后恢复联网。", updated_at: later(0) }) },
  // An older server: its sentences carry their own numbers and the explanation counts down beside them (no deadline_ms).
  { name: "older_server", gate: closed, label: "外网未放行 · 内存不足", recheck: true, deadline: true, older: true, explanation: () => ({ state: "waiting_memory",
    reason: "环境内存不足（可用约 46 MB，共 2.8 GB），联网许可到期前仍无法读取浏览器时区和语言，外网已暂停",
    next: "外网暂停中。自动完整重检正在等待内存压力缓解，最迟约 60 秒后也会进行，通过后自动恢复联网。", recheck_at: later(60), memory_short: true, advice, updated_at: later(0) }) },
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
      // The live one-second intervals: the countdown is the only one this explanation starts.
      await page.addInitScript(() => {
        const live = new Set(), set = window.setInterval.bind(window), clear = window.clearInterval.bind(window);
        window.setInterval = (fn, ms, ...rest) => { const id = set(fn, ms, ...rest); if (ms === 1000) live.add(id); return id; };
        window.clearInterval = (id) => { live.delete(id); return clear(id); };
        window.__ticking = () => live.size;
      });
    },
  });
  const ticking = () => page.evaluate(() => window.__ticking());
  let idle;
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
        idle = await ticking();
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
      // The card: the reason and one action; the next step, the advice and the countdown are in the popover.
      assert.equal(await block.getByRole("button").count(), 1, item.name + " one action in the card");
      assert.equal(await block.locator(".network-explanation-next, .network-explanation-advice, [role=timer]").count(), 0, item.name + " details stay in the popover");
      const closedGate = item.gate().mode === "closed";
      const heading = await page.getByRole("heading", { name: /^Super Cage 环境已就绪/ }).textContent();
      assert.equal(heading, closedGate ? "Super Cage 环境已就绪 · 外网暂停" : "Super Cage 环境已就绪", item.name + " the card's title names a closed network");
      // Nothing ticks while the explanation is only the card (or the gate is fine); the popover starts one countdown.
      assert.equal(await ticking(), idle, item.name + " the card starts no timer");
      await button.click();
      const popover = page.getByRole("dialog", { name: /./ }).and(page.locator(".network-popover"));
      await popover.waitFor();
      assert.equal(await ticking(), idle + (item.deadline ? 1 : 0), item.name + " the open popover runs one countdown");
      await popover.getByText(item.explanation().reason).waitFor();
      if (item.explanation().advice) for (const line of item.explanation().advice) await popover.getByText(line).waitFor();
      const timer = popover.getByRole("timer");
      if (item.deadline) {
        const first = await timer.textContent();
        await page.waitForTimeout(1300);
        assert.notEqual(await timer.textContent(), first, item.name + " countdown is live");
      } else assert.equal(await timer.count(), 0);
      // One reason sentence and one next-step sentence, with one countdown in all.
      assert.equal(await popover.locator(".network-explanation-reason").count(), 1, item.name + " one reason");
      assert.equal(await popover.locator(".network-explanation-next").count(), 1, item.name + " one next-step sentence");
      const next = await popover.locator(".network-explanation-next").innerText();
      if (item.next) assert.match(next.replace(/\s+/g, " ").trim(), item.next, item.name + " next-step sentence");
      if (!item.older) assert.ok((next.match(/\d+\s*秒后/g) || []).length <= (item.deadline ? 1 : 0), item.name + " says the countdown once: " + next);
      if (!item.explanation().recheck_at && !item.explanation().lease_until) assert.ok(!/\d+ 秒/.test(next), item.name + " has no number: " + next);
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
        assert.equal(await ticking(), idle, item.name + " the closed popover clears its timer");
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
