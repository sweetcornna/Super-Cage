// Populated data for reviewing the module pages (settings sub-pages, accounts,
// connections, browser chrome, initialization) in the isolated workbench
// fixture. Opt-in: call routeModulePages(page, state, options) from a
// fixture's beforeNavigate; nothing here changes workbench-fixture.mjs answers.
//
// options.variant: "full" (default, long names included), "empty" or "error".
// It is read on every request, so a script can switch it and reload.

const HOME = "/Users/shihao";
export const projects = {
  design: HOME + "/Projects/super-cage-design-language-and-desktop-workbench",
  brand: HOME + "/Projects/林间工作室-品牌官网与小程序重构计划",
  workspace: HOME + "/.claude-cage-workspace",
};

const text = (status, body) => ({ status, contentType: "text/plain; charset=utf-8", body });

export function vaultData(now) {
  return {
    entries: [
      { id: "v1", origin: "https://mail.linjian.example", username: "studio@linjian.example", updated_at: now },
      { id: "v2", origin: "https://github.com", username: "linjian-studio" },
      { id: "v3", origin: "https://console.cloud-provider-with-a-very-long-domain-name.example.com", username: "night.shift.automation.long-address@example.test" },
      { id: "v4", origin: "https://figma.com", username: "" },
      { id: "v5", origin: "https://app.netlify.com", username: "design@linjian.example" },
    ],
    blocked: ["https://bank.example.cn", "https://intranet.linjian.example"],
  };
}

export function archivedSessions(now) {
  const ago = (minutes) => new Date(now - minutes * 60000).toISOString();
  return [
    { id: "a1", workspace: projects.design, title: "梳理设置页的分组与保存栏行为", updated_at: ago(60 * 5), archived: true },
    { id: "a2", workspace: projects.brand, title: "Rewrite the onboarding copy for the mini-program landing page and align every illustration with the brand guide", updated_at: ago(60 * 30), archived: true },
    { id: "a3", workspace: projects.workspace, title: "你好", updated_at: ago(60 * 24 * 6), archived: true },
    { id: "a4", workspace: projects.design, title: "排查终端在窄窗口下的换行问题", updated_at: ago(60 * 24 * 12), archived: true },
  ];
}

export const proxyData = (running, now = Date.now()) => ({
  sources: [
    { id: "airport", name: "旅行订阅 · 月付套餐（每 12 小时自动更新）", remote: true, interval_hours: 12,
      host: "sub.travel-proxy.example", updated_at: new Date(now - 3 * 3600000).toISOString(),
      nodes: [
        { id: "la02", name: "🇺🇸 USA Los Angeles 02", type: "anytls" },
        { id: "tokyo", name: "Tokyo Transit", type: "trojan" },
        { id: "sg", name: "Singapore Premium IPLC 专线 · 低延迟 · 适合日常浏览与视频会议", type: "vless" },
        { id: "fra", name: "Frankfurt 01", type: "hysteria2" },
      ] },
    { id: "home", name: "住宅代理", remote: false, interval_hours: 0, updated_at: new Date(now - 4 * 86400000).toISOString(),
      nodes: [{ id: "res", name: "LoopsForge · AI 住宅 IP · 节点001", type: "vless" }] },
  ],
  selected: "la02",
  route: ["la02"],
  active: true,
  running: true,
  core_available: true,
  version: "mihomo 1.19.12",
  port: 17896,
  state: running ? "ready" : "prepared",
  state_label: running ? "已联网" : "待启动环境",
  network_open: Boolean(running),
  route_error: "",
});

export const proxyConnections = () => ({
  uploadTotal: 18_400_000,
  downloadTotal: 412_000_000,
  connections: [
    { id: "c1", host: "api.anthropic.com", port: 443, network: "tcp", upload: 182_000, download: 2_400_000 },
    { id: "c2", host: "statsig.anthropic.com", port: 443, network: "tcp", upload: 12_000, download: 48_000 },
    { id: "c3", host: "objects.githubusercontent.com", port: 443, network: "tcp", upload: 4_000, download: 86_000_000 },
    { id: "c4", host: "registry.npmjs.org", port: 443, network: "tcp", upload: 9_000, download: 3_100_000 },
  ],
});

