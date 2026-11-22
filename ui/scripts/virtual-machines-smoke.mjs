// Exercise the real sidebar with a fixture for the native gateway.
import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

const endpoint = "/client/virtual-machines";
const vms = [{ id: "default", name: "默认环境", state: "ready" }];
let generation = "test-generation-1";
let current = "default";
let failList = true;
let fixtureState;
const { browser, page, errors } = await createWorkbench({
  async beforeNavigate(page, state) {
    fixtureState = state;
    await page.route("**/api/environment/guest-systems", route => route.fulfill({ json: { fresh: true, system: "macos", available: true } }));
    await page.route("**/api/environment/storage", route => route.fulfill({ json: { directory: "/preview/virtual-machine/state", has_environment: false } }));
  },
  async clientRequest(route, path, body) {
    if (path === "/client/context") {
      return route.fulfill({ json: {
        mode: "local", desktop_gateway: true, virtual_machines: true,
        connection_id: generation, client_platform: "darwin", server_platform: "darwin",
        connection: { state: "connected", connection_id: generation },
      } });
    }
    if (path === "/client/hosts") return route.fulfill({ json: { hosts: [] } });
    if (path === "/client/local/status") return route.fulfill({ json: { known: true } });
    // This computer's own terminal settings and entry do not belong to an environment connection.
    if (path.startsWith("/client/local-terminal") || path.startsWith("/client/local-entry")) return route.fulfill({ json: {} });
    assert.equal(route.request().headers()["x-cage-connection"], generation);
    if (path === endpoint && route.request().method() === "GET") {
      if (failList) {
        failList = false;
        return route.fulfill({ status: 503, body: "请重新读取虚拟机列表" });
      }
      return route.fulfill({ json: { vms, current_id: current } });
    }
    if (path === endpoint) {
      assert.deepEqual(body, { name: "开发环境" });
      const vm = { id: "1234abcd", name: body.name, state: "stopped" };
      vms.push(vm);
      return route.fulfill({ json: { vm } });
    }
    assert.deepEqual(body, {});
    if (current === "default") {
      assert.equal(path, `${endpoint}/1234abcd/open`);
      current = "1234abcd";
      generation = "test-generation-2";
      fixtureState.initialization = { required: true, tracked: true, started: false };
      Object.assign(fixtureState.account, { local_vm: true, trusted_local: true, macos_account_host: true, macos_credentials_ready: false });
    } else {
      assert.equal(path, `${endpoint}/default/open`);
      current = "default";
      generation = "test-generation-3";
      fixtureState.initialization = { required: false };
      Object.assign(fixtureState.account, { local_vm: false, trusted_local: false, macos_credentials_ready: true });
    }
    return route.fulfill({ json: { current_id: current } });
  },
});
try {
  const button = page.getByRole("button", { name: "虚拟机", exact: true });
  const dialog = page.getByRole("dialog", { name: "虚拟机" });
  await button.click();
  await dialog.getByRole("button", { name: "重新读取", exact: true }).click();
  assert.equal(await dialog.getByRole("button", { name: /默认环境.*当前虚拟机/ }).isDisabled(), true);
  assert.equal(await dialog.locator("input").count(), 1);
  const input = dialog.getByRole("textbox", { name: "虚拟机名称" });
  await input.fill("  开发环境  ");
  await input.press("Enter");
  await dialog.getByRole("button", { name: "开发环境", exact: true }).click();
  await dialog.waitFor({ state: "hidden" });
  const setup = page.getByRole("heading", { name: "为 Super Cage 准备独立环境", exact: true });
  await setup.waitFor();
  await button.click();
  assert.equal(await dialog.getByRole("button", { name: /开发环境.*当前虚拟机/ }).isDisabled(), true);
  await dialog.getByRole("button", { name: "默认环境", exact: true }).click();
  await setup.waitFor({ state: "hidden" });
  await page.getByRole("complementary", { name: "项目与终端" }).waitFor();
  await button.click();
  assert.equal(await dialog.getByRole("button", { name: /默认环境.*当前虚拟机/ }).isDisabled(), true);
  assert.deepEqual(errors, []);
  console.log("PASS: name-only creation, generation binding, and round trip through first-run setup on a Mac host");
} catch (error) {
  console.error(await page.locator("body").innerText(), errors);
  throw error;
} finally {
  await browser.close();
}
