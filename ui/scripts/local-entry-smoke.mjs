import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { createWorkbench } from "./workbench-fixture.mjs";

// The "本机终端" settings page (design 6.2) against a mocked /api/entry:
// lazy loading, the Include dialog and both of its choices, copy, devices
// (rename, revoke), every state line, load errors and turning it off; both
// themes at 1440×900 and 1000×700. Run against a dev server:
//   CAGE_UI_URL=http://127.0.0.1:5242 node scripts/local-entry-smoke.mjs
const output = new URL("../../output/local-entry/", import.meta.url);
await fs.mkdir(output, { recursive: true });
const now = Date.now();
const ago = (minutes) => new Date(now - minutes * 60000).toISOString();
const fingerprint = "SHA256:o/+qFR975llb1cjqdgUG0z2vIkIUeG0n2P6A+7yb6/A";
const bomWarning = "~/.ssh/config 以 BOM（字节顺序标记）开头，多数 OpenSSH 会把带 BOM 的第一行读成未知选项而报错；Super Cage 的这一行会写在 BOM 之后。建议先把文件另存为不带 BOM 的 UTF-8。";
const hostBlock = "# Super Cage 本机终端 — generated, do not edit. Environment: 默认环境\nHost cage-default\n  HostName cage-default.invalid\n  User cage\n  ProxyCommand '/Applications/Super Cage.app/Contents/MacOS/cage-attach' entry proxy default\n  HostKeyAlias cage-default\n  StrictHostKeyChecking yes\n  IdentitiesOnly yes\n  ControlMaster no\n  ControlPath none\n";

function entryState() {
  return {
    variant: "ok",
    includePresent: false,
    view: {
      enabled: false, supported: true, environment: "default", name: "默认环境", state: "off",
      connections: { total: 0, shell: 0, exec: 0, sftp: 0 }, devices: [],
      revoked: false, kept: [], kept_limit: 4,
    },
    bom: false,
    unreached: [],
    requests: [],
  };
}

function turnedOn(entry) {
  const view = entry.view;
  view.enabled = true;
  view.state = "idle";
  view.host_alias = "cage-default";
  view.commands = { ssh: "ssh cage-default", cage_ssh: "cage ssh default", attach: "cage attach", cage_on_path: true };
  view.include = { line: "Include ~/.ssh/super-cage/config", present: entry.includePresent, display_path: "~/.ssh/config" };
  if (!view.devices.some((device) => device.current)) view.devices.push({ id: "a".repeat(32), name: "shihao 的 MacBook Pro", fingerprint, created: ago(1), current: true, revoked: false });
  view.devices = view.devices.map((device) => (device.current ? { ...device, revoked: false } : device));
  view.revoked = false;
}

async function routeEntry(page, entry) {
  await page.route("**/api/entry**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname.replace("/api", "");
    const method = request.method();
    const body = method === "POST" ? request.postDataJSON() : undefined;
    entry.requests.push({ method, path, body });
    if (entry.variant === "outdated") return route.fulfill({ status: 404, contentType: "text/plain", body: "404 page not found" });
    if (entry.variant === "remote") return route.fulfill({ status: 403, json: { status: 403, code: "local_only", message: "本机终端只能在这台电脑上开启和连接" } });
    const view = entry.view;
    if (path === "/entry" && method === "GET") return route.fulfill({ json: view });
    if (path === "/entry/include" && method === "GET") return route.fulfill({ json: { path: "/Users/shihao/.ssh/config", display_path: "~/.ssh/config", line: "Include ~/.ssh/super-cage/config", present: entry.includePresent, token: entry.includePresent ? "" : "token-1", alias: "cage-default", block: hostBlock, warning: entry.bom ? bomWarning : undefined } });
    if (path === "/entry/include") {
      if (body.token !== "token-1") return route.fulfill({ status: 409, contentType: "text/plain", body: "~/.ssh/config 在确认后又有变化：请重新确认" });
      entry.includePresent = true;
      if (view.include) view.include.present = true;
      return route.fulfill({ json: { present: true } });
    }
    if (path === "/entry/on") {
      turnedOn(entry);
      return route.fulfill({ json: view });
    }
    if (path === "/entry/off") {
      Object.assign(view, { enabled: false, state: "off", commands: undefined, include: undefined, host_alias: undefined });
      return route.fulfill({ json: view });
    }
    const device = path.match(/^\/entry\/devices\/([^/]+)\/(revoke|rename)$/);
    if (device) {
      const record = view.devices.find((item) => item.id === device[1]);
      if (device[2] === "revoke") record.revoked = true;
      else record.name = body.name;
      if (record.current && record.revoked) view.revoked = true;
      return route.fulfill({ json: device[2] === "revoke" ? { ...view, unreached: entry.unreached } : view });
    }
    return route.fulfill({ status: 404, body: "" });
  });
  // 结束 on a kept terminal is the window's own DELETE /api/terminals/<id>.
  await page.route("**/api/terminals/kept-*", async (route) => {
    const request = route.request();
    const id = new URL(request.url()).pathname.split("/").at(-1);
    entry.requests.push({ method: request.method(), path: "/terminals/" + id });
    entry.view.kept = entry.view.kept.filter((terminal) => terminal.id !== id);
    return route.fulfill({ json: { ok: true } });
  });
}