export const proxyLogs = () => ({ lines: [
  "15:18:02 核心已启动 · mihomo 1.19.12 · 端口 17896",
  "15:18:03 已选择出口 🇺🇸 USA Los Angeles 02 (anytls)",
  "15:18:05 出口检查通过 · United States · Los Angeles",
  "15:19:40 订阅「旅行订阅」已更新 · 4 个节点",
  "15:20:11 DNS 经 https://cloudflare-dns.com/dns-query 解析",
] });

export const environmentVersions = () => ({
  controller: { version: "0.1.15 (b213406)" },
  components: { status: "update_required", update_required: true },
  tools: {
    chromium: { current: "142.0.7444.59", required: "142.0.7444.59" },
    code_server: { current: "4.104.3", required: "4.105.1" },
    claude: { current: "2.0.37", required: "2.0.42" },
    node: { current: "22.21.0", required: "22.21.0" },
    python: { current: "3.13.9", required: "3.13.9" },
    git: { current: "2.51.2", required: "2.51.2" },
    gh: { current: "", required: "2.82.1" },
    proxy: { current: "1.19.12", required: "1.19.12" },
  },
  tools_update_allowed: true,
});

export const cacheInventory = (now) => ({
  plan_id: "plan-1",
  notice: "盘点于刚才完成。只列出允许清单中的缓存，预览 10 分钟内有效。",
  items: [
    { id: "npm", title: "npm 下载缓存", location: "~/.npm/_cacache", reason: "可重新下载", bytes: 812 * 1024 * 1024, files: 18342, state: "ready" },
    { id: "pip", title: "pip 下载缓存", location: "~/.cache/pip", reason: "可重新下载", bytes: 164 * 1024 * 1024, files: 2210, state: "ready" },
    { id: "chromium", title: "浏览器缓存（不含登录与 Cookie）", location: "~/.config/chromium/Default/Cache/Cache_Data/with/a/very/long/path/segment", reason: "浏览器正在运行，暂不能清理", bytes: 96 * 1024 * 1024, files: 940, state: "busy" },
  ],
  protected: [
    { title: "Claude 登录资料", present: 2, unknown: 0 },
    { title: "浏览器 Cookie 与登录", present: 1, unknown: 1 },
  ],
  batches: [{ id: "b1", created_at: Math.floor(now / 1000) - 86400 * 2, count: 3 }],
});

export const resources = (now) => ({
  automatic: false, memory_gib: 8, cpus: 4, running: true, restart_required: true,
  recommended_memory_bytes: 8 * 2 ** 30, recommended_cpus: 4, max_memory_gib: 16, max_cpus: 8,
  configured_memory_bytes: 6 * 2 ** 30, configured_cpus: 3, resources_verified: true,
  observation: { observed_at: new Date(now).toISOString(), cpu_percent: 23.4, cpus: 3,
    guest: { total_bytes: 6 * 2 ** 30, available_bytes: 3.4 * 2 ** 30, oom_kills: 0 } },
});

