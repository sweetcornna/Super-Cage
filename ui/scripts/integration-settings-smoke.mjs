// Run with CAGE_UI_URL pointing to Vite. The browser only uses in-memory API fixtures.
import assert from "node:assert/strict";
import { chooseInline, createWorkbench } from "./workbench-fixture.mjs";

const context = {
  mode: "local", connection_id: "fixture-a", connection: { state: "connected" },
  desktop_gateway: true, client_platform: "darwin", server_platform: "darwin",
};
const orca = {
  enabled: false, supports_managed: true, server_name: "Fixture Orca", guest_port: 16768,
  host_tunnel_port: 0, tunnel_port: 0, bind_mode: "loopback", auto_sync_projects: true,
  terminal_memory_max: "", account_bindings: { claude: "auto" }, codex_registration_supported: true,
};
const t3 = {
  enabled: false, supports_managed: true, direct_available: false, mode: "managed", port: 13773,
  external_url: "", open_mode: "workbench", focus_chrome: true, account_bindings: { claude: "auto" },
};
const orcaWindows = { ...orca, supports_managed: false, server_name: "Windows Orca" };
const t3Windows = { ...t3, supports_managed: false };
const magpie = {
  enabled: false, supports_managed: true, direct_available: false, mode: "managed",
  port: 3430, gateway_port: 3425, gateway_only: false, external_url: "",
  open_mode: "workbench", focus_chrome: true,
};
const herdr = { enabled: false, supports_managed: true, takeover_terminals: true };
const agent = {
  spec: {
    id: "fixture-cli", name: "Fixture CLI", binary: "fixture", built_in: true, category: "first-party",
    channels: [{ id: "npm", kind: "npm", label: "npm" }, { id: "script", kind: "script", label: "脚本" }],
    auth: { methods: ["oauth"] }, terminal: { image_paste_shortcut: true },
  },
  config: { launch_command: "fixture" }, runtime: { installed: false }, accounts: {},
};
const changes = [];
const registrations = [];
const protectedActions = [];
let failOrca = false;
let failCodexAccounts = true;
let failSettings = false;
let failAppDetection = true;
let local = { image_upload: true, clipboard_write: true };

