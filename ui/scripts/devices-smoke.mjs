// Settings → 设备 and the phone page in device mode, against the isolated workbench fixture and a stand-in for the
// host's and the gateway's routes (devices-fixture.mjs). Every state of the page and its two dialogs in both themes at
// 1440×900 and 1000×700, then the phone page of a joined computer at 1440×900 and 1000×700. Also checks what the
// footprint rules say: the page's code is not requested and the host is not asked before the section opens, nothing is
// asked while the page or a dialog sits open, a closed dialog leaves nothing running, and device mode is inert in a
// phone app. Needs a Vite dev server (CAGE_UI_URL). Screenshots go to CAGE_TEST_CAPTURE.
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { join } from "node:path";
import { createPhoneFixture } from "../tenant/phone-fixture.mjs";
import { smokeCaptureDirectory } from "./smoke-captures.mjs";
import { createDevicesHost, devicesRoutes, sampleComputers, sampleDevices } from "./devices-fixture.mjs";
import { createWorkbench } from "./workbench-fixture.mjs";

const output = await smokeCaptureDirectory("devices");
const origin = process.env.CAGE_UI_URL || "http://127.0.0.1:5198";
const quietMs = Number(process.env.CAGE_DEVICES_QUIET_MS || 2500);
const context = { mode: "local", connection_id: "g".repeat(64), desktop_gateway: true, client_platform: "darwin", server_platform: "darwin", connection: { state: "connected" } };
const LONG = "CAGE-7QK2-M9XT-4HNA-WB3E-0RCP-YL6D";