// The automatic allocation answers (internal/cage/resource-allocation.md §7.10),
// one per state of the 计算资源 section; "older" is a server without them.
export function resourceAllocations(now) {
  const GiB = 2 ** 30;
  const sample = (extra = {}) => ({ observed_at: new Date(now).toISOString(), maximum_bytes: 6 * GiB, target_bytes: 4 * GiB, cpus: 5, cpu_percent: 23.4,
    capacity_bytes: 5.2 * GiB, demand_bytes: 2.6 * GiB, possible_bytes: 6 * GiB, summary: "环境内存充足（分配 4 GB，需要 2.6 GB）。", next_action: "无需操作。",
    guest: { total_bytes: 4 * GiB, available_bytes: 1.6 * GiB, oom_kills: 0 }, ...extra });
  const base = { running: true, max_memory_gib: 6, max_cpus: 5, recommended_memory_bytes: 6 * GiB, recommended_cpus: 5,
    automatic_memory_gib: 6, automatic_cpus: 5, possible_memory_bytes: 6 * GiB, resource_error: "" };
  return {
    automatic: { ...base, automatic: true, allocation_origin: "automatic", memory_gib: 6, cpus: 5, configured_memory_bytes: 6 * GiB, configured_cpus: 5,
      restart_required: false, observation: sample() },
    restart: { ...base, automatic: true, allocation_origin: "automatic", memory_gib: 6, cpus: 5, configured_memory_bytes: 3 * GiB, configured_cpus: 3,
      restart_required: true, notice: "自动分配已按这台机器重新计算为内存 6 GB、CPU 5 核，重启环境后生效。",
      observation: sample({ maximum_bytes: 3 * GiB, target_bytes: 3 * GiB }) },
    legacy: { ...base, automatic: false, allocation_origin: "legacy_default", memory_gib: 3, cpus: 3, configured_memory_bytes: 3 * GiB, configured_cpus: 3,
      restart_required: false, legacy_allocation: true,
      notice: "内存 3 GB、CPU 3 核与旧版本的默认设置相同。改为自动（推荐）后，这台机器可以给到 6 GB、5 核，需要重启环境；如果这是你特意设的，可以保留。",
      offer: { automatic: true, memory_gib: 6, cpus: 5, restart_required: true }, observation: sample({ maximum_bytes: 3 * GiB, target_bytes: 3 * GiB }) },
    advice: { ...base, automatic: false, allocation_origin: "user", memory_gib: 3, cpus: 2, configured_memory_bytes: 3 * GiB, configured_cpus: 2,
      restart_required: false,
      advice: { message: "环境内存已用满手动设置的 3 GB。改为自动（推荐）后可以调到 6 GB，需要重启环境。", memory_bytes: 6 * GiB, restart_required: true },
      observation: sample({ maximum_bytes: 3 * GiB, target_bytes: 3 * GiB, short: true, limited_by: "maximum", guest: { total_bytes: 3 * GiB, available_bytes: 0.2 * GiB, oom_kills: 0 },
        summary: "环境内存紧张（分配 3 GB，需要 3.4 GB），已到上限。" }) },
    manual: null,
    // The outage: the old default at its maximum and short; and the automatic
    // size recalculated while short at the old maximum.
    outage: { ...base, automatic: false, allocation_origin: "legacy_default", memory_gib: 3, cpus: 3, configured_memory_bytes: 3 * GiB, configured_cpus: 3,
      restart_required: false, legacy_allocation: true,
      notice: "内存 3 GB、CPU 3 核与旧版本的默认设置相同。改为自动（推荐）后，这台机器可以给到 6 GB、5 核，需要重启环境；如果这是你特意设的，可以保留。",
      offer: { automatic: true, memory_gib: 6, cpus: 5, restart_required: true },
      advice: { message: "环境内存已用满手动设置的 3 GB。改为自动（推荐）后可以调到 6 GB，需要重启环境。", memory_bytes: 6 * GiB, restart_required: true },
      observation: sample({ maximum_bytes: 3 * GiB, target_bytes: 3 * GiB, short: true, limited_by: "maximum", guest: { total_bytes: 3 * GiB, available_bytes: 0.15 * GiB, oom_kills: 2 } }) },
    "restart-short": { ...base, automatic: true, allocation_origin: "automatic", memory_gib: 6, cpus: 5, configured_memory_bytes: 3 * GiB, configured_cpus: 3,
      restart_required: true, notice: "自动分配已按这台机器重新计算为内存 6 GB、CPU 5 核，重启环境后生效。",
      advice: { message: "可以把内存调到 6 GB，需要重启环境。", memory_bytes: 6 * GiB, restart_required: true },
      observation: sample({ maximum_bytes: 3 * GiB, target_bytes: 3 * GiB, short: true, limited_by: "maximum" }) },
    older: { automatic: true, memory_gib: 12, cpus: 5, running: false, recommended_memory_bytes: 12 * GiB, recommended_cpus: 5, max_memory_gib: 12, max_cpus: 5 },
  };
}

