// The phones in Settings → 设备 (what 手机连接 showed before): a phone from a host that predates the model report, live
// model and platform, reload, cancelling a removal, a refused removal, a removal, and a list read that arrives after the
// removal and must not bring the phone back. Needs a Vite dev server (CAGE_UI_URL). Screenshots: CAGE_TEST_CAPTURE.
import assert from "node:assert/strict";
import { join } from "node:path";
import { smokeCaptureDirectory } from "./smoke-captures.mjs";
import { createDevicesHost, devicesRoutes } from "./devices-fixture.mjs";
import { createWorkbench } from "./workbench-fixture.mjs";

const output = await smokeCaptureDirectory("mobile-pair-settings");
const host = createDevicesHost({ devices: [{ id: "p1", kind: "phone", platform: "ios", name: "", last_seen: "" }], keepAwake: { active: true } });
const { browser, page, errors } = await createWorkbench({
  beforeNavigate: async (page) => { await devicesRoutes(page, host); },
});
async function open() {
  await page.getByRole("button", { name: "设置", exact: true }).click();
  await page.getByRole("navigation", { name: "设置分组" }).getByRole("button", { name: "设备", exact: true }).click();
  await page.getByRole("heading", { name: "可以连接这台电脑的设备", exact: true }).waitFor();
}
const row = (text) => page.locator(".devices-row", { hasText: text });
async function confirmRemoval(name) {
  await page.getByRole("button", { name: "移除 " + name, exact: true }).click();
  await page.getByRole("dialog").last().waitFor();
}
try {
  await open();
  await page.getByText("有设备配对时，这台电脑保持运行，屏幕可以熄灭。移除全部设备后恢复原来的休眠设置。", { exact: true }).waitFor();
  host.keepAwake = { active: false, error: "测试：系统未允许防休眠" };
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await page.getByRole("alert").filter({ hasText: "主机防休眠未生效：测试：系统未允许防休眠" }).waitFor();
  host.keepAwake = { active: true };
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await page.getByRole("alert").filter({ hasText: "主机防休眠未生效" }).waitFor({ state: "hidden" });
  // A host that predates the report: a phone without a model.
  await row("型号待获取").getByText("手机 · iOS · 还没有连接过", { exact: true }).waitFor();
  // The model arrives.
  host.devices = [{ id: "p1", kind: "phone", platform: "android", name: "Google Pixel 9 Pro", last_seen: new Date().toISOString(), connected: true }];
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await row("Google Pixel 9 Pro").getByText("手机 · Android · 已连接", { exact: true }).waitFor();
  await page.screenshot({ path: join(output, "android-model.png") });
  await page.reload();
  await open();
  await row("Google Pixel 9 Pro").waitFor();
  await confirmRemoval("Google Pixel 9 Pro");
  await page.getByText("移除 Google Pixel 9 Pro 后，它要重新配对才能访问这台电脑。正在进行的操作会立即中断。", { exact: true }).waitFor();
  await page.getByRole("button", { name: "取消", exact: true }).click();
  assert.equal(host.count(/^DELETE \/devices\/p1$/), 0);

  host.devices = [{ id: "p1", kind: "phone", platform: "ios", name: "iPhone 16 Pro", last_seen: new Date().toISOString() }];
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await row("iPhone 16 Pro").getByText(/^手机 · iOS · 上次连接 /).waitFor();
  await page.screenshot({ path: join(output, "iphone-model.png") });
  host.failNext["DELETE /devices/p1"] = { status: 500, body: "移除失败，请重试" };
  await confirmRemoval("iPhone 16 Pro");
  await page.getByRole("dialog").last().getByRole("button", { name: "移除", exact: true }).click();
  await row("iPhone 16 Pro").getByText("移除失败，请重试", { exact: true }).waitFor();
  assert.equal(host.devices.length, 1);
  // A list read that is asked before the removal and answered after it must not bring the phone back.
  const release = host.holdNextList();
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  for (let i = 0; i < 100 && !host.heldList; i++) await page.waitForTimeout(20);
  assert.equal(host.heldList, true, "the list read is waiting");
  await confirmRemoval("iPhone 16 Pro");
  await page.getByRole("dialog").last().getByRole("button", { name: "移除", exact: true }).click();
  await page.getByRole("heading", { name: "还没有设备", exact: true }).waitFor();
  release();
  await page.waitForTimeout(500);
  assert.equal(await page.locator(".devices-row").count(), 0, "the late list does not bring the phone back");
  assert.equal(host.count(/^DELETE \/devices\/p1$/), 2);
  assert.deepEqual(errors, []);
  console.log("Phones in 设备: model fallback, live Android/iOS metadata, keep-awake line and failure, reload, cancel, refused removal, removal and a late list read passed.");
} finally { await browser.close(); }