async function desktopRun({ theme, viewport, only }) {
  const host = createDevicesHost({ devices: [], computers: [] });
  const fixture = await createWorkbench({
    viewport, clientContext: context,
    beforeNavigate: async (page, state) => {
      await page.emulateMedia({ colorScheme: theme });
      if (only === "custom") state.settings.mobile_relay = "https://relay.example.com";
      await devicesRoutes(page, host);
    },
  });
  const { page, browser, errors } = fixture;
  const name = theme + "-" + viewport.width + "x" + viewport.height;
  // A dialog fades and rises in 180 ms: the picture is taken once it has settled.
  const began = Date.now();
  const shot = async (label) => { if (process.env.CAGE_DEVICES_TIMING) console.log(String(Date.now() - began).padStart(7), label); await page.waitForTimeout(320); await page.screenshot({ path: join(output, name + "-" + label + ".png") }); };
  const chunks = [];
  page.on("request", (request) => { if (/\/(assets|src)\/devices[-.]/.test(request.url())) chunks.push(request.url()); });
  const dialog = (title) => page.getByRole("dialog", { name: title, exact: true });
  const quiet = async (label) => {
    await page.waitForTimeout(300);
    const before = host.log.length;
    await page.waitForTimeout(quietMs);
    assert.deepEqual(host.log.slice(before), [], label + ": nothing may ask the host while it sits");
  };
  // At the number the join waits for a person: nothing may ask but the one held read that waits for a change.
  const heldOnly = async (label) => {
    await page.waitForTimeout(300);
    const before = host.log.length;
    await page.waitForTimeout(quietMs);
    const asked = host.log.slice(before);
    assert.ok(asked.length <= 1 && asked.every((entry) => entry === "GET /client/devices/join"), label + ": at most the one held read, asked " + JSON.stringify(asked));
  };
  const fits = async (title) => {
    const box = await dialog(title).boundingBox();
    assert.ok(box.x >= 0 && box.y >= 0 && box.x + box.width <= viewport.width && box.y + box.height <= viewport.height, title + " fits the window: " + JSON.stringify(box));
  };
  try {
    // Before the section opens: no code, no request.
    assert.deepEqual(chunks, []);
    assert.deepEqual(host.log, []);
    const nav = page.getByRole("navigation", { name: "设置分组" });
    if (only === "custom") {
      await page.getByRole("button", { name: "设置", exact: true }).click();
      await nav.getByRole("button", { name: "设备", exact: true }).click();
      await page.getByRole("heading", { name: "设备", exact: true }).waitFor();
      assert.equal(await page.getByRole("button", { name: "自建中继", exact: true }).getAttribute("aria-pressed"), "true");
      assert.equal(await page.getByLabel("自建中继地址").inputValue(), "https://relay.example.com");
      await page.getByLabel("管理令牌（可选）").fill("secret-token");
      await page.getByRole("button", { name: "添加设备", exact: true }).click();
      await dialog("添加设备").getByText("这台电脑使用自建中继 relay.example.com，对方也要设置同一个中继。").waitFor();
      assert.deepEqual(host.inviteBody, { relay: "https://relay.example.com", admin: "secret-token" }, "the saved relay goes with its token");
      await shot("add-custom-relay");
      await fits("添加设备");
      await dialog("添加设备").getByRole("button", { name: "关闭", exact: true }).click();
      // An unsaved address cannot make a code: the setting is saved first.
      await page.getByLabel("自建中继地址").fill("https://other.example.com");
      await page.getByText("保存设置后，可使用新中继添加设备。").waitFor();
      assert.equal(await page.getByRole("button", { name: "添加设备", exact: true }).isDisabled(), true);
      assert.deepEqual(errors, []);
      console.log("PASS: " + name + " custom relay: the line in the dialog, unsaved address holds 添加设备");
      return;
    }

    // The sidebar row opens the same page.
    await page.getByRole("button", { name: "设备", exact: true }).click();
    await page.getByRole("heading", { name: "设备", exact: true }).waitFor();
    assert.equal(await nav.getByRole("button", { name: "设备", exact: true }).getAttribute("aria-current"), "page");
    assert.equal(await nav.getByRole("button", { name: "远程连接", exact: true }).count(), 0, "the old section is gone");
    assert.ok(chunks.length > 0, "the page's code loads when the section opens");

    // Empty: the drawing, the heading, one clay button, 加入其他电脑 beside nothing else.
    await page.getByRole("heading", { name: "还没有设备", exact: true }).waitFor();
    await page.getByText("手机和其他电脑可以连接这台电脑上的工作台。", { exact: true }).waitFor();
    assert.equal(await page.locator(".settings-content button.clay").count(), 1, "one clay control in the empty view");
    assert.equal(await page.locator(".devices-empty .drawing svg").count(), 1);
    await page.getByRole("button", { name: "加入其他电脑", exact: true }).waitFor();
    await shot("empty");
    await quiet("empty page");
    assert.equal(host.count(/^GET \/devices$/), 1);
    assert.equal(host.count(/^GET \/client\/devices$/), 1);
    assert.equal(host.count(/^GET \/devices\/invite/), 0, "no code, no held read");

    // The list: three devices, one connected; joined computers; the relay choice at the bottom.
    host.devices = sampleDevices();
    host.computers = sampleComputers();
    await page.evaluate(() => window.dispatchEvent(new Event("focus")));
    await page.getByText("3 台设备 · 1 台在线", { exact: true }).waitFor();
    const rows = page.getByRole("list", { name: undefined }).locator(".devices-row");
    assert.equal(await page.locator(".devices-row").count(), 5);
    const phone = page.locator(".devices-row", { hasText: "iPhone 15" });
    await phone.getByText("手机 · iOS · 已连接", { exact: true }).waitFor();
    assert.equal(await phone.getByRole("img", { name: "已连接" }).count(), 1);
    await page.locator(".devices-row", { hasText: "Google Pixel 9 Pro" }).getByText(/^手机 · Android · 上次连接 3 小时前$/).waitFor();
    await page.locator(".devices-row", { hasText: "办公室的 Windows 台式机" }).getByText(/^电脑 · Windows · 上次连接 (昨天|前天|\d+月\d+日)|电脑 · Windows · 上次连接 /).waitFor();
    assert.equal(await page.locator(".devices-live").count(), 1, "the live dot is on the connected device only");
    await page.getByText("有设备配对时，这台电脑保持运行，屏幕可以熄灭。", { exact: false }).waitFor();
    await page.getByRole("heading", { name: "我加入的电脑", exact: true }).waitFor();
    await page.locator(".devices-row", { hasText: "家里的 Mac mini" }).getByText(/^电脑 · macOS · 上次打开 /).waitFor();
    await page.locator(".devices-row", { hasText: "实验室的 Linux 工作站" }).getByText("电脑 · 还没有打开过", { exact: true }).waitFor();
    await page.getByRole("heading", { name: "中继服务", exact: true }).waitFor();
    assert.equal(await page.locator(".settings-content button.clay").count(), 1, "one clay control with devices listed");
    assert.equal(await page.getByRole("button", { name: "添加设备", exact: true }).getAttribute("class"), "clay");
    assert.equal(await page.locator(".settings-content").evaluate((node) => node.scrollWidth > node.clientWidth), false, "no sideways scroll");
    await shot("list");
    await quiet("listed page");

    // Removing: a quiet confirm that says what happens; cancelling changes nothing.
    await page.getByRole("button", { name: "移除 Google Pixel 9 Pro", exact: true }).click();
    const confirm = page.getByRole("dialog").last();
    await confirm.getByText("移除 Google Pixel 9 Pro 后，它要重新配对才能访问这台电脑。正在进行的操作会立即中断。", { exact: true }).waitFor();
    assert.equal(await confirm.getByRole("button", { name: "移除", exact: true }).getAttribute("class"), "danger-fill");
    await shot("remove-confirm");
    await confirm.getByRole("button", { name: "取消", exact: true }).click();
    assert.equal(host.count(/^DELETE \/devices\/d2$/), 0);
    await page.getByRole("button", { name: "移除 家里的 Mac mini", exact: true }).click();
    await page.getByRole("dialog").last().getByText("只会从这台电脑上忘记它。还需要在那台电脑的“设备”里移除这台电脑。", { exact: true }).waitFor();
    await page.getByRole("dialog").last().getByRole("button", { name: "取消", exact: true }).click();
    assert.equal(host.count(/^DELETE \/client\/devices\/h1$/), 0);
    await page.getByRole("button", { name: "移除 Google Pixel 9 Pro", exact: true }).click();
    await page.getByRole("dialog").last().getByRole("button", { name: "移除", exact: true }).click();
    await page.getByText("2 台设备 · 1 台在线", { exact: true }).waitFor();
    assert.equal(host.count(/^DELETE \/devices\/d2$/), 1);
    assert.equal(await page.locator(".devices-row", { hasText: "Google Pixel 9 Pro" }).count(), 0);
    // A removal the host refuses says why, on the row.
    host.failNext["DELETE /devices/d3"] = { status: 500, body: "移除没有完成，请重试。" };
    await page.getByRole("button", { name: "移除 办公室的 Windows 台式机", exact: true }).click();
    await page.getByRole("dialog").last().getByRole("button", { name: "移除", exact: true }).click();
    await page.locator(".devices-row", { hasText: "办公室的 Windows 台式机" }).getByText("移除没有完成，请重试。", { exact: true }).waitFor();

    // At the limit: the sentence, and no way to add.
    host.devices = [...sampleDevices(), { id: "d4", kind: "phone", platform: "ios", name: "iPad", last_seen: "" }, { id: "d5", kind: "phone", platform: "android", name: "Pixel 7", last_seen: "" }];
    await page.evaluate(() => window.dispatchEvent(new Event("focus")));
    await page.getByText("已达到 5 台设备，请先移除一台。", { exact: true }).waitFor();
    assert.equal(await page.getByRole("button", { name: "添加设备", exact: true }).isDisabled(), true);
    await page.locator(".devices-row", { hasText: "Pixel 7" }).getByText("手机 · Android · 还没有连接过", { exact: true }).waitFor();
    await shot("limit");
    host.devices = sampleDevices().slice(0, 3);
    await page.evaluate(() => window.dispatchEvent(new Event("focus")));
    await page.getByText("3 台设备 · 1 台在线", { exact: true }).waitFor();

    // 添加设备: the code, the QR, the countdown, one sentence per kind of device.
    await page.getByRole("button", { name: "添加设备", exact: true }).click();
    const add = dialog("添加设备");
    await add.locator(".devices-digits").waitFor();
    assert.deepEqual(host.inviteBody, { relay: "", admin: "" }, "the official relay: nothing to send but the empty relay and token");
    const digits = await add.locator(".devices-digits").innerText();
    assert.match(digits, /^\d{4} \d{4}$/);
    assert.match(await add.locator(".devices-countdown").innerText(), /^\d:\d\d 后失效$/);
    assert.equal(await add.locator("img.devices-qr").evaluate((image) => image.complete && image.naturalWidth > 0), true);
    const qr = await add.locator("img.devices-qr").boundingBox();
    assert.ok(Math.abs(qr.width - 320) < 1.5 && Math.abs(qr.height - 320) < 1.5 || viewport.width < 900, "the QR is 320×320: " + JSON.stringify(qr));
    await add.getByText("打开 Super Cage 手机端，点「连接电脑」→「扫码连接」，扫这个二维码。").waitFor();
    await add.getByText("在那台电脑的 Super Cage 里点「设备」→「加入其他电脑」，输入上面的 8 位数字。").waitFor();
    await add.getByText("只给你自己的设备看这个配对码；关闭窗口后它立即失效。").waitFor();
    assert.equal(await add.getByRole("button", { name: "关闭", exact: true }).count(), 1);
    assert.equal(await add.locator("button.clay").count(), 0, "the clay button stays on the page");
    await shot("add-code");
    await fits("添加设备");
    assert.equal(host.count(/^GET \/devices\/invite/) >= 1, true, "a held read follows the code");
    // The countdown moves while the code is on show.
    const first = await add.locator(".devices-countdown").innerText();
    await page.waitForTimeout(2200);
    assert.notEqual(await add.locator(".devices-countdown").innerText(), first);
    // The window shares its clipboard with the environment (the desktop app does while one is ready): a program there
    // reads what is copied without anyone pasting. The long code, which pairs without approval, is not offered for
    // copying; the 8 digits, which need 允许, stay on show.
    await page.evaluate(() => { window.cageSyncClipboard = () => Promise.resolve(); });
    await add.getByText("共享剪贴板已开启，环境内的程序能读到剪贴板。请扫码，或使用 8 位配对码。", { exact: true }).waitFor();
    assert.equal(await add.getByRole("button", { name: "复制长配对码", exact: true }).count(), 0, "no copying of the long code");
    assert.match(await add.locator(".devices-digits").innerText(), /^\d{4} \d{4}$/);
    await shot("add-shared-clipboard");
    await page.evaluate(() => { delete window.cageSyncClipboard; });
    await add.getByRole("button", { name: "复制长配对码", exact: true }).click();
    await add.getByText("长配对码已复制，可以粘贴到另一台电脑或手机。").waitFor();
    assert.equal(await add.getByText("共享剪贴板已开启", { exact: false }).count(), 0);
    // A new code replaces the old one.
    await add.getByRole("button", { name: "重新生成", exact: true }).click();
    await add.getByText("已换成新的配对码，之前的已失效。").waitFor();
    assert.notEqual(await add.locator(".devices-digits").innerText(), digits);
    assert.equal(host.mints, 2);
    await shot("add-refreshed");

    // The other computer asks: approval with the check number.
    host.say("request", { request: { name: "MacBook Air", platform: "darwin", check: "3817" } });
    await add.getByRole("heading", { name: "MacBook Air（macOS）想加入这台电脑", exact: true }).waitFor();
    await add.getByText("核对数字 3817，与那台电脑上显示的一致再允许。", { exact: true }).waitFor();
    assert.equal(await add.getByRole("button", { name: "允许", exact: true }).getAttribute("class"), "primary");
    assert.equal(await add.getByRole("button", { name: "拒绝", exact: true }).count(), 1);
    assert.equal(await add.locator(".devices-digits").count(), 0, "the code is replaced by the prompt");
    await shot("add-approval");
    await fits("添加设备");
    await add.getByRole("button", { name: "拒绝", exact: true }).click();
    await add.getByText("已拒绝 MacBook Air，这个配对码已失效。", { exact: true }).waitFor();
    assert.equal(host.count(/^POST \/devices\/invite\/reject$/), 1);
    await shot("add-rejected");
    await add.getByRole("button", { name: "重新生成", exact: true }).click();
    await add.locator(".devices-digits").waitFor();
    host.say("request", { request: { name: "MacBook Air", platform: "darwin", check: "5521" } });
    await add.getByText("核对数字 5521，与那台电脑上显示的一致再允许。", { exact: true }).waitFor();
    await add.getByRole("button", { name: "允许", exact: true }).click();
    // 允许 is not the end: the other computer confirms the number on its side, and nothing is saved before that.
    const waitingFor = add.getByRole("group", { name: "等待对方确认", exact: true });
    await waitingFor.waitFor();
    assert.equal((await waitingFor.innerText()).replace(/\s+/g, " ").trim(), "已允许，等待对方确认 核对数字 5521，对方在它的屏幕上确认一致后才会添加；关闭这个窗口会取消。");
    assert.equal(host.count(/^POST \/devices\/invite\/approve$/), 1);
    assert.equal(await add.getByText("已添加", { exact: false }).count(), 0, "not added before the other computer confirms");
    assert.equal(host.devices.some((device) => device.name === "MacBook Air"), false);
    assert.equal(await add.getByRole("button", { name: "取消", exact: true }).count(), 1);
    await shot("add-confirming");
    await fits("添加设备");
    host.finishJoin();
    await add.getByText("已添加 MacBook Air", { exact: true }).waitFor();
    await shot("add-joined");
    await add.getByRole("button", { name: "完成", exact: true }).click();
    await add.waitFor({ state: "detached" });
    await page.getByText("4 台设备 · 2 台在线", { exact: true }).waitFor();
    assert.equal(host.count(/^DELETE \/devices\/invite$/) >= 0, true);

    // The other computer does not confirm (不一致, no answer, or it closed its dialog): the host says so, nothing is added.
    await page.getByRole("button", { name: "添加设备", exact: true }).click();
    await add.locator(".devices-digits").waitFor();
    host.say("request", { request: { name: "MacBook Air", platform: "darwin", check: "6407" } });
    await add.getByRole("button", { name: "允许", exact: true }).click();
    await waitingFor.waitFor();
    const devicesBefore = host.devices.length;
    host.say("failed", { error: "对方没有确认核对数字，没有添加。" });
    await add.getByText("对方没有确认核对数字，没有添加。", { exact: true }).waitFor();
    assert.equal(host.devices.length, devicesBefore, "nothing added");
    await shot("add-not-confirmed");
    await add.getByRole("button", { name: "重新生成", exact: true }).click();
    await add.locator(".devices-digits").waitFor();
    // Closing the dialog while the other computer is asked ends it.
    const cancels = host.count(/^DELETE \/devices\/invite$/);
    host.say("request", { request: { name: "MacBook Air", platform: "darwin", check: "6408" } });
    await add.getByRole("button", { name: "允许", exact: true }).click();
    await waitingFor.waitFor();
    await add.getByRole("button", { name: "取消", exact: true }).click();
    await add.waitFor({ state: "detached" });
    assert.equal(host.count(/^DELETE \/devices\/invite$/), cancels + 1, "取消 ends the invite");

    // A code that runs out, and one that fails.
    await page.getByRole("button", { name: "添加设备", exact: true }).click();
    await add.locator(".devices-digits").waitFor();
    host.say("expired");
    await add.getByText("配对码已过期", { exact: true }).waitFor();
    await shot("add-expired");
    await add.getByRole("button", { name: "重新生成", exact: true }).click();
    await add.locator(".devices-digits").waitFor();
    host.say("failed");
    await add.getByText("配对失败：配对码不对或连接被篡改。请重新生成。", { exact: true }).waitFor();
    await shot("add-failed");
    await add.getByRole("button", { name: "关闭", exact: true }).click();
    await add.waitFor({ state: "detached" });

    // The host is full although the list was read before: the host's sentence is shown.
    host.freeze();
    const full = host.limit;
    host.limit = host.devices.length;
    await page.getByRole("button", { name: "添加设备", exact: true }).click();
    await add.getByText("已达到 " + host.limit + " 台设备，请先移除一台。", { exact: true }).waitFor();
    assert.equal(host.count(/^POST \/devices\/invite$/) > 0, true);
    await shot("add-limit");
    await add.getByRole("button", { name: "关闭", exact: true }).click();
    host.limit = full;
    host.thaw();

    // A host without short codes: the QR and the long code, as before.
    host.short = false;
    await page.getByRole("button", { name: "添加设备", exact: true }).click();
    await add.locator("img.devices-qr").waitFor();
    assert.equal(await add.locator(".devices-digits").count(), 0);
    await add.getByText("点「复制长配对码」，在那台电脑的 Super Cage 里点「设备」→「加入其他电脑」，粘贴。").waitFor();
    await shot("add-long-only");
    await add.getByRole("button", { name: "关闭", exact: true }).click();
    await add.waitFor({ state: "detached" });
    host.short = true;

    // Closing a code on show cancels it; afterwards nothing runs.
    const deletes = host.count(/^DELETE \/devices\/invite$/);
    await page.getByRole("button", { name: "添加设备", exact: true }).click();
    await add.locator(".devices-digits").waitFor();
    await add.getByRole("button", { name: "关闭", exact: true }).click();
    await add.waitFor({ state: "detached" });
    await page.getByText("4 台设备 · 2 台在线", { exact: true }).waitFor();
    assert.equal(host.count(/^DELETE \/devices\/invite$/), deletes + 1, "the code dies with the dialog");
    await quiet("after the dialog closed");

    // 加入其他电脑.
    await page.getByRole("button", { name: "加入其他电脑", exact: true }).click();
    const join = dialog("加入其他电脑");
    const field = join.getByLabel("配对码", { exact: true });
    await field.waitFor();
    assert.equal(await join.getByRole("button", { name: "加入", exact: true }).isDisabled(), true);
    await join.getByText("在那台电脑的 Super Cage 里点“设备”→“添加设备”，输入显示的 8 位数字。也可以粘贴长配对码。", { exact: true }).waitFor();
    const label = join.getByLabel("电脑名称", { exact: true });
    assert.equal(await label.getAttribute("maxlength"), "40");
    await shot("join");
    await field.fill("48219");
    assert.equal(await field.inputValue(), "4821 9");
    assert.equal(await join.getByRole("button", { name: "加入", exact: true }).isDisabled(), true);
    await field.fill("4821-9037");
    assert.equal(await field.inputValue(), "4821 9037");
    assert.equal(await join.getByRole("button", { name: "加入", exact: true }).isDisabled(), false);
    await field.fill(LONG);
    assert.equal(await field.inputValue(), LONG);
    assert.equal(await join.getByRole("button", { name: "加入", exact: true }).isDisabled(), false);
    await field.fill("hello");
    await join.getByText("配对码是 8 位数字，或以 CAGE 开头的长配对码。").waitFor();
    // Typing digits: grouped as typed.
    await field.fill("");
    await field.pressSequentially("48219037", { delay: 10 });
    assert.equal(await field.inputValue(), "4821 9037");
    await label.fill("家里的 Mac mini");
    await join.getByRole("button", { name: "加入", exact: true }).click();
    await join.getByText("在那台电脑上点“允许”。核对数字 3817", { exact: true }).waitFor();
    assert.equal(host.count(/^POST \/client\/devices\/join 48219037$/), 1, "eight digits, no space");
    assert.deepEqual(host.joinBody, { code: "48219037", name: "家里的 Mac mini" }, "the name goes with the code");
    assert.equal(await label.isDisabled(), true);
    await shot("join-approval");
    await fits("加入其他电脑");
    // The other computer allows: this one shows the number again and saves nothing until 确认.
    const agree = join.getByRole("group", { name: "核对数字", exact: true });
    const agreeText = async () => (await agree.innerText()).replace(/\s+/g, " ").trim();
    host.joinSay("confirm", { check: "3817" });
    await agree.waitFor();
    assert.equal(await agreeText(), "那台电脑已允许加入 核对数字 3817，与那台电脑上显示的数字一致再确认。");
    assert.equal(await join.getByText("已加入", { exact: false }).count(), 0, "not joined before 确认");
    assert.equal(host.computers.some((item) => item.id === "h3"), false, "nothing is saved before 确认");
    assert.equal(await join.getByRole("button", { name: "确认", exact: true }).isDisabled(), false);
    assert.equal(await join.getByRole("button", { name: "不一致", exact: true }).isDisabled(), false);
    await shot("join-confirm");
    await fits("加入其他电脑");
    // Nothing ticks while a person decides: at most the one held read that waits for a change.
    await heldOnly("the number waits for 确认");
    assert.equal(host.answers.length, 0);
    await join.getByRole("button", { name: "确认", exact: true }).click();
    await join.getByText("已加入「家里的 Mac mini」", { exact: true }).waitFor();
    assert.deepEqual(host.answers, [{ path: "/client/devices/join/confirm", body: { check: "3817" } }], "the number shown goes back with 确认");
    assert.equal(host.computers.some((item) => item.id === "h3"), true, "确认 saves the computer");
    await shot("join-joined");
    await join.getByRole("button", { name: "完成", exact: true }).click();
    await join.waitFor({ state: "detached" });
    await page.locator(".devices-row", { hasText: "家里的 Mac mini" }).first().waitFor();
    await page.getByText("电脑 · 还没有打开过", { exact: true }).first().waitFor();

    const computersAfterJoin = host.computers.length;
    const reopen = () => page.getByRole("button", { name: "加入其他电脑", exact: true }).click();
    const ask = async (digits) => {
      await field.fill(digits);
      await join.getByRole("button", { name: "加入", exact: true }).click();
      await join.getByText("在那台电脑上点“允许”。核对数字 3817", { exact: true }).waitFor();
      await host.joinHeld();
    };

    // Why a join fails.
    for (const [mode, text] of [
      ["notfound", "没有找到这个配对码：请核对数字，并确认两台电脑使用同一个中继。"],
      ["used", "配对码已被使用，请在那台电脑上重新生成。"],
      ["relay", "这个配对码属于中继 relay.example.com，而这台电脑使用官方中继。请在“设备”里选择同一个中继。"],
    ]) {
      host.joinMode = mode;
      await page.getByRole("button", { name: "加入其他电脑", exact: true }).click();
      await field.fill("00000000");
      await join.getByRole("button", { name: "加入", exact: true }).click();
      await join.getByText(text, { exact: true }).waitFor();
      if (mode === "notfound") await shot("join-error");
      await field.fill("0000000");
      assert.equal(await join.getByText(text, { exact: true }).count(), 0, "typing clears the sentence");
      await join.getByRole("button", { name: "关闭", exact: true }).click();
      await join.waitFor({ state: "detached" });
    }
    // The other computer says no.
    host.joinMode = "approval";
    await page.getByRole("button", { name: "加入其他电脑", exact: true }).click();
    await field.fill("11112222");
    await join.getByRole("button", { name: "加入", exact: true }).click();
    await join.getByText("在那台电脑上点“允许”。核对数字 3817", { exact: true }).waitFor();
    host.joinSay("denied");
    await join.getByText("那台电脑没有允许加入。", { exact: true }).waitFor();
    // Allowed, but the numbers differ: 不一致 saves nothing, on either side.
    await ask("33334444");
    host.joinSay("confirm", { check: "5206" });
    await agree.waitFor();
    assert.equal(await agreeText(), "那台电脑已允许加入 核对数字 5206，与那台电脑上显示的数字一致再确认。");
    await join.getByRole("button", { name: "不一致", exact: true }).click();
    await join.getByText("两台电脑的核对数字不一致，没有加入，也没有保存任何内容。", { exact: true }).waitFor();
    assert.deepEqual(host.answers.at(-1), { path: "/client/devices/join/reject", body: {} });
    assert.equal(host.computers.length, computersAfterJoin, "nothing saved");
    // Closing the dialog ends the join, while the other computer has not answered yet and while the number waits.
    await field.fill("33334445");
    await join.getByRole("button", { name: "加入", exact: true }).click();
    await join.getByText("在那台电脑上点“允许”。核对数字 3817", { exact: true }).waitFor();
    await page.keyboard.press("Escape");
    await join.waitFor({ state: "detached" });
    assert.equal(host.count(/^DELETE \/client\/devices\/join$/), 1, "closing ends the join");
    await reopen();
    await ask("33334446");
    host.joinSay("confirm", { check: "7714" });
    await agree.waitFor();
    await join.getByRole("button", { name: "关闭面板", exact: true }).click();
    await join.waitFor({ state: "detached" });
    assert.equal(host.count(/^DELETE \/client\/devices\/join$/), 2);
    assert.equal(host.count(/\/join\/(confirm|reject)$/), 2, "nothing was answered by closing: only the 不一致 above and the 确认 before");
    assert.equal(host.computers.length, computersAfterJoin, "nothing saved");
    // The other computer closes its dialog while the number waits: said at once, the question goes away.
    await reopen();
    await ask("33334447");
    host.joinSay("confirm", { check: "7715" });
    await agree.waitFor();
    host.joinSay("failed", { error: "对方电脑已取消，没有加入，也没有保存任何内容。" });
    await join.getByText("对方电脑已取消，没有加入，也没有保存任何内容。", { exact: true }).waitFor();
    await agree.waitFor({ state: "detached" });
    // A number the gateway does not show is refused and the question stays: the sentence is shown, 不一致 still ends it.
    await ask("33334448");
    host.joinSay("confirm", { check: "7716" });
    await agree.waitFor();
    host.joinState = { state: "confirm", check: "0000" };
    await join.getByRole("button", { name: "确认", exact: true }).click();
    await join.getByText("核对数字不一致：只有两台电脑显示相同的数字时才能确认", { exact: true }).waitFor();
    assert.equal(await agreeText(), "那台电脑已允许加入 核对数字 7716，与那台电脑上显示的数字一致再确认。 核对数字不一致：只有两台电脑显示相同的数字时才能确认");
    await join.getByRole("button", { name: "不一致", exact: true }).click();
    await join.getByText("两台电脑的核对数字不一致，没有加入，也没有保存任何内容。", { exact: true }).waitFor();
    // An answer that finds nothing waiting any more (the time ran out meanwhile) starts over, saving nothing.
    await ask("33334449");
    host.joinSay("confirm", { check: "7717" });
    await agree.waitFor();
    host.failNext["POST /client/devices/join/confirm"] = { status: 409, body: "没有等待确认的加入" };
    await join.getByRole("button", { name: "确认", exact: true }).click();
    await join.getByText("这次加入已失效，请在那台电脑上重新生成配对码。", { exact: true }).waitFor();
    assert.equal(host.computers.length, computersAfterJoin, "nothing saved");
    // A long code joins at once, with nothing to approve.
    host.joinMode = "ok";
    await field.fill(LONG);
    await join.getByRole("button", { name: "加入", exact: true }).click();
    await join.getByText("已加入「另一台电脑」", { exact: true }).waitFor();
    assert.equal(host.count(new RegExp("^POST /client/devices/join " + LONG + "$")), 1);
    assert.deepEqual(host.joinBody, { code: LONG }, "no name asked for, none sent: the gateway says 「另一台电脑」");
    await join.getByRole("button", { name: "完成", exact: true }).click();
    await join.waitFor({ state: "detached" });
    await quiet("after the join dialogs closed");

    // The list of joined computers cannot be read: one line says so, and its one button clears it (the gateway moves
    // the file aside) and opens 加入其他电脑.
    const keptComputers = host.computers;
    host.joinedProblem = "已加入的电脑列表无法读取";
    host.computers = [];
    await page.evaluate(() => window.dispatchEvent(new Event("focus")));
    const joinedProblem = page.getByRole("alert").filter({ hasText: "已加入的电脑列表无法读取" });
    await joinedProblem.waitFor();
    assert.equal(await page.getByRole("heading", { name: "我加入的电脑", exact: true }).count(), 0);
    await page.getByRole("button", { name: "加入其他电脑", exact: true }).waitFor();
    await shot("joined-unreadable");
    await joinedProblem.getByRole("button", { name: "清除并重新加入", exact: true }).click();
    await field.waitFor();
    await joinedProblem.waitFor({ state: "detached" });
    assert.equal(host.count(/^DELETE \/client\/devices$/), 1, "cleared once");
    await join.getByRole("button", { name: "关闭", exact: true }).click();
    await join.waitFor({ state: "detached" });
    host.computers = keptComputers;
    await page.evaluate(() => window.dispatchEvent(new Event("focus")));
    await page.locator(".devices-row", { hasText: "家里的 Mac mini" }).first().waitFor();

    // 打开: a refusal is told on the row; success keeps the connection and goes to the phone page.
    host.failNext["POST /client/devices/h2/open"] = { status: 502, body: "连接不上那台电脑，请确认它已打开 Super Cage 并联网。" };
    await page.locator(".devices-row", { hasText: "实验室的 Linux 工作站" }).getByRole("button", { name: "打开", exact: true }).click();
    await page.locator(".devices-row", { hasText: "实验室的 Linux 工作站" }).getByText("连接不上那台电脑，请确认它已打开 Super Cage 并联网。", { exact: true }).waitFor();
    assert.deepEqual(errors, []);
    await page.locator(".devices-row", { hasText: "家里的 Mac mini" }).first().getByRole("button", { name: "打开", exact: true }).click();
    await page.waitForURL((url) => url.pathname === "/mobile.html");
    assert.equal(await page.evaluate(() => sessionStorage.getItem("cage-cli-local-connection")), "device:h1:1");
    assert.equal(await page.evaluate(() => sessionStorage.getItem("cage-device-name")), "家里的 Mac mini");
    console.log("PASS: " + name + " empty, list, remove, limit, add (code, refreshed, approval, rejected, joined, expired, failed, limit, long code only), join (approval, joined, errors, denied, long code), open");
  } finally { await browser.close(); }
}