// Tailscale states for the panel (tailscale-smoke shapes), long names included.
export function tailscaleState(variant) {
  const base = { installed: true, version: "1.104.5", enabled: false, backend: "Stopped", ips: [], peers: [], keys: [], forwards: [], user: "cage", ssh_port: 22 };
  const connected = { ...base, enabled: true, backend: "Running", tailnet: "林间私网", account: "studio@linjian.example", name: "cage-workbench.tail1234.ts.net",
    hostname: "cage-workbench", os: "linux", ips: ["100.88.2.3", "fd7a:115c:a1e0::3"], proxy_url: "http://cage:fixture-credential@127.0.0.1:32100",
    keys: ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIM2b7y0k3dW4fV1x6mQp9rZ0xY8u7t6s5r4q3p2o1n0m studio@MacBook-Pro"],
    forwards: [{ host: "100.88.2.4", port: 5173, address: "127.0.0.1:41873" }],
    peers: [
      { name: "studio-macbook-pro.tail1234.ts.net", hostname: "studio-macbook-pro", os: "macOS", ip: "100.88.2.4", online: true },
      { name: "iphone-16-pro-max.tail1234.ts.net", hostname: "iphone-16-pro-max", os: "iOS", ip: "100.88.2.7", online: true },
      { name: "render-farm-node-with-a-very-long-hostname-for-overnight-jobs.tail1234.ts.net", hostname: "render-farm-node-with-a-very-long-hostname-for-overnight-jobs", os: "linux", ip: "100.88.2.12", online: false },
      { name: "nas.tail1234.ts.net", hostname: "nas", os: "linux", ip: "100.88.2.5", online: false },
    ] };
  if (variant === "connected") return connected;
  if (variant === "login") return { ...base, enabled: true, backend: "NeedsLogin", auth_url: "https://login.tailscale.com/a/fixture-login" };
  if (variant === "health") return { ...connected, peers: [], health: ["Tailscale 无法连接到协调服务器：DNS 解析超时。", "此设备的密钥将在 3 天后过期，请在管理台续期。"] };
  if (variant === "missing") return { ...base, installed: false };
  return base;
}

export const aboutBuild = { version: "0.1.15", commit: "b213406c0ffee5e1d9a0c4c2f5b7d81a90e3f2aa", modified: false };
export const aboutCheck = (now) => ({
  build: aboutBuild,
  checkedAt: new Date(now).toISOString(),
  source: { status: "available", commit: "f737fcfb2d9e0c4b8a7d6e5f4c3b2a1908172635", aheadBy: 6 },
  release: { status: "current", version: "v0.1.15", prerelease: false },
});

export function claudeAccounts(seconds) {
  return [
    { id: "p1", label: "林间工作室 · 主力账号", email: "studio@linjian.example", plan: "Max", enabled: true, status: "ready", handoff_group: "工作室",
      quota: { observed_at: seconds - 120, five_hour: { used_percentage: 42, resets_at: seconds + 5400, observed_at: seconds - 120 },
        seven_day: { used_percentage: 71, resets_at: seconds + 260000, observed_at: seconds - 120 } }, usage_status: "reported", usage_checked_at: seconds - 120 },
    { id: "p2", label: "备用账号（周末与夜间自动化任务专用，额度用完后自动切换）", email: "night.shift.automation.long-address@example.test",
      plan: "Pro", enabled: true, status: "signed_out" },
    { id: "p3", label: "设计评审", email: "review@example.test", plan: "Team", enabled: false, status: "ready",
      quota: { observed_at: seconds - 4000, five_hour: { used_percentage: 96, resets_at: seconds + 1200, observed_at: seconds - 4000 },
        seven_day: { used_percentage: 15, resets_at: seconds + 400000, observed_at: seconds - 4000 } }, usage_status: "reported", usage_checked_at: seconds - 4000 },
    { id: "p4", label: "新账号", enabled: true, status: "pending" },
  ];
}