const fixture = await createWorkbench({
  viewport: { width: 1000, height: 600 },
  clientContext: context,
  clientRequest: async (route, path, body) => {
    if (path === "/client/context") return route.fulfill({ json: context });
    if (path === "/client/local-terminal/app") return failAppDetection
      ? route.fulfill({ status: 503, body: "fixture" })
      : route.fulfill({ json: { app: { id: "ghostty", name: "Ghostty" } } });
    if (path === "/client/local-terminal") {
      if (body && failSettings) return route.fulfill({ status: 409, body: "fixture" });
      if (body) {
        changes.push({ name: "local-terminal", body });
        local = structuredClone(body);
      }
      return route.fulfill({ json: { settings: local, supported: true } });
    }
    return route.fulfill({ json: {} });
  },
  beforeNavigate: async page => {
    const read = async (route, name, first, second) => {
      const request = route.request();
      const target = request.headers()["x-cage-connection"] === "fixture-b" ? second : first;
      if (request.method() !== "POST") return route.fulfill({ json: target });
      if (name === "orca" && request.url().endsWith("/register_codex_account")) {
        registrations.push(request.postDataJSON());
        return route.fulfill({ json: { registered: true } });
      }
      if (!request.url().endsWith("/config")) {
        protectedActions.push(request.url());
        return route.fulfill({ status: 409, body: "fixture" });
      }
      const body = request.postDataJSON();
      if (name === "orca" && failOrca) return route.fulfill({ status: 503, body: "fixture" });
      if (name === "t3code" && body.external_url?.startsWith("http://outside.")) {
        return route.fulfill({ status: 400, contentType: "text/plain; charset=utf-8", body: "非本机回环地址的 T3 Code 实例必须使用 HTTPS" });
      }
      changes.push({ name, body });
      Object.assign(target, body);
      if ("terminal_memory_max" in body) target.terminal_memory_max = body.terminal_memory_max.trim().toUpperCase();
      if ("host_tunnel_port" in body || body.enabled) target.tunnel_port = target.host_tunnel_port || 16769;
      if (body.external_url) target.external_url = body.external_url.replace(/\/?$/, "/");
      return route.fulfill({ json: target });
    };
    for (const [name, first, second] of [
      ["orca", orca, orcaWindows], ["t3code", t3, t3Windows],
      ["magpie", magpie, { ...magpie }], ["herdr", herdr, { ...herdr }],
    ]) {
      await page.route(new RegExp(`/api/${name}(?:[/?]|$)`), route => read(route, name, first, second));
    }
    await page.route("**/api/claude-accounts", route => route.fulfill({ json: {
      accounts: [{ id: "fixture-claude", label: "测试账号", status: "ready", enabled: true }],
    } }));
    await page.route("**/api/codex-accounts", route => failCodexAccounts
      ? route.fulfill({ status: 503, contentType: "text/plain; charset=utf-8", body: "暂时无法读取 Codex 已登记账号" })
      : route.fulfill({ json: { guest_supported: true, accounts: [
        { id: "native-codex", label: "原生 Codex 账号", status: "ready", enabled: true },
      ] } }));
    await page.route("**/api/agent-clis/codex/accounts?*", route => route.fulfill({ json: {
      accounts: [{ id: "agent-codex", label: "Agent Codex 账号", status: "ready", enabled: true }],
    } }));
    await page.route("**/api/agent-clis/config", route => {
      const { agent_id, ...body } = route.request().postDataJSON();
      assert.equal(agent_id, agent.spec.id);
      changes.push({ name: "agent", body });
      Object.assign(agent.config, body);
      return route.fulfill({ json: { agents: [agent] } });
    });
    const agentList = route => route.fulfill({ json: { agents: [
      agent,
      { spec: { id: "claude", name: "Claude Code", binary: "claude", built_in: true, category: "first-party" }, accounts: {} },
      { spec: { id: "codex", name: "Codex", binary: "codex", built_in: true, category: "first-party" }, accounts: {} },
    ] } });
    await page.route("**/api/agent-clis", agentList);
    await page.route("**/api/agent-clis?*", agentList);
  },
});
const { page, browser, errors, state } = fixture;
page.setDefaultTimeout(8000);
const nav = page.getByRole("navigation", { name: "设置分组" });
const section = name => nav.getByRole("button", { name, exact: true });
const openSettings = () => page.getByRole("button", { name: "设置", exact: true }).click();
const back = () => page.getByRole("button", { name: "返回工作台" }).click();
const orcaCard = page.getByRole("region", { name: "Orca Server 可选服务端接入" });
const t3Card = page.getByRole("region", { name: "T3 Code 可选集成" });

