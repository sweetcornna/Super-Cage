import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";
import { resourceAllocations } from "./module-pages-fixture.mjs";

// 计算资源 with automatic allocation: the legacy notice's one action and its
// quiet alternative, the advice action, manual sliders and the restart flow.
const now = Date.now();
let answer = resourceAllocations(now).legacy;
const posts = [];
const { browser, page, state, errors } = await createWorkbench({
  beforeNavigate: async (page, state) => {
    state.status.ready = state.status.vm_running = true;
    await page.route("**/api/environment/resources", async (route) => {
      if (route.request().method() === "POST") {
        const body = route.request().postDataJSON();
        posts.push(body);
        // As the server does: a saved allocation is automatic or the user's choice.
        if (body.allocation) answer = body.allocation.automatic ? resourceAllocations(now).automatic
          : { ...answer, ...body.allocation, allocation_origin: "user", legacy_allocation: undefined, notice: undefined, offer: undefined };
        return route.fulfill({ json: { ok: true } });
      }
      answer.observation &&= { ...answer.observation, observed_at: new Date().toISOString() };
      return route.fulfill({ json: answer });
    });
  },
});
async function open() {
  if (!(await page.getByRole("region", { name: "设置", exact: true }).count()))
    await page.getByRole("button", { name: "设置", exact: true }).click();
  await page.getByRole("navigation", { name: "设置分组" }).getByRole("button", { name: "环境与恢复", exact: true }).click();
  return page.getByRole("region", { name: "计算资源", exact: true });
}
try {
  let panel = await open();
  await panel.getByText(/与旧版本的默认设置相同/).waitFor();
  assert.equal(await panel.getByRole("radio", { name: /自动（推荐）.*内存 6 GB · CPU 5 核/ }).isChecked(), false);
  assert.equal(await panel.getByRole("radio", { name: /手动/ }).isChecked(), true);
  // The offer asks first, since a running environment restarts.
  await panel.getByRole("button", { name: "改为自动", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "改为自动分配并重启环境？", exact: true });
  await dialog.getByRole("button", { name: "取消", exact: true }).click();
  assert.equal(posts.length, 0, "cancelled: nothing sent");
  await panel.getByRole("button", { name: "改为自动", exact: true }).click();
  await dialog.getByRole("button", { name: "改为自动并重启", exact: true }).click();
  await panel.getByRole("button", { name: "改为自动", exact: true }).waitFor({ state: "detached" });
  assert.deepEqual(posts.at(-1), { allocation: { automatic: true }, apply: true });
  assert.equal(await panel.getByRole("radio", { name: /自动（推荐）/ }).isChecked(), true);

  // The quiet alternative saves the same values as the user's own choice, no restart.
  answer = resourceAllocations(now).legacy;
  await page.reload();
  panel = await open();
  await panel.getByRole("button", { name: "保留当前设置", exact: true }).click();
  await panel.getByText(/与旧版本的默认设置相同/).waitFor({ state: "detached" });
  assert.deepEqual(posts.at(-1), { allocation: { automatic: false, memory_gib: 3, cpus: 3 } });
  assert.equal(await page.getByRole("dialog").count(), 0, "keeping never restarts");

  // Manual and short: the advice is shown in place with its one action.
  answer = resourceAllocations(now).advice;
  await page.reload();
  panel = await open();
  await panel.getByText("环境内存已用满手动设置的 3 GB。改为自动（推荐）后可以调到 6 GB，需要重启环境。", { exact: true }).waitFor();
  await panel.getByText("内存紧张 · 已到上限", { exact: true }).waitFor();
  assert.equal(await panel.getByLabel("内存上限（GiB）", { exact: true }).getAttribute("max"), "6");
  await panel.getByLabel("内存上限（GiB）", { exact: true }).fill("5");
  await panel.getByRole("button", { name: "保存资源配置", exact: true }).click();
  await panel.getByText("已保存，重启环境后生效。", { exact: true }).waitFor();
  assert.deepEqual(posts.at(-1), { allocation: { automatic: false, memory_gib: 5, cpus: 2 } });

  // Automatic recalculated: the notice carries the existing restart flow.
  answer = resourceAllocations(now).restart;
  await page.reload();
  panel = await open();
  await panel.getByText(/自动分配已按这台机器重新计算/).waitFor();
  assert.equal(await panel.getByRole("button", { name: "重启并应用已保存配置", exact: true }).count(), 1);
  await panel.getByRole("button", { name: "重启并应用已保存配置", exact: true }).click();
  const before = posts.length;
  await page.getByRole("dialog", { name: "重启并应用已保存的资源配置？", exact: true }).getByRole("button", { name: "重启并应用", exact: true }).click();
  for (let i = 0; i < 50 && posts.length === before; i++) await page.waitForTimeout(100);
  assert.deepEqual(posts.at(-1), {});
  assert.deepEqual(errors, []);
  console.log("Resource allocation: legacy keep and offer with confirmation, advice in place, manual range, restart notice passed.");
} finally { await browser.close(); }