export function codexAccounts(seconds) {
  return [
    { id: "a".repeat(24), label: "工作账号", email: "work@linjian.example", plan: "pro", enabled: true, status: "ready",
      quota: { primary: { used_percentage: 42, resets_at: seconds + 3600, window_minutes: 300, observed_at: seconds - 30 },
        secondary: { used_percentage: 7, resets_at: seconds + 86400 * 3, window_minutes: 10080, observed_at: seconds - 30 }, observed_at: seconds - 30 },
      usage_status: "reported", usage_checked_at: seconds - 30 },
    { id: "b".repeat(24), label: "备用账号 · 夜间批处理与发布脚本审阅专用", email: "spare.batch.reviews@example.invalid", enabled: true, status: "signed_out" },
    { id: "c".repeat(24), label: "实验账号", email: "lab@example.invalid", plan: "plus", enabled: false, status: "ready",
      quota: { primary: { used_percentage: 88, resets_at: seconds + 900, window_minutes: 300, observed_at: seconds - 600 },
        secondary: { used_percentage: 55, resets_at: seconds + 86400, window_minutes: 10080, observed_at: seconds - 600 }, observed_at: seconds - 600 },
      usage_status: "reported", usage_checked_at: seconds - 600 },
  ];
}

// Registers the routes. Playwright tries the last registered route first, so
// these answer before the generic fixture handler.
export async function routeModulePages(page, state, options = {}) {
  const now = options.now || Date.now();
  const variant = () => options.variant || "full";
  const json = (route, value) => route.fulfill({ json: value });
  const failed = (route, message) => route.fulfill(text(503, message));
  await page.route("**/api/vault", (route) => {
    if (route.request().method() !== "GET") return json(route, { ok: true });
    if (variant() === "error") return failed(route, "密码保险箱暂时无法读取，请重新解锁 Super Cage 后再试");
    return json(route, variant() === "empty" ? { entries: [], blocked: [] } : vaultData(now));
  });
  await page.route(/\/api\/cli\/sessions\?archived=true$/, (route) => {
    if (variant() === "error") return failed(route, "无法读取已归档会话：会话服务暂时不可用");
    return json(route, { sessions: variant() === "empty" ? [] : archivedSessions(now), notice: "" });
  });
  await page.route("**/api/mobile-pair", (route) => json(route, variant() === "full"
    ? { paired: true, status: "", device: { model: "iPhone 16 Pro Max", platform: "ios" }, keep_awake: { active: true } }
    : { paired: false, status: "" }));
  await page.route("**/api/proxy", (route) => {
    if (route.request().method() !== "GET") return json(route, {});
    const value = proxyData(state.status.vm_running, now);
    if (variant() === "empty") Object.assign(value, { sources: [], selected: "", route: [], active: false, running: false, state_label: "未配置代理" });
    if (variant() === "error") {
      value.route_error = "代理核心拒绝该配置：节点 🇺🇸 USA Los Angeles 02 握手失败；原配置已保留";
      value.sources[0].error = "订阅更新失败：sub.travel-proxy.example 返回 403，已保留上次的 4 个节点";
      value.sources[0].skipped = [{ index: 5, name: "Legacy SS", type: "ss", reason: "不支持的加密方式" }];
    }
    return json(route, value);
  });
  await page.route("**/api/proxy/connections", (route) => json(route, variant() === "empty" ? { uploadTotal: 0, downloadTotal: 0, connections: [] } : proxyConnections()));
  await page.route("**/api/proxy/logs", (route) => json(route, proxyLogs()));
  await page.route("**/api/environment/versions", (route) => variant() === "error"
    ? failed(route, "无法读取环境版本：环境服务没有响应") : json(route, environmentVersions()));
  await page.route("**/api/environment/cache/inventory", (route) => variant() === "error"
    ? failed(route, "测试：请重新盘点，已有资料保留。") : json(route, variant() === "empty" ? { ...cacheInventory(now), items: [], batches: [] } : cacheInventory(now)));
  await page.route("**/api/environment/resources", (route) => route.request().method() === "GET"
    ? json(route, (variant() === "manual" ? resourceAllocations(now).automatic : resourceAllocations(now)[variant()]) || resources(now)) : json(route, { ok: true }));
  await page.route("**/api/diagnostics", (route) => json(route, { bytes: 12.4 * 1024 * 1024, files: 37 }));
  await page.route("**/api/environment/storage", (route) => json(route, { directory: "/Volumes/Studio SSD/Super Cage/环境", has_environment: true, backup: "/Users/shihao/Library/Application Support/Super Cage/backup-2026-11-18" }));
  await page.route("**/api/client/about", (route) => {
    if (route.request().method() === "GET") return json(route, aboutBuild);
    if (variant() === "error") return failed(route, "无法连接 GitHub：主机网络与 Super Cage 代理均未能完成请求");
    return json(route, aboutCheck(now));
  });
  const seconds = Math.floor(now / 1000);
  await page.route("**/api/tailscale**", (route) => {
    const tail = ["connected", "login", "health", "stopped", "missing"].includes(variant()) ? variant() : "stopped";
    return json(route, tailscaleState(tail));
  });
  await page.route("**/api/claude-accounts*", (route) => {
    if (variant() === "error") return json(route, { accounts: claudeAccounts(seconds).slice(0, 2), detection_status: "signed_out",
      detection_error: "无法检测普通终端的登录：Claude Code 没有响应，请稍后重试", login_notice: "", usage_panel_running: false,
      usage_unavailable_reason: "额度查询暂不可用：当前代理拒绝了 claude.ai 的请求", scheduling: { mode: "balanced", switch_policy: "ask" } });
    if (variant() === "detected") return json(route, {
      accounts: [{ id: "p0", source: "default", label: "me@linjian.example", email: "me@linjian.example", organization: "林间工作室", plan: "Max", enabled: true, status: "changed" },
        ...claudeAccounts(seconds)],
      detected: { identity: "d".repeat(64), email: "night.owl@linjian.example", organization: "林间工作室", plan: "Pro",
        quota: { observed_at: seconds - 60, five_hour: { used_percentage: 12, resets_at: seconds + 9000, observed_at: seconds - 60 },
          seven_day: { used_percentage: 33, resets_at: seconds + 300000, observed_at: seconds - 60 } }, usage_status: "reported", usage_checked_at: seconds - 60 },
      detection_status: "ready", detection_error: "", login_notice: "", usage_panel_running: false, usage_unavailable_reason: "",
      scheduling: { mode: "sequential", switch_policy: "ask" } });
    return json(route, { accounts: variant() === "empty" ? [] : claudeAccounts(seconds), detection_status: "signed_out", detection_error: "",
      login_notice: "", usage_panel_running: false, usage_unavailable_reason: "", scheduling: { mode: "balanced", switch_policy: "ask" } });
  });
  await page.route("**/api/codex-accounts*", (route) => json(route, { accounts: variant() === "empty" ? [] : codexAccounts(seconds),
    scheduling: { mode: "balanced", switch_policy: "ask" }, guest_supported: true, detection_status: "ready",
    detected: variant() === "empty" ? { identity: "f".repeat(64), email: "default@linjian.example", plan: "plus", checked_at: seconds } : undefined }));
  await page.route(/\/api\/cli\/sessions\/[^/]+\/account$/, (route) => json(route, { supported: true, switch_policy: "ask",
    session: { id: "s1", account_id: "p1", account_label: "林间工作室 · 主力账号", account_failure: variant() === "error" ? "rate_limit" : "" },
    candidates: [{ id: "p3", label: "设计评审" }, { id: "p2", label: "备用账号（周末与夜间自动化任务专用）" }] }));
  for (const extra of options.extraRoutes || []) await extra(page, state, { now, variant });
}