try {
  await openSettings();
  await section("Orca Server").click();
  const orcaToggle = orcaCard.getByRole("switch", { name: "启用 Orca Server 服务端接入" });
  const nameInput = orcaCard.getByRole("textbox", { name: /服务器名称/ });
  const guestPort = orcaCard.getByRole("spinbutton", { name: "客机服务端口" });
  const hostPort = orcaCard.getByRole("spinbutton", { name: /本机固定隧道端口/ });
  const memory = orcaCard.getByRole("textbox", { name: /内存上限/ });
  const saveOrca = () => orcaCard.getByRole("button", { name: "保存", exact: true }).click();
  await nameInput.waitFor();
  assert.equal(await orcaToggle.isChecked(), false);
  failOrca = true;
  await orcaToggle.click();
  await orcaCard.getByRole("alert").getByText(/保存 Orca Server 设置失败/).waitFor();
  assert.equal(await orcaToggle.isChecked(), false);

  await nameInput.fill("Discarded");
  await section("通用").click();
  const dialog = page.getByRole("dialog");
  await dialog.getByText("放弃尚未保存的客户端设置？").waitFor();
  await dialog.getByRole("button", { name: "取消", exact: true }).click();
  assert.equal(await nameInput.inputValue(), "Discarded");
  await orcaCard.getByRole("button", { name: "放弃修改", exact: true }).click();
  assert.equal(await nameInput.inputValue(), "Fixture Orca");
  await guestPort.fill("1024.5");
  await saveOrca();
  await orcaCard.getByRole("alert").getByText(/客机端口须/).waitFor();
  assert.deepEqual(changes, []);
  await nameInput.fill(" Lab Orca ");
  await guestPort.fill("17040");
  await hostPort.fill("17532");
  await memory.fill("512m");
  await saveOrca();
  await orcaCard.getByRole("alert").getByText(/保存 Orca Server 设置失败/).waitFor();
  assert.equal(await nameInput.inputValue(), " Lab Orca ");

  failOrca = false;
  await saveOrca();
  await orcaCard.getByText("Orca Server 设置已保存", { exact: true }).waitFor();
  assert.equal(orca.enabled, false);
  assert.equal(orca.guest_port, 17040);
  await orcaCard.getByRole("radio", { name: /Tailscale 私有网络/ }).click();
  await orcaCard.getByText("监听模式已保存", { exact: true }).waitFor();
  await orcaCard.getByRole("switch", { name: /自动同步 Cage/ }).click();
  await orcaCard.getByText("已关闭自动同步项目", { exact: true }).waitFor();
  await chooseInline(orcaCard.getByRole("combobox", { name: "Claude Code 账号绑定" }), "fixture-claude");
  await orcaCard.getByText("账号绑定已保存", { exact: true }).waitFor();
  const orcaCodex = orcaCard.getByRole("combobox", { name: "Codex 账号绑定" });
  await orcaCard.getByText(/暂时无法读取 Codex 已登记账号/).waitFor();
  failCodexAccounts = false;
  await orcaCard.getByRole("button", { name: "重试账号列表" }).click();
  await orcaCard.getByText(/暂时无法读取 Codex 已登记账号/).waitFor({ state: "detached" });
  await chooseInline(orcaCodex, "native-codex");
  await orcaCard.getByText("账号绑定已保存", { exact: true }).waitFor();
  assert.equal(state.status.vm_running, false);
  assert.equal(orca.account_bindings.codex, "native-codex");
  await hostPort.fill("");
  await saveOrca();
  await orcaCard.getByText("Orca Server 设置已保存", { exact: true }).waitFor();
  await orcaToggle.click();
  await orcaCard.getByText("未安装", { exact: true }).waitFor();

  await back();
  await page.getByRole("button", { name: "配置 Orca Server", exact: true }).click();
  assert.equal(await nameInput.inputValue(), "Lab Orca");
  assert.equal(await guestPort.inputValue(), "17040");
  assert.equal(await hostPort.inputValue(), "");
  assert.match(await hostPort.getAttribute("placeholder"), /16769/);
  assert.equal(await memory.inputValue(), "512M");
  assert.equal(await orcaCard.getByRole("radio", { name: /Tailscale 私有网络/ }).getAttribute("aria-checked"), "true");
  assert.equal(await orcaCard.getByRole("switch", { name: /自动同步 Cage/ }).isChecked(), false);
  assert.match(await orcaCard.getByRole("combobox", { name: "Claude Code 账号绑定" }).innerText(), /测试账号/);
  assert.match(await orcaCodex.innerText(), /原生 Codex 账号/);
  Object.assign(orca, { installed: true, running: true });
  await orcaCard.getByRole("button", { name: "刷新 Orca Server 状态" }).click();
  await orcaCard.getByText("运行中", { exact: true }).waitFor();
  await orcaCard.getByRole("radio", { name: /仅本机回环隧道/ }).click();
  await dialog.getByText("保存设置并断开 Orca 连接？").waitFor();
  await dialog.getByRole("button", { name: "取消", exact: true }).click();
  assert.equal(orca.bind_mode, "tailscale");
  assert.deepEqual(protectedActions, [], "saving must not start, pair or install Orca");

  await section("T3 Code").click();
  const t3Toggle = t3Card.getByRole("switch", { name: "启用 T3 Code 集成" });
  const t3Port = t3Card.getByRole("spinbutton", { name: "客机服务端口" });
  await t3Port.fill("14771");
  await t3Card.getByRole("button", { name: "保存端口" }).click();
  await t3Card.getByText("服务端口已保存", { exact: true }).waitFor();
  await chooseInline(t3Card.getByRole("combobox", { name: /Claude Code 的 T3 Code/ }), "default");
  await t3Card.getByText("账号绑定已保存", { exact: true }).waitFor();
  await chooseInline(t3Card.getByRole("combobox", { name: "Codex 的 T3 Code 账号绑定" }), "agent-codex");
  await t3Card.getByText("账号绑定已保存", { exact: true }).waitFor();
  await chooseInline(t3Card.getByRole("combobox", { name: "T3 Code 默认打开方式" }), "direct");
  await t3Card.getByText("默认打开方式已保存", { exact: true }).waitFor();
  await t3Card.getByRole("switch", { name: /工作台极简沉浸顶栏/ }).click();
  const externalMode = t3Card.getByRole("button", { name: "接入已有实例" });
  const url = t3Card.getByRole("textbox", { name: "外部 T3 Code 地址" });
  await externalMode.click();
  await url.fill("http://outside.example.test");
  await t3Card.getByRole("button", { name: "保存外部地址" }).click();
  await t3Card.getByRole("alert").getByText(/必须使用 HTTPS/).waitFor();
  assert.equal(await url.inputValue(), "http://outside.example.test");
  await t3Card.getByRole("button", { name: "放弃修改", exact: true }).click();
  assert.equal(await url.count(), 0);
  await externalMode.click();
  await url.fill("https://t3.example.test");
  await t3Card.getByRole("button", { name: "保存外部地址" }).click();
  await t3Card.getByText("外部地址已保存", { exact: true }).waitFor();
  assert.equal(await t3Toggle.isChecked(), false);
  await section("通用").click();
  await section("T3 Code").click();
  assert.equal(await url.inputValue(), "https://t3.example.test/");
  assert.equal(t3.port, 14771);
  assert.equal(t3.focus_chrome, false);
  assert.equal(t3.account_bindings.claude, "default");
  assert.equal(t3.account_bindings.codex, "agent-codex");
  assert.match(await t3Card.getByRole("combobox", { name: "T3 Code 默认打开方式" }).innerText(), /原生极速直连窗口/);
  await t3Toggle.click();
  await t3Card.getByText("外部地址已保存并接入", { exact: true }).waitFor();
  assert.equal(t3.enabled, true);

  await section("Agent CLI").click();
  const catalogCard = page.locator(".agent-catalog-grid article").filter({ hasText: "Fixture CLI" });
  await catalogCard.getByRole("button", { name: "终端优化与启动设置" }).click();
  const command = catalogCard.getByRole("textbox", { name: "启动命令" });
  await command.fill("fixture --verbose");
  await chooseInline(catalogCard.getByRole("combobox", { name: "默认安装渠道" }), "script");
  await catalogCard.getByRole("button", { name: "保存终端设置" }).click();
  await catalogCard.getByText(/已保存，将应用于新建终端/).waitFor();
  assert.equal(agent.config.preferred_channel, "script");
  await section("通用").click();
  await section("Agent CLI").click();
  await catalogCard.getByRole("button", { name: "终端优化与启动设置" }).click();
  assert.equal(await command.inputValue(), "fixture --verbose");
  assert.equal(await catalogCard.getByRole("combobox", { name: "默认安装渠道" }).innerText(), "脚本");

  const magpieCard = page.getByRole("region", { name: "Magpie 模型路由与本地网关" });
  await magpieCard.getByRole("spinbutton", { name: "Magpie Web 端口" }).fill("4491");
  await magpieCard.getByRole("button", { name: "保存端口", exact: true }).click();
  await magpieCard.getByText("Web 服务端口已保存", { exact: true }).waitFor();
  await magpieCard.getByRole("switch", { name: /仅网关模式/ }).click();
  await magpieCard.getByText("仅网关模式设置已保存", { exact: true }).waitFor();
  await magpieCard.getByRole("button", { name: "接入已有实例", exact: true }).click();
  const magpieUrl = magpieCard.getByRole("textbox", { name: "外部 Magpie 网页地址" });
  await magpieUrl.fill("https://magpie.example.test");
  await magpieCard.getByRole("button", { name: "保存外部地址", exact: true }).click();
  await magpieCard.getByText("外部地址已保存", { exact: true }).waitFor();
  assert.equal(magpie.enabled, false, "saving must not enable Magpie or change account scheduling");
  assert.equal(magpie.port, 4491);
  assert.equal(magpie.gateway_only, true);
  const herdrCard = page.getByRole("region", { name: "Herdr 持久终端" });
  const takeover = herdrCard.getByRole("switch", { name: /新建终端都放进 Herdr/ });
  await takeover.click();
  await herdrCard.getByText("新终端恢复直连", { exact: true }).waitFor();
  await section("通用").click();
  await section("Agent CLI").click();
  assert.equal(await magpieUrl.inputValue(), "https://magpie.example.test/");
  assert.equal(await takeover.isChecked(), false);
  assert.equal(herdr.enabled, false);

  await section("本机终端").click();
  const native = page.getByRole("region", { name: "在本机终端中打开" });
  await native.getByRole("alert").waitFor();
  const clipboard = native.getByRole("switch", { name: /允许环境写入剪贴板/ });
  await clipboard.click();
  failSettings = true;
  await native.getByRole("button", { name: "保存", exact: true }).click();
  await native.getByText(/保存本机终端设置失败/).waitFor();
  assert.equal(local.clipboard_write, true);
  assert.equal(await clipboard.isChecked(), false);
  failSettings = false;
  await native.getByRole("button", { name: "保存", exact: true }).click();
  await native.getByText("已保存", { exact: true }).waitFor();
  failAppDetection = false;
  await native.getByRole("button", { name: "重新读取本机终端状态" }).click();
  await native.getByText(/用 Ghostty 打开/).waitFor();
  await section("通用").click();
  await section("本机终端").click();
  await native.getByText(/用 Ghostty 打开/).waitFor();
  assert.equal(await clipboard.isChecked(), false);

  await section("T3 Code").click();
  await section("Orca Server").click();
  Object.assign(state.status, { vm_running: true, ready: true });
  await chooseInline(orcaCard.getByRole("combobox", { name: "登记 Codex 账号到 Orca" }), "native-codex");
  const register = orcaCard.getByRole("button", { name: "登记到 Orca", exact: true });
  await register.click();
  const confirmRegistration = page.getByRole("dialog", { name: /登记到客机 Orca/ });
  await confirmRegistration.getByRole("button", { name: "取消" }).click();
  assert.deepEqual(registrations, []);
  await register.click();
  await confirmRegistration.getByRole("button", { name: "登记账号" }).click();
  await orcaCard.getByText("「原生 Codex 账号」已登记到 Orca。", { exact: true }).waitFor();
  assert.deepEqual(registrations, [{ account_id: "native-codex" }]);
  await section("T3 Code").click();
  await url.fill("https://draft.example.test");
  const switched = page.waitForEvent("load");
  context.connection_id = "fixture-b";
  context.virtual_machine_id = "windows-fixture";
  Object.assign(state.status, { guest_system: "windows-11" });
  await switched;
  await openSettings();
  await section("Orca Server").click();
  await orcaCard.getByText(/Windows 原生客机暂不支持托管 Orca Server/).waitFor();
  assert.equal(await orcaToggle.isDisabled(), true);
  assert.equal(await nameInput.count(), 0);
  await section("T3 Code").click();
  await url.waitFor();
  assert.equal(await url.inputValue(), "");
  assert.equal(await t3Card.getByRole("button", { name: /客机内置托管/ }).isDisabled(), true);
  await t3Toggle.click();
  await url.fill("https://windows-t3.example.test");
  await t3Card.getByRole("button", { name: "保存地址并接入" }).click();
  await t3Card.getByText("外部地址已保存并接入", { exact: true }).waitFor();
  assert.equal(t3Windows.enabled, true);
  assert.equal(t3Windows.external_url, "https://windows-t3.example.test/");
  assert.equal(t3.external_url, "https://t3.example.test/");
  assert.deepEqual(protectedActions, []);
  assert.deepEqual(errors, []);
} finally {
  await browser.close();
}
console.log("PASS: Orca, T3 Code, Agent CLI, Magpie, Herdr and local terminal settings, save errors, cancel, readback, and environment switch");