const populated = (entry) => {
  // Relative to now, not to the start of the run: "3 分钟前" must stay true.
  const ago = (minutes) => new Date(Date.now() - minutes * 60000).toISOString();
  turnedOn(entry);
  entry.view.state = "in_use";
  entry.view.connections = { total: 2, shell: 1, exec: 1, sftp: 0 };
  entry.view.include.present = true;
  entry.includePresent = true;
  entry.view.devices = [
    { id: "a".repeat(32), name: "shihao 的 MacBook Pro", fingerprint, created: ago(9000), last_used: ago(3), current: true, revoked: false },
    { id: "b".repeat(32), name: "林间工作室的 Mac Studio（客厅，接 5K 显示器与外置键盘，周末备份用）", fingerprint: "SHA256:Zz9yXw8vUu7tTs6rRq5pPo4nNm3lLk2jJi1hHg0fFe9", created: ago(30000), last_used: ago(60 * 26), revoked: false },
    { id: "c".repeat(32), name: "旧笔记本", fingerprint: "SHA256:Aa1bBc2cCd3dDe4eEf5fFg6gGh7hHi8iIj9jJk0kKl1", created: ago(90000), revoked: true },
  ];
  entry.view.kept = [
    { id: "kept-1", title: "终端 3", device: "shihao 的 MacBook Pro", ended: false },
    { id: "kept-2", title: "终端 5", device: "林间工作室的 Mac Studio（客厅，接 5K 显示器与外置键盘，周末备份用）", ended: true },
  ];
};