// Browser chrome: tabs with long titles, favourites, the password picker with a
// save prompt and the cookie list. Opt-in like routeModulePages: pass it in
// options.extraRoutes, and call browserTabs(now) for state.tabs.
export function browserTabs() {
  return [
    { id: "tab-1", kind: "web", title: "林间工作室 · 品牌官网与小程序重构计划 - 设计评审记录与发布排期", url: "https://linjian.example/projects/brand-site/review" },
    { id: "tab-2", kind: "web", title: "GitHub - sweetcornna/example: a very long repository description that keeps going", url: "https://github.com/example/example" },
    { id: "tab-3", kind: "web", title: "Claude", url: "https://claude.ai/new" },
    { id: "tab-4", kind: "web", title: "Super Cage 设计语言", url: "https://docs.linjian.example/super-cage/design-language" },
    { id: "tab-5", kind: "web", title: "新标签页", url: "https://www.bing.com/" },
  ];
}

export function favoritesData() {
  return {
    folders: [{ id: "f1", title: "设计参考" }, { id: "f2", title: "夜间批处理与发布脚本审阅（长名称收藏夹）" }],
    items: [
      { id: "b1", title: "林间工作室 · 品牌官网与小程序重构计划 - 设计评审记录与发布排期", url: "https://linjian.example/projects/brand-site/review", folder_id: "f1" },
      { id: "b2", title: "Claude", url: "https://claude.ai/new", folder_id: "" },
      { id: "b3", title: "Super Cage 设计语言", url: "https://docs.linjian.example/super-cage/design-language", folder_id: "f1" },
      { id: "b4", title: "Netlify 部署面板", url: "https://app.netlify.com/sites/linjian-studio-brand-site-preview/deploys", folder_id: "f2" },
      { id: "b5", title: "", url: "https://figma.com/file/abc", folder_id: "" },
    ],
  };
}

