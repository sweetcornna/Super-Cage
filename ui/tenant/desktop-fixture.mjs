// Test-only answers that give the desktop workbench in the mock workspace something to show: a running
// environment with two projects, open terminals and Claude sessions. Served with the phone fixture
// (startMockServer({ phoneFixture: true })) for the tablet screens of ui/scripts/mobile-web-matrix.mjs.
// Shapes follow ui/scripts/workbench-fixture.mjs; terminals have no live socket here.

const workspace = "/home/cage/super-cage";
const research = "/home/cage/数据清洗与可视化-一个名字很长的研究项目";
const iso = (minutes) => new Date(Date.UTC(2026, 10, 21, 8, 0) - minutes * 60_000).toISOString();

export function createDesktopFixture() {
  const settings = {
    workspace, browser_home_url: "https://www.bing.com/", dns_url: "https://cloudflare-dns.com/dns-query",
    timezone: "Asia/Shanghai", locale: "zh_CN.UTF-8", browser_language: "zh-CN", auto_timezone: true,
    auto_language: false, keyboard: "mac", exit_policy: { mode: "dynamic", scope: "country", country_code: "US" },
  };
  const status = {
    environment_directory: workspace, environment_workspace: true, projects: [workspace, research],
    ready: true, vm_running: true, keyboard: "mac", job: { running: false }, can_restore: true, workspace,
    guest_system: "debian-13",
  };
  const terminals = [
    { id: "tablet-claude", kind: "claude", title: "整理发布说明", workspace },
    { id: "tablet-shell", kind: "shell", title: "bash", workspace },
    { id: "tablet-research", kind: "shell", title: "数据清洗", workspace: research },
  ];
  const sessions = [
    { id: "tablet-s1", workspace, title: "修复门户在手机上的键盘遮挡问题，并补上回归测试", updated_at: iso(42), pinned: true },
    { id: "tablet-s2", workspace, title: "写一份部署检查清单", updated_at: iso(300) },
    { id: "tablet-s3", workspace: research, title: "清洗 2026 年第三季度的销售数据", updated_at: iso(1500) },
  ];
  // method, path (without the query): a JSON body, or undefined for the mock's own answer.
  return (method, path) => {
    if (method !== "GET") return undefined;
    if (path === "/api/status") return status;
    if (path === "/api/settings") return settings;
    if (path === "/api/terminals") return terminals;
    if (path === "/api/cli/sessions") return { sessions, notice: "" };
    if (path === "/api/browser/tabs") return { tabs: [] };
    return undefined;
  };
}