for (const [width, height] of [[1440, 900], [1000, 700]]) {
  for (const scheme of ["light", "dark"]) {
    const entry = entryState();
    const chunks = [];
    let clipboard = "";
    const fixture = await createWorkbench({ viewport: { width, height }, nativeRequest: async (body) => {
      if (body.action === "clipboardWrite") clipboard = body.text;
      return null;
    }, beforeNavigate: async (page) => {
      await page.emulateMedia({ colorScheme: scheme });
      page.on("request", (request) => { if (/\/src\/local-entry[.-]|\/assets\/local-entry-/.test(new URL(request.url()).pathname)) chunks.push(request.url()); });
      await routeEntry(page, entry);
    } });
    const { page, browser, errors, context } = fixture;
    const shot = async (name) => {
      await page.waitForTimeout(300); // dialogs fade in over 180 ms
      return page.screenshot({ path: fileURLToPath(new URL(`${width}x${height}-${scheme}-${name}.png`, output)) });
    };
    try {
      await context.grantPermissions(["clipboard-read", "clipboard-write"]);
      await page.getByRole("button", { name: "设置", exact: true }).click();
      const nav = page.getByRole("navigation", { name: "设置分组" });
      await nav.getByRole("button", { name: "设备", exact: true }).click();
      assert.deepEqual(chunks, [], "the page must load only when its section opens");
      assert.equal(entry.requests.length, 0);
      await nav.getByRole("button", { name: "本机终端", exact: true }).click();
      const content = page.locator(".settings-content");
      // Off: the empty state with one clay button.
      await content.getByRole("heading", { name: "在你自己的终端里使用这个环境" }).waitFor();
      assert.ok(chunks.length > 0, "the page chunk loads on demand");
      assert.equal(await content.locator("button.clay").count(), 1);
      assert.deepEqual(entry.requests.map((r) => r.method + " " + r.path), ["GET /entry"]);
      await content.getByRole("region", { name: "从你自己的终端连进来", exact: true }).waitFor();
      assert.equal(await content.getByText(/Herdr/).count(), 0);
      await shot("off");
      // The dialog shows the exact line; 取消 writes nothing.
      await content.getByRole("button", { name: "开启本机终端" }).click();
      const dialog = page.getByRole("dialog", { name: "让 ssh 认识这个环境" });
      await dialog.getByText("Include ~/.ssh/super-cage/config", { exact: true }).waitFor();
      // The host name and the whole block the line brings in (F6).
      await dialog.getByText("之后用 ssh cage-default 连接。这一行引入的配置：").waitFor();
      assert.equal(await dialog.locator("pre.local-entry-include-block").textContent(), hostBlock);
      await shot("dialog");
      await dialog.getByRole("button", { name: "取消", exact: true }).click();
      assert.equal(entry.requests.filter((r) => r.method === "POST").length, 0, "取消 must not write");
      // 不写入，只用 cage ssh.
      await content.getByRole("button", { name: "开启本机终端" }).click();
      await dialog.getByRole("button", { name: "不写入，只用 cage ssh", exact: true }).click();
      await content.getByText("已开启 · 没有连接").waitFor();
      await content.getByText("cage ssh default", { exact: true }).waitFor();
      assert.equal(entry.requests.some((r) => r.method === "POST" && r.path === "/entry/include"), false);
      await content.getByText(/还没有包含 Super Cage 的配置/).waitFor();
      // What is filtered and what is not is said on the page, exactly:
      // sessions with a terminal through cage attach's filter, the rest not.
      await content.getByText(/scp、sftp 和 rsync 原样传送环境的字节，不经过滤/).waitFor();
      await content.getByText(/带终端的会话（ssh、ssh -t）是一个 Super Cage 终端：环境的输出经过和 cage attach 一样的过滤/).waitFor();
      assert.equal(await content.getByText(/会被拒绝/).count(), 0);
      // cage attach has its row after ssh's: two rows, each with its copy button.
      await content.getByText("cage attach", { exact: true }).waitFor();
      const rows = content.locator(".local-entry-commands li");
      assert.equal(await rows.count(), 2);
      assert.equal(await rows.getByRole("button").count(), 2);
      assert.equal(await page.getByText(/Herdr|稍后提供/).count(), 0, "nothing on the page names another application");
      await shot("on-without-include");
      // Writing it later, from the row; a BOM is warned about first.
      entry.bom = true;
      await content.getByRole("button", { name: "写入 ~/.ssh/config" }).click();
      await page.getByRole("dialog", { name: "让 ssh 认识这个环境" }).getByText(bomWarning, { exact: true }).waitFor();
      await shot("dialog-bom");
      await page.getByRole("dialog", { name: "让 ssh 认识这个环境" }).getByRole("button", { name: "写入", exact: true }).click();
      await content.getByText("ssh cage-default", { exact: true }).waitFor();
      assert.deepEqual(entry.requests.find((r) => r.path === "/entry/include" && r.method === "POST").body, { token: "token-1" });
      // Copy.
      await content.getByRole("button", { name: "复制 ssh cage-default" }).click();
      await content.getByRole("button", { name: "已复制" }).waitFor();
      assert.equal(clipboard || (await page.evaluate(() => navigator.clipboard.readText())), "ssh cage-default");
      await shot("on");
      // Populated: two connections, long names, a revoked device.
      populated(entry);
      await nav.getByRole("button", { name: "设备", exact: true }).click();
      await nav.getByRole("button", { name: "本机终端", exact: true }).click();
      await content.getByText("已开启 · 2 个连接").waitFor();
      await content.getByText("最近使用 3 分钟前").waitFor();
      await content.getByText("SHA256:o/+qFR…b6/A").waitFor();
      await content.getByText("已撤销", { exact: true }).waitFor();
      assert.equal(await content.getByRole("button", { name: "撤销", exact: true }).count(), 2);
      assert.equal(await page.evaluate(() => document.querySelector(".settings-content").scrollWidth > document.querySelector(".settings-content").clientWidth), false, "no horizontal overflow");
      await shot("populated");
      // Rename.
      await content.getByRole("button", { name: "重命名 shihao 的 MacBook Pro" }).click();
      const input = content.getByRole("textbox", { name: "设备名称" });
      await input.fill("工作本");
      await input.press("Enter");
      await content.getByText("工作本", { exact: true }).waitFor();
      assert.deepEqual(entry.requests.at(-1), { method: "POST", path: "/entry/devices/" + "a".repeat(32) + "/rename", body: { name: "工作本" } });
      // Revoke another device: a destructive confirm.
      const studio = content.getByRole("region", { name: "设备" }).locator(".local-entry-device").filter({ hasText: "林间工作室" });
      await studio.getByRole("button", { name: "撤销", exact: true }).click();
      const revoke = page.getByRole("dialog", { name: /^撤销「林间工作室/ });
      await revoke.getByText("这台电脑的所有连接会立即断开，之后需要重新开启才能连接。").waitFor();
      assert.equal(await revoke.locator("button.danger-fill").count(), 1);
      await shot("revoke-confirm");
      // Another environment did not answer in time: the page says so.
      entry.unreached = ["研究机"];
      await revoke.getByRole("button", { name: "撤销", exact: true }).click();
      await studio.getByText("已撤销").waitFor();
      await content.getByRole("status").filter({ hasText: "另一个环境暂时没有响应，已撤销的钥匙会在它下次连接时被拒绝（研究机）" }).waitFor();
      await shot("revoke-unreached");
      // Kept terminals: listed with their limit, each with 结束.
      const kept = content.getByRole("region", { name: "保留的终端" });
      await kept.getByText(/最多 4 个/).waitFor();
      await kept.getByText("来自 林间工作室的 Mac Studio（客厅，接 5K 显示器与外置键盘，周末备份用） · 已结束").waitFor();
      assert.equal(await page.evaluate(() => document.querySelector(".settings-content").scrollWidth > document.querySelector(".settings-content").clientWidth), false, "no horizontal overflow with kept terminals");
      await shot("kept");
      await kept.locator(".local-entry-device").filter({ hasText: "终端 3" }).getByRole("button", { name: "结束", exact: true }).click();
      const end = page.getByRole("dialog", { name: "结束「终端 3」？" });
      await end.getByText("终端里还在运行的程序会一起结束。").waitFor();
      await end.getByRole("button", { name: "结束终端", exact: true }).click();
      await kept.getByText("终端 3", { exact: true }).waitFor({ state: "detached" });
      assert.ok(entry.requests.some((r) => r.method === "DELETE" && r.path === "/terminals/kept-1"));
      // State lines.
      for (const [state, text] of [["paused", "网络已暂停 · ssh 已断开，恢复网络后可重新连接"], ["stopped", "环境已停止 · 启动环境后可连接"], ["starting", "环境正在启动 · 启动完成后可连接"]]) {
        entry.view.state = state;
        await nav.getByRole("button", { name: "设备", exact: true }).click();
        await nav.getByRole("button", { name: "本机终端", exact: true }).click();
        await content.getByText(text).waitFor();
      }
      await shot("paused-or-starting");
      // Off: confirm, then the empty state again.
      entry.view.state = "idle";
      await nav.getByRole("button", { name: "设备", exact: true }).click();
      await nav.getByRole("button", { name: "本机终端", exact: true }).click();
      await content.getByRole("button", { name: "关闭本机终端" }).click();
      const off = page.getByRole("dialog", { name: "关闭本机终端？" });
      await off.getByText("所有本机连接会立即断开；环境里的终端继续运行。").waitFor();
      await off.getByRole("button", { name: "关闭本机终端", exact: true }).click();
      await content.getByRole("heading", { name: "在你自己的终端里使用这个环境" }).waitFor();
      // A controller without the entry, and a window on another computer.
      for (const [variant, text] of [["outdated", "需要更新 Super Cage：正在运行的版本还没有本机终端。"], ["remote", "本机终端只能在这台电脑上的 Super Cage 里管理。"]]) {
        entry.variant = variant;
        await nav.getByRole("button", { name: "设备", exact: true }).click();
        await nav.getByRole("button", { name: "本机终端", exact: true }).click();
        await content.getByRole("alert").filter({ hasText: text }).waitFor();
      }
      await shot("outdated-or-remote");
      assert.deepEqual(errors, []);
      console.log(`PASS: ${width}x${height} ${scheme}: lazy load, include dialog (cancel, skip, write, BOM warning), copy, devices, revoke (unreached), kept terminals (end), filter note, state lines, off, errors`);
    } finally {
      await browser.close();
    }
  }
}

// A remote client never shows the section.
{
  const fixture = await createWorkbench({ clientContext: { mode: "remote", connection_id: "remote-test", instance_id: "x", client_platform: "darwin", server_platform: "linux", connection: { state: "connected" } }, clientRequest: (route, path) => route.fulfill({ json: path === "/client/context" ? { mode: "remote", connection_id: "remote-test", instance_id: "x", client_platform: "darwin", server_platform: "linux", connection: { state: "connected" } } : {} }) });
  try {
    await fixture.page.getByRole("button", { name: "设置", exact: true }).click();
    const nav = fixture.page.getByRole("navigation", { name: "设置分组" });
    await nav.getByRole("button", { name: "关于", exact: true }).waitFor();
    assert.equal(await nav.getByRole("button", { name: "本机终端", exact: true }).count(), 0);
    console.log("PASS: a remote window has no 本机终端 section");
  } finally {
    await fixture.browser.close();
  }
}
