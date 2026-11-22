import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

// Codex account panel, terminal menu entry and terminal account labels.
const now = Math.floor(Date.now() / 1000);
for (const system of ["debian-13", "macos"]) {
  const linux = system === "debian-13";
  const accounts = [
    { id: "a".repeat(24), label: "工作账号", email: "work@example.invalid", plan: "pro", enabled: true, status: "ready",
      quota: { primary: { used_percentage: 42, resets_at: now + 3600, window_minutes: 300, observed_at: now - 30 },
        secondary: { used_percentage: 7, resets_at: now + 86400 * 3, window_minutes: 10080, observed_at: now - 30 }, observed_at: now - 30 },
      usage_status: "reported", usage_checked_at: now - 30 },
    { id: "b".repeat(24), label: "备用账号", email: "spare@example.invalid", enabled: true, status: "signed_out" },
  ];
  const posts = [], starts = [];
  const { page, state, browser, errors } = await createWorkbench({
    beforeNavigate: async (page, state) => {
      state.account.guest_system = state.status.guest_system = system;
      state.status.ready = state.status.vm_running = true;
      // Codex surfaces follow presence on a Linux guest: this environment has Codex.
      if (linux) state.status.clients = ["codex"];
      await page.route("**/api/codex-accounts*", async (route) => {
        const body = route.request().postDataJSON();
        if (body) {
          posts.push(body);
          assert.ok(["update", "scheduling", "refresh", "usage_panel", "remove"].includes(body.action), body.action);
          if (body.action === "update") Object.assign(accounts.find((a) => a.id === body.id), body.label !== undefined ? { label: body.label } : { enabled: body.enabled });
          if (body.action === "remove") accounts.splice(accounts.findIndex((a) => a.id === body.id), 1);
        }
        return route.fulfill({ json: { accounts, scheduling: { mode: "balanced", switch_policy: "ask" }, guest_supported: linux,
          detection_status: linux ? "ready" : "unsupported",
          detected: linux ? { identity: "f".repeat(64), email: "default@example.invalid", plan: "plus", checked_at: now } : undefined } });
      });
      await page.route("**/api/terminals", async (route) => {
        const body = route.request().postDataJSON();
        if (!body) return route.fulfill({ json: state.terminals });
        starts.push(body);
        const terminal = { id: "t" + starts.length, title: body.kind === "codex-login" ? "Codex 账号登录" : "Codex " + starts.length, kind: body.kind,
          workspace: body.workspace, codex_account_id: body.kind === "codex" ? accounts[0].id : body.codex_account_id || "c".repeat(24),
          codex_account_source: "managed" };
        state.terminals.push(terminal);
        return route.fulfill({ json: terminal });
      });
    },
  });
  try {
    const nav = page.getByRole("navigation", { name: "主导航" });
    await nav.getByRole("button", { name: "Codex 账号管理", exact: true }).click();
    const panel = page.getByRole("main", { name: "Codex 账号管理", exact: true });
    await panel.waitFor();
    if (!linux) {
      await panel.getByText(/Codex 账号目前只支持 Linux 环境/).waitFor();
      assert.ok(await panel.getByRole("button", { name: "登录新账号" }).isDisabled());
      await nav.getByRole("button", { name: "工作台", exact: true }).click();
      await page.getByRole("button", { name: /^在 .* 新建终端$/ }).click();
      await page.getByRole("menuitem", { name: /Claude 终端$/ }).waitFor();
      assert.equal(await page.getByRole("menuitem", { name: /Codex 终端$/ }).count(), 0, "Codex terminal offered on a native guest");
      assert.deepEqual(errors, []);
      continue;
    }
    const work = panel.getByRole("article", { name: "工作账号", exact: true });
    await work.getByRole("group", { name: "5h 额度" }).getByRole("progressbar").waitFor();
    assert.equal(await work.getByRole("group", { name: "5h 额度" }).getByRole("progressbar").getAttribute("value"), "42");
    await work.getByRole("group", { name: "7d 额度" }).waitFor();
    await work.getByText("pro", { exact: true }).waitFor();
    await panel.getByRole("article", { name: "default@example.invalid" }).getByText("环境默认登录（~/.codex）").waitFor();
    const spare = panel.getByRole("article", { name: "备用账号", exact: true });
    await spare.getByText("需要登录").waitFor();
    await spare.getByRole("button", { name: "打开登录终端" }).waitFor();
    await work.getByRole("checkbox", { name: "参与自动分配" }).click();
    await work.getByText("已暂停").waitFor();
    assert.deepEqual(posts.find((p) => p.action === "update"), { action: "update", id: accounts[0].id, enabled: false });
    await work.getByRole("checkbox", { name: "参与自动分配" }).click();
    await work.getByText("参与分配").waitFor();
    await spare.getByRole("button", { name: "移除账号" }).click();
    const confirm = page.getByRole("dialog", { name: "移除「备用账号」？" });
    await confirm.getByText(/登录资料和会话记录会保留/).waitFor();
    await confirm.getByRole("button", { name: "移除账号", exact: true }).click();
    await spare.waitFor({ state: "detached" });
    assert.equal(accounts.length, 1);
    await panel.getByRole("button", { name: "登录新账号" }).click();
    await page.getByRole("tab", { name: /Codex 账号登录/ }).waitFor();
    assert.deepEqual(starts.at(-1), { workspace: starts.at(-1).workspace, kind: "codex-login" });
    await page.getByText("Codex 登录 · 新账号登录").first().waitFor();
    await page.getByRole("button", { name: /^在 .* 新建终端$/ }).click();
    await page.getByRole("menuitem", { name: /Codex 终端$/ }).press("Enter");
    await page.getByRole("tab", { name: /Codex 2.*Codex · 工作账号/ }).waitFor();
    assert.equal(starts.at(-1).kind, "codex");
    assert.equal(starts.at(-1).codex_account_id, undefined);
    await page.getByText("Codex · 工作账号").first().waitFor();
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
}
console.log("Codex account panel, menu and terminal labels passed");