// The phone page of a joined computer.
const require = createRequire(import.meta.url);
async function phoneRun({ theme, viewport }) {
  const engines = require(process.env.CAGE_PLAYWRIGHT_MODULE || "playwright");
  const browser = await engines[process.env.CAGE_TEST_BROWSER || "chromium"].launch({ headless: true, executablePath: process.env.CAGE_TEST_BROWSER_PATH });
  const name = theme + "-" + viewport.width + "x" + viewport.height;
  const answer = createPhoneFixture();
  async function open({ agent, key = "device:h1:1" }) {
    const ctx = await browser.newContext({ viewport, colorScheme: theme, ...(agent ? { userAgent: agent } : {}) });
    const page = await ctx.newPage();
    const seen = { mobile: [], strays: [], closes: [], errors: [] };
    page.on("pageerror", (error) => seen.errors.push(error.message));
    await page.addInitScript(([connection]) => {
      // Only the page the joined computer opens: after 返回本机 the key stays gone.
      if (location.pathname === "/mobile.html") {
        sessionStorage.setItem("cage-cli-local-connection", connection);
        sessionStorage.setItem("cage-device-name", "家里的 Mac mini");
      }
      // The desktop app's own bridge: a window with it must not be sent back to the desktop workbench.
      window.webkit = { messageHandlers: { cage: { postMessage() {} } } };
    }, [key]);
    // Nothing may reach the Vite proxy (and a service on this Mac): every /api request is answered here.
    await page.route("**/api/**", async (route) => {
      const request = route.request();
      const url = new URL(request.url());
      const header = request.headers()["x-cage-connection"] ?? null;
      if (url.pathname.startsWith("/api/mobile/")) {
        seen.mobile.push({ path: url.pathname, header });
        const result = answer(request.method(), url.pathname, request.postData() ? request.postDataJSON() : undefined, url.search);
        return route.fulfill({ status: result.status, ...(typeof result.body === "string" ? { contentType: "text/plain; charset=utf-8", body: result.body } : { json: result.body }) });
      }
      if (/^\/api\/client\/devices\/h1\/close$/.test(url.pathname)) {
        seen.closes.push({ method: request.method(), header });
        return route.fulfill({ status: 204, body: "" });
      }
      seen.strays.push(request.method() + " " + url.pathname);
      return route.fulfill({ status: 404, contentType: "text/plain; charset=utf-8", body: "没有这个接口" });
    });
    await page.goto(origin + "/mobile.html");
    return { page, ctx, seen };
  }
  try {
    const { page, ctx, seen } = await open({});
    const shot = (label) => page.screenshot({ path: join(output, "phone-" + name + "-" + label + ".png") });
    const bar = page.getByRole("region", { name: "设备", exact: true });
    await bar.waitFor();
    await bar.getByText("家里的 Mac mini · 手机版功能", { exact: true }).waitFor();
    await bar.getByRole("button", { name: "返回本机", exact: true }).waitFor();
    const drawer = page.getByRole("dialog", { name: "工作区侧栏" });
    await drawer.waitFor();
    await drawer.getByRole("button", { name: "返回本机", exact: true }).waitFor();
    assert.equal(await drawer.getByText("退出", { exact: true }).count(), 0, "退出 becomes 返回本机");
    assert.equal(await drawer.getByText("电脑版", { exact: true }).count(), 0);
    await page.getByRole("button", { name: /整理发布说明/ }).first().waitFor();
    assert.equal(await page.evaluate(() => document.documentElement.classList.contains("device-mode")), true);
    // The bar sits above the app: nothing overlaps, nothing is cut off.
    const barBox = await bar.boundingBox();
    const appBox = await page.locator(".mobile-app").boundingBox();
    assert.ok(barBox.y === 0 && barBox.height >= 30 && barBox.height <= 40, "the bar is slim: " + JSON.stringify(barBox));
    assert.ok(appBox.y >= barBox.y + barBox.height - 1 && appBox.y + appBox.height <= viewport.height + 1, "the app starts under the bar and ends in the window: " + JSON.stringify({ barBox, appBox }));
    await shot("home");
    // The phone surface of that computer: terminals, files, environment, with the device header on every request.
    await drawer.getByRole("button", { name: "终端", exact: true }).click();
    await page.getByText("在手机上直接操作环境里的终端").waitFor();
    await drawer.getByRole("button", { name: "文件", exact: true }).click();
    await page.getByRole("button", { name: /^README\.md/ }).waitFor();
    await drawer.getByRole("button", { name: "环境", exact: true }).click();
    await page.locator(".env-hero h1").waitFor();
    await shot("environment");
    assert.ok(seen.mobile.length >= 4, "the page asked the host for projects, sessions and more: " + seen.mobile.length);
    assert.deepEqual(seen.mobile.filter((entry) => entry.header !== "device:h1:1"), [], "every /api/mobile request carries the device connection");
    assert.deepEqual(seen.strays, [], "nothing else asks (no tenant context, no desktop route)");
    assert.equal(await page.evaluate(() => sessionStorage.getItem("cage-native-desktop")), null, "a native desktop window is not sent back to the desktop workbench");
    assert.equal(new URL(page.url()).pathname, "/mobile.html");
    assert.deepEqual(seen.errors, []);
    // 返回本机: the link is closed without the device header, the connection is forgotten, and the desktop workbench returns.
    await bar.getByRole("button", { name: "返回本机", exact: true }).click();
    await page.waitForURL((url) => url.pathname === "/");
    await page.waitForFunction(() => true);
    for (let i = 0; i < 40 && !seen.closes.length; i++) await page.waitForTimeout(50);
    assert.deepEqual(seen.closes, [{ method: "POST", header: null }]);
    assert.equal(await page.evaluate(() => sessionStorage.getItem("cage-cli-local-connection")), null);
    assert.equal(await page.evaluate(() => sessionStorage.getItem("cage-device-name")), null);
    await ctx.close();

    // The drawer's own way back does the same.
    const second = await open({});
    await second.page.getByRole("dialog", { name: "工作区侧栏" }).getByRole("button", { name: "返回本机", exact: true }).click();
    await second.page.waitForURL((url) => url.pathname === "/");
    for (let i = 0; i < 40 && !second.seen.closes.length; i++) await second.page.waitForTimeout(50);
    assert.equal(second.seen.closes.length, 1);
    await second.ctx.close();

    // Leaving works when the close cannot be delivered.
    const third = await open({});
    await third.page.route("**/api/client/devices/h1/close", (route) => route.abort());
    await third.page.getByRole("region", { name: "设备", exact: true }).getByRole("button", { name: "返回本机", exact: true }).click();
    await third.page.waitForURL((url) => url.pathname === "/");
    await third.ctx.close();

    // In a phone app the device connection means nothing: no bar, no device layout, no way back to anywhere.
    const app = await open({ agent: "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CageMobile/1" });
    await app.page.locator(".mobile-app").waitFor();
    assert.equal(await app.page.locator(".device-bar").count(), 0);
    assert.equal(await app.page.evaluate(() => document.documentElement.classList.contains("device-mode")), false);
    assert.equal(await app.page.getByText("返回本机").count(), 0);
    await app.ctx.close();
    // A plain /mobile.html with some other connection (the older desktop use of the page) is not device mode either.
    const other = await open({ key: "local" });
    await other.page.locator(".mobile-app").waitFor();
    assert.equal(await other.page.locator(".device-bar").count(), 0);
    assert.equal(await other.page.evaluate(() => document.documentElement.classList.contains("device-mode")), false);
    await other.ctx.close();
    console.log("PASS: " + name + " phone page in device mode: bar, drawer, header on every request, 返回本机 (both ways, also offline), inert in an app and for other connections");
  } finally { await browser.close(); }
}

// CAGE_DEVICES_ONLY=desktop|phone runs one half.
const only = process.env.CAGE_DEVICES_ONLY || "";
if (only === "one") await desktopRun({ theme: "light", viewport: { width: 1440, height: 900 } });
else if (only !== "phone") {
  for (const theme of ["light", "dark"]) {
    for (const viewport of [{ width: 1440, height: 900 }, { width: 1000, height: 700 }]) await desktopRun({ theme, viewport });
  }
  await desktopRun({ theme: "light", viewport: { width: 1000, height: 700 }, only: "custom" });
}
if (only !== "desktop") {
  for (const theme of ["light", "dark"]) await phoneRun({ theme, viewport: { width: 1440, height: 900 } });
  await phoneRun({ theme: "light", viewport: { width: 1000, height: 700 } });
}
