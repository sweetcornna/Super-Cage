// Fixture for 设置 → 其他客户端 (other-clients-smoke.mjs, claude-path-smoke.mjs):
// what GET /api/agents answers and what its install, update, remove and cancel
// routes do, as design 4.1 describes them. Install lines are the ones
// internal/cage/agent-install.sh prints (docs/agent-install.md). The routes are
// registered on a page after createWorkbench, so they take precedence over its
// generic handler and the shared fixture stays as it is.

const mb = 1048576;
export const environment = { running: true, arch: "arm64", guest_system: "debian-13", disk_free_mb: 18637 };

const base = {
  kind: "terminal", status: "available", with_environment: false, person_only: false, present: null, update_available: false,
  supported: true, unsupported_code: "", unsupported_reason: "", allowed: true, operation: null, terms_url: "",
  hosts: { install: ["github.com"], signin: ["auth.openai.com", "chatgpt.com"], run: ["chatgpt.com", "api.openai.com", "ab.chatgpt.com"], telemetry: [] },
  telemetry_note: "", region_note: "", data_dirs: [], mark: {},
};
export const entry = (over) => ({ ...base, ...over, hosts: { ...base.hosts, ...over.hosts } });

export const codex = (over = {}) => entry({
  id: "codex", name: "Codex", vendor: "OpenAI", summary: "OpenAI 的终端编程客户端", pinned_version: "0.171.0", download_mb: 124, disk_mb: 310,
  telemetry_note: "已默认关闭使用数据上报和启动时的更新检查。",
  region_note: "OpenAI 暂不向中国大陆和香港提供服务；登录取决于所选代理的出口地区。",
  data_dirs: [{ path: "~/.codex", shared: false }], mark: { builtin: "codex" }, ...over,
});
export const antigravity = (over = {}) => entry({
  id: "antigravity-cli", name: "Antigravity CLI", vendor: "Google", summary: "Google 的终端编程客户端", pinned_version: "1.2.0", download_mb: 98, disk_mb: 256,
  person_only: true, hosts: { install: ["storage.googleapis.com"], signin: ["accounts.google.com"], run: ["generativelanguage.googleapis.com", "cloudcode-pa.googleapis.com"], telemetry: [] },
  telemetry_note: "这个客户端的遥测是账号级设置，Super Cage 无法预先关闭。可在登录后于设置 › 账号中关闭。",
  region_note: "", data_dirs: [{ path: "~/.antigravity", shared: false }], mark: { monogram: "Ag" }, ...over,
});

// The failure lines of design 4.4, as agent-install.sh prints them.
export const failures = {
  download: "下载中断，已保存进度。重试会继续下载。",
  digest: "安装文件校验失败，已停止安装，原有版本保持不变。",
  gone: "OpenAI 已不再提供 Codex 0.171.0 的下载。请更新 Super Cage 以获取新的固定版本。",
  disk: "客机磁盘空间不足：需要 505 MB，当前只有 212 MB。",
  verify: "安装文件无法在这个客机运行，已停止安装，原有文件保持不变。",
  "in-use": "请先结束正在运行的 Codex 终端，再更新或修复。",
  "own-install": "已经安装了 Codex（自行安装，/home/dev/.local/bin/codex），Super Cage 不会覆盖它。",
  userns: "这个环境不允许普通用户创建命名空间，Codex 的沙箱无法运行，因此没有安装。",
  canceled: "已取消，已下载的部分会在下次继续使用。",
};
// Which of them offer 重试 (a retry cannot change the others).
export const retryable = { download: true, digest: true, gone: false, disk: true, verify: true, "in-use": true, "own-install": false, userns: false, canceled: true };

export const job = (kind, id, over = {}) => ({ id: "agent-job-" + id, kind, running: true, message: "", ...over });
export const progress = (phase, label, received, total) => ({ phase, label, received, total, bytes_per_second: 2 * mb });

/**
 * model: { entries, environment, calls[], reply(path, body) }. calls records
 * every request of the section (method, path, query, body).
 */
export function createAgents(entries, env = environment) {
  return { entries, environment: env, calls: [], replies: [], hold: null };
}
export async function installAgents(page, model) {
  await page.route("**/api/agents**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const body = request.method() === "POST" && request.postData() ? request.postDataJSON() : undefined;
    const path = url.pathname.replace("/api", "");
    model.calls.push({ method: request.method(), path, query: url.search, body });
    if (request.method() === "GET") {
      if (model.failList) return route.fulfill({ status: 500, contentType: "text/plain; charset=utf-8", body: model.failList });
      return route.fulfill({ json: { agents: model.entries, environment: model.environment } });
    }
    const reply = model.replies.shift();
    if (model.hold) await model.hold;
    if (reply?.status) return route.fulfill({ status: reply.status, contentType: "text/plain; charset=utf-8", body: reply.body });
    if (path === "/agents/cancel") return route.fulfill({ status: 204, body: "" });
    const [, , id, operation] = path.split("/");
    const accepted = reply?.json || job("agent-" + operation + ":" + id, id + "-" + model.calls.length);
    model.lastJob = accepted;
    return route.fulfill({ json: accepted });
  });
}