export async function routeBrowserChrome(page, state, { now, variant }) {
  const json = (route, value) => route.fulfill({ json: value });
  await page.route("**/api/favorites", (route) => route.request().method() === "GET"
    ? json(route, variant() === "empty" ? { items: [], folders: [] } : favoritesData())
    : json(route, favoritesData()));
  await page.route("**/api/vault/browser", (route) => {
    const tab = state.active || "tab-1";
    if (variant() === "error") return route.fulfill(text(503, "密码保险箱已锁定，请先解锁 Super Cage"));
    return json(route, { available: true, tab, origin: "https://linjian.example", document: "d1",
      entries: variant() === "empty" ? [] : [{ id: "v1", username: "studio@linjian.example" }, { id: "v6", username: "night.shift.automation.long-address@example.test" }],
      candidates: variant() === "empty" ? [] : [{ id: "c1", tab, origin: "https://linjian.example", username: "design@linjian.example", update: false }] });
  });
  await page.route("**/api/browser/cookies", (route) => {
    if (variant() === "error") return route.fulfill(text(503, "浏览器暂时没有响应，Cookie 无法读取"));
    const cookies = variant() === "empty" ? [] : [
      { key: "k1", name: "session_id", domain: ".linjian.example", path: "/", size: 64, session: true, secure: true, httpOnly: true, sameSite: "Lax" },
      { key: "k2", name: "preferences_theme_and_layout_for_the_brand_site_preview", domain: "linjian.example", path: "/projects", size: 212, session: false, expires: now / 1000 + 86400 * 30, secure: true, sameSite: "Strict" },
      { key: "k3", name: "", domain: "cdn.linjian.example", path: "/", size: 12, session: true, partitionKey: { topLevelSite: "https://linjian.example" } },
    ];
    return json(route, { hostname: "linjian.example", cookies, total: cookies.length, page: 0 });
  });
}
