// Test-only answers for the phone UI's /api/mobile/* routes, served by the mock
// workspace when startMockServer({ phoneFixture: true }) is used. The shapes
// follow internal/workbench (mobile_cli_store.go, mobile_files.go, ...); the
// data is invented and never leaves the test machine.

const project = "/home/cage/super-cage";
const longProject = "/home/cage/数据清洗与可视化-一个名字很长的研究项目";
const iso = (minutes) => new Date(Date.UTC(2026, 10, 21, 8, 0) - minutes * 60_000).toISOString();
const millis = (minutes) => Date.UTC(2026, 10, 21, 8, 0) - minutes * 60_000;
const id = (n) => `5c0a1e2d-0000-4000-8000-${String(n).padStart(12, "0")}`;

const markdown = [
  "发布说明已整理好，放在 `docs/release.md`。主要变化：",
  "",
  "1. 手机网页版自动进入手机界面，可以随时切换到电脑版。",
  "2. 门户在手机上全屏打开工作台，键盘弹出时输入框不被遮挡。",
  "3. 首屏只下载手机需要的资源。",
  "",
  "```bash",
  "npm --prefix ui run build && go test -p 2 ./internal/webentry ./internal/tenant",
  "```",
  "",
  "详情见 [发布说明](https://example.com/release-notes)。",
].join("\n");

export function createPhoneFixture() {
  const sessions = [
    { id: id(1), workspace: project, title: "整理发布说明", revision: 3, generation: id(1), updated_at: iso(3), pinned: true, active: false },
    { id: id(2), workspace: project, title: "修复门户在手机上的键盘遮挡问题，并补上回归测试", revision: 2, generation: id(2), updated_at: iso(42), pinned: false, active: true },
    { id: id(3), workspace: project, title: "Refactor the session store to use revisions and long-poll waits", revision: 1, generation: id(3), updated_at: iso(180), pinned: false, active: false },
    { id: id(4), workspace: longProject, title: "清洗 2026 年第三季度的销售数据", revision: 1, generation: id(4), updated_at: iso(1500), pinned: false, active: false, provider: "codex" },
    { id: id(5), workspace: project, title: "写一份部署检查清单", revision: 1, generation: id(5), updated_at: iso(3000), pinned: false, active: false },
  ];
  const turns = (session) => session.id === id(1) ? [
    { id: "t1", prompt: "整理 0.1.16 的发布说明，按用户能感知到的变化分组。", status: "completed", text: markdown,
      tools: [{ id: "u1", name: "Read", detail: "docs/release.md", status: "completed" }, { id: "u2", name: "Bash", detail: "git log --oneline v0.1.15..HEAD", status: "completed" }] },
    { id: "t2", prompt: "再把第二条写得更具体一点：键盘弹出时发生了什么？", status: "completed",
      text: "门户在手机上把工作台铺满整个屏幕；键盘弹出时，工作台的高度跟着可见区域缩小，输入框始终停在键盘上方，页面不会整体跳动。" },
  ] : session.id === id(2) ? [
    { id: "t3", prompt: "看一下 iPhone 上键盘弹出后输入框被挡住的问题。", status: "running", text: "",
      approval: { id: "a1", tool: "Bash", input: { command: "npm --prefix ui run build", description: "构建界面" } } },
  ] : [];
  const snapshot = (session) => ({ ...session, turns: turns(session), can_send: true, terminal_available: false });
  const terminals = [
    { id: "0123456789abcdef", kind: "claude", title: "claude", workspace: project, closed: false },
    { id: "fedcba9876543210", kind: "shell", title: "bash", workspace: longProject, closed: true, exit_code: 0 },
  ];
  const entries = [
    { name: "docs", type: "dir", mtime: millis(60) },
    { name: "internal", type: "dir", mtime: millis(90) },
    { name: "ui", type: "dir", mtime: millis(30) },
    { name: "README.md", type: "file", size: 4211, mtime: millis(600) },
    { name: "go.mod", type: "file", size: 1830, mtime: millis(9000) },
    { name: "一个很长的中文文件名-用于检查截断和换行是否正常.md", type: "file", size: 120, mtime: millis(20) },
    { name: "screenshot-2026-11-21-at-08.15.42-mobile-portrait.png", type: "file", size: 381244, mtime: millis(5) },
  ];
  const readme = Buffer.from("# Super Cage\n\n手机网页版测试项目。\n").toString("base64");
  const ok = (body) => ({ status: 200, body });

  return function answer(method, path, body) {
    const cli = path.replace(/^\/api\/mobile\/cli/, "");
    if (path === "/api/mobile/cli/projects") return ok({ host: "开发环境", ready: true, shared_sessions: true,
      projects: [{ path: project, name: "super-cage" }, { path: longProject, name: "数据清洗与可视化-一个名字很长的研究项目" }] });
    if (path === "/api/mobile/cli/sessions" && method === "GET") return ok({ sessions, notice: "" });
    if (path === "/api/mobile/cli/sessions" && method === "POST") {
      const created = { id: id(9), workspace: body?.workspace || project, title: "新对话", revision: 1, generation: id(9), updated_at: iso(0), pinned: false, active: false };
      if (!sessions.some((s) => s.id === created.id)) sessions.unshift(created);
      return ok(snapshot(created));
    }
    const match = /^\/sessions\/([^/]+)(\/[a-z]+)?$/.exec(cli);
    if (match) {
      const session = sessions.find((s) => s.id === decodeURIComponent(match[1]));
      if (!session) return { status: 404, body: "会话不存在" };
      if (match[2] === "/account") return ok({ session: snapshot(session), supported: false, candidates: [] });
      return ok(snapshot(session));
    }
    if (path === "/api/mobile/environment") return ok({ ready: true, vm_running: true, guest_system: "Debian 12",
      network: { mode: "open", exit_ip: "203.0.113.7" }, proxy: { selected: "n1", selected_name: "东京 01" }, job: { running: false } });
    if (path === "/api/mobile/environment/proxy") return ok({ sources: [{ id: "s1", name: "订阅", nodes: [{ id: "n1", name: "东京 01" }, { id: "n2", name: "新加坡 02" }] }] });
    if (path === "/api/mobile/terminals") return ok({ terminals });
    const output = /^\/api\/mobile\/terminals\/([0-9a-f]{16})\/output$/.exec(path);
    if (output) return ok({ data: Buffer.from("\u001b[1m~/super-cage\u001b[0m $ git status\r\nOn branch mobile-web\r\nnothing to commit, working tree clean\r\n$ ").toString("base64"), next: 1, closed: output[1] === terminals[1].id, exit_code: 0 });
    if (/^\/api\/mobile\/terminals\/[0-9a-f]{16}\/(input|resize)$/.test(path)) return ok({});
    if (path === "/api/mobile/files/list") return ok({ entries: body?.path ? entries.filter((e) => e.type !== "dir") : entries, truncated: false });
    if (path === "/api/mobile/files/read") return ok({ size: 40, mime: "text/markdown", version: "v1", data: body?.offset ? "" : readme });
    if (path === "/api/mobile/browser/favorites") return ok({ items: [], folders: [] });
    return { status: 404, body: "未找到" };
  };
}
