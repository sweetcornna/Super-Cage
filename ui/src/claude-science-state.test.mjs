import assert from "node:assert/strict";
import { createRequire } from "node:module";
import test from "node:test";
import { formatMB, hostFrameStatus, hostRenderContract, hostRenderExplanation, hostRenderUpdateNotice, hostRenderView, jobFailureReason, readFailure, scienceView, signInNote } from "./claude-science-state.mjs";

const fixtures = createRequire(import.meta.url)("../scripts/claude-science-fixtures.json");
const body = (name) => fixtures[name].body;
const idle = { running: true, ready: true, job: { running: false } };
const view = (name, extra = {}) => scienceView({ data: body(name), ...idle, ...extra });
const actions = (v) => [v.primary, ...v.secondary].filter(Boolean).map((item) => item.action);
const enabled = (v) => [v.primary, ...v.secondary].filter((item) => item && !item.disabled).map((item) => item.action);

test("every fixture maps to a state with its actions", () => {
  const expected = {
    "unsupported-architecture": ["unsupported", []],
    "not-installed": ["not-installed", ["install"]],
    installing: ["installing", []],
    stopped: ["stopped", ["start"]],
    "running-signed-out": ["running", ["open", "stop"]],
    "running-signed-in": ["running", ["open", "stop"]],
    "running-host-render-available": ["running", ["open", "stop"]],
    "running-host-render": ["running", ["open", "stop"]],
    "running-host-render-elsewhere": ["running", ["open", "stop"]],
    "update-required": ["running", ["open", "stop"]],
    error: ["failed", ["start"]],
  };
  for (const name of Object.keys(fixtures)) {
    if (fixtures[name].status !== 200) continue;
    assert.ok(expected[name], "no expectation for fixture " + name);
    const v = view(name);
    assert.equal(v.state, expected[name][0], name);
    assert.deepEqual(actions(v), expected[name][1], name);
  }
  const unreachable = scienceView({ error: { status: fixtures.unreachable.status, message: fixtures.unreachable.text }, ...idle });
  assert.equal(unreachable.state, "unreachable");
  assert.equal(unreachable.lead, fixtures.unreachable.text);
  assert.deepEqual(actions(unreachable), ["refresh"]);
});

test("unsupported says why in the server's words and offers nothing to press", () => {
  const v = view("unsupported-architecture");
  assert.equal(v.lead, body("unsupported-architecture").unsupported_reason);
  assert.equal(v.primary, null);
  assert.deepEqual(v.secondary, []);
  assert.equal(v.drawing, "plug");
  // The answer needs no running environment, so it also shows while stopped.
  assert.equal(scienceView({ data: body("unsupported-architecture"), running: false }).state, "unsupported");
  const blank = scienceView({ data: { ...body("unsupported-architecture"), unsupported_reason: "" }, ...idle });
  assert.match(blank.lead, /暂不支持/);
});

test("not installed shows disk needed against disk free and installs", () => {
  const v = view("not-installed");
  assert.deepEqual(v.facts, [{ label: "需要空间", value: "至少 11 GB" }, { label: "当前可用", value: "38 GB", tone: "" }]);
  assert.equal(v.primary.label, "安装 Claude Science");
  assert.equal(v.primary.disabled, false);
  assert.equal(v.drawing, "tray");
  const short = scienceView({ data: { ...body("not-installed"), disk_free_mb: 4096 }, ...idle });
  assert.equal(short.primary.disabled, true);
  assert.equal(short.facts[1].tone, "warn");
  assert.match(short.hint, /可用空间不足/);
  // An unknown free size (omitted by the server) must not block the install.
  const unknown = scienceView({ data: { ...body("not-installed"), disk_free_mb: undefined }, ...idle });
  assert.equal(unknown.primary.disabled, false);
  assert.deepEqual(unknown.facts.map((fact) => fact.label), ["需要空间"]);
});

test("installing follows the operation and the job, with its log tail", () => {
  const bare = view("installing");
  assert.equal(bare.state, "installing");
  assert.match(bare.progress.message, /正在下载并安装/);
  assert.equal(bare.primary, null);
  const log = Array.from({ length: 9 }, (_, index) => "第 " + (index + 1) + " 行").join("\n") + "\n";
  const job = { id: "j1", kind: "claude-science-install", running: true, message: "正在下载 Claude Science 0.1.73…", log };
  const running = scienceView({ data: body("not-installed"), ...idle, job, pending: "install" });
  assert.equal(running.state, "installing");
  assert.equal(running.progress.message, job.message);
  assert.deepEqual(running.log, ["第 4 行", "第 5 行", "第 6 行", "第 7 行", "第 8 行", "第 9 行"]);
  // Before the status feed reports the job, the submitted action alone is enough.
  assert.equal(scienceView({ data: body("not-installed"), ...idle, pending: "install" }).state, "installing");
  assert.equal(scienceView({ data: body("stopped"), ...idle, pending: "start" }).state, "starting");
  assert.equal(scienceView({ data: body("running-signed-out"), ...idle, pending: "stop" }).state, "stopping");
  assert.equal(scienceView({ data: { ...body("stopped"), operation: "start" }, ...idle }).state, "starting");
});

test("a failed job shows the refusal reason, the last line of its log", () => {
  const job = { id: "j2", kind: "claude-science-install", running: false, error: "维护操作失败", log: "检查客机环境\n正在下载\nClaude Science 的沙箱需要 bubblewrap 0.8 或更高版本。\n" };
  const v = scienceView({ data: body("not-installed"), ...idle, job });
  assert.deepEqual(v.notices, [{ tone: "error", label: "安装未完成", text: "Claude Science 的沙箱需要 bubblewrap 0.8 或更高版本。", details: true }]);
  assert.equal(v.primary.action, "install");
  // A raw tool line is not a reason: fall back to the job's own error.
  assert.equal(jobFailureReason({ error: "维护操作失败", log: "curl: (6) Could not resolve host" }), "维护操作失败");
  assert.match(jobFailureReason({ error: "", log: "" }), /操作详情/);
  // Not while a new attempt is under way, and not for another feature's job.
  assert.deepEqual(scienceView({ data: body("not-installed"), ...idle, job, pending: "install" }).notices, []);
  assert.deepEqual(scienceView({ data: body("not-installed"), ...idle, job: { ...job, kind: "claude-desktop-install" } }).notices, []);
});

test("stopped starts; failed explains the reason and retries", () => {
  const stopped = view("stopped");
  assert.equal(stopped.primary.label, "启动");
  assert.deepEqual(stopped.chips, [{ text: "v0.1.73", tone: "" }]);
  assert.equal(stopped.tone, "idle");
  const reasons = {};
  for (const reason of ["port_in_use", "oom-kill", "exit", "something-new"]) {
    const v = scienceView({ data: { ...body("error"), failure_reason: reason }, ...idle });
    assert.equal(v.state, "failed");
    assert.equal(v.tone, "error");
    assert.equal(v.primary.label, "重试");
    reasons[reason] = v.lead;
  }
  assert.match(reasons.port_in_use, /端口/);
  assert.match(reasons["oom-kill"], /内存/);
  assert.match(reasons.exit, /意外退出/);
  assert.match(reasons["something-new"], /未能启动/);
  assert.equal(new Set(Object.values(reasons)).size, 4);
});

test("a running unit that does not answer yet is starting", () => {
  const v = scienceView({ data: { ...body("running-signed-out"), ready: false }, ...idle });
  assert.equal(v.state, "starting");
  assert.deepEqual(actions(v), ["stop"]);
  assert.equal(v.primary, null);
});

test("running opens, explains sign-in, and stops quietly", () => {
  const v = view("running-signed-out");
  assert.equal(v.primary.action, "open");
  assert.equal(v.primary.label, "打开 Claude Science");
  assert.equal(v.note, signInNote);
  assert.match(v.note, /隔离浏览器/);
  assert.match(v.note, /Claude Desktop/);
  assert.deepEqual(v.secondary.map((item) => [item.action, item.confirm]), [["stop", true]]);
  assert.equal(v.tone, "ok");
  assert.deepEqual(v.chips, [{ text: "v0.1.73", tone: "" }]);
  assert.deepEqual(view("running-signed-in").chips.map((chip) => chip.text), ["v0.1.73", "已登录"]);
  // Phase 1 reports signed_in as null: no sign-in claim either way.
  assert.deepEqual(view("running-signed-out").chips.map((chip) => chip.text), ["v0.1.73"]);
  // The browser must be ready for the tab to open.
  const early = scienceView({ data: body("running-signed-out"), ...idle, ready: false });
  assert.equal(early.primary.disabled, true);
  assert.match(early.hint, /浏览器/);
  assert.deepEqual(enabled(early), ["stop"]);
});

test("an update is offered next to the version, never in place of opening", () => {
  const data = { ...body("running-signed-in"), version: "0.1.72", update_available: true };
  const v = scienceView({ data, ...idle });
  assert.deepEqual(v.chips.map((chip) => chip.text), ["v0.1.72", "已登录", "有新版本"]);
  assert.equal(v.primary.action, "open");
  assert.deepEqual(v.secondary.map((item) => [item.action, item.label, item.confirm]), [["update", "更新到 0.1.73", true], ["stop", "停止", true]]);
  const stopped = scienceView({ data: { ...body("stopped"), version: "0.1.72", update_available: true }, ...idle });
  assert.equal(stopped.primary.action, "start");
  assert.deepEqual(stopped.secondary.map((item) => [item.action, item.confirm]), [["update", false]]);
});

test("a required update shows the server's hint and updates only when this build can", () => {
  const superCage = view("update-required");
  assert.equal(superCage.notices.length, 1);
  assert.equal(superCage.notices[0].text, body("update-required").update_hint);
  assert.equal(superCage.notices[0].tone, "warn");
  assert.ok(superCage.chips.some((chip) => chip.text === "需要更新"));
  assert.equal(superCage.tone, "ok");
  assert.equal(superCage.primary.action, "open");
  assert.ok(!actions(superCage).includes("update"));
  const here = scienceView({ data: { ...body("update-required"), update_via: "claude-science" }, ...idle });
  assert.equal(here.primary.action, "update");
  assert.deepEqual(here.secondary.map((item) => item.action), ["open", "stop"]);
});

test("another job, a stopped environment, a lock and guest errors keep the Claude Desktop patterns", () => {
  const elsewhere = scienceView({ data: body("stopped"), ...idle, job: { id: "x", kind: "settings", running: true, message: "正在保存设置" } });
  assert.equal(elsewhere.state, "stopped");
  assert.equal(elsewhere.progress.own, false);
  assert.match(elsewhere.progress.message, /正在保存设置/);
  assert.deepEqual(enabled(elsewhere), []);
  const down = scienceView({ data: body("stopped"), running: false, ready: false });
  assert.equal(down.state, "environment-stopped");
  assert.deepEqual(actions(down), []);
  assert.equal(scienceView({ running: false }).state, "environment-stopped");
  const stopped409 = scienceView({ error: { status: 409, message: "请先启动 Super Cage 环境" }, ...idle });
  assert.equal(stopped409.state, "environment-stopped");
  assert.equal(stopped409.lead, "请先启动 Super Cage 环境");
  const locked = scienceView({ error: { status: 423, message: "Super Cage 已锁定，请先登录" }, ...idle });
  assert.equal(locked.state, "locked");
  assert.equal(locked.lead, "Super Cage 已锁定，请先登录");
  assert.deepEqual(locked.notices, []);
  assert.equal(scienceView(idle).state, "loading");
  // A failed re-read keeps the last answer on screen and says so.
  const stale = scienceView({ data: body("stopped"), error: { status: 502, message: "读取失败" }, ...idle });
  assert.equal(stale.state, "stopped");
  assert.deepEqual(stale.notices, [{ tone: "error", text: "读取失败" }]);
  // A refused action (409 busy, 502 guest) is shown as well and nothing stays disabled.
  const refused = scienceView({ data: body("stopped"), actionError: "另一项操作正在进行", ...idle });
  assert.deepEqual(refused.notices, [{ tone: "error", text: "另一项操作正在进行" }]);
  assert.deepEqual(enabled(refused), ["start"]);
});

test("opening or any submitted action disables the others", () => {
  assert.deepEqual(enabled(scienceView({ data: body("running-signed-out"), ...idle, pending: "open" })), []);
  assert.deepEqual(readFailure(null), null);
  assert.equal(readFailure({ status: 502, message: "x" }).kind, "unreachable");
  assert.equal(readFailure(new Error("offline")).kind, "unreachable");
});

test("disk sizes read as GB or MB", () => {
  assert.equal(formatMB(11264), "11 GB");
  assert.equal(formatMB(38912), "38 GB");
  assert.equal(formatMB(1536), "1.5 GB");
  assert.equal(formatMB(512), "512 MB");
  assert.equal(formatMB(0), "");
  assert.equal(formatMB(undefined), "");
});

const windowsWindow = { desktop_gateway: true, mode: "local", client_platform: "windows" };
const render = (name, extra = {}) => hostRenderView({ data: body(name), client: windowsWindow, bridge: true, contract: hostRenderContract, running: true, ...extra });

test("host rendering: off by default, a quiet switch where it can be used", () => {
  for (const name of ["running-signed-in", "stopped", "not-installed"]) {
    assert.deepEqual(render(name), { toggle: null, frame: false, fallback: "", notice: "" }, name);
  }
  const available = render("running-host-render-available");
  assert.equal(available.frame, false);
  assert.deepEqual(available.toggle, { label: "本机渲染（实验性）", checked: false, disabled: false, explanation: hostRenderExplanation });
  // macOS, Linux, a browser, the phone, a remote host: nothing to switch on.
  for (const client of [{ ...windowsWindow, client_platform: "darwin" }, { ...windowsWindow, mode: "remote" }, { mode: "local", client_platform: "windows" }]) {
    assert.equal(hostRenderView({ data: body("running-host-render-available"), client, bridge: true, contract: hostRenderContract, running: true }).toggle, null);
  }
  assert.equal(render("running-host-render-available", { bridge: false }).toggle, null);
  assert.equal(render("running-host-render-elsewhere").toggle, null);
});

test("host rendering: the frame only while everything holds", () => {
  assert.equal(render("running-host-render").frame, true);
  assert.equal(render("running-host-render").toggle.checked, true);
  for (const [why, extra] of Object.entries({ "environment stopped": { running: false }, locked: { locked: true }, "job running": { busy: true }, "fell back": { fallback: "身份核对未通过" }, "no bridge": { bridge: false }, "remote host": { client: { ...windowsWindow, mode: "remote" } } })) {
    assert.equal(render("running-host-render", extra).frame, false, why);
  }
  const stopped = { ...body("running-host-render"), running: false, ready: false };
  assert.equal(hostRenderView({ data: stopped, client: windowsWindow, bridge: true, contract: hostRenderContract, running: true }).frame, false, "daemon stopped");
  // On but not usable here (the server refuses the switch from here too):
  // the pane stays as it was.
  for (const client of [{ ...windowsWindow, mode: "remote" }, { ...windowsWindow, client_platform: "linux" }, { mode: "local", client_platform: "windows" }]) {
    assert.deepEqual(render("running-host-render", { client }), { toggle: null, frame: false, fallback: "", notice: "" });
  }
  assert.equal(render("running-host-render", { fallback: "身份核对未通过" }).fallback, "身份核对未通过");
});

test("host rendering: offered only to a window whose bridge has the frame contract (F8)", () => {
  assert.equal(hostRenderContract, 2);
  // null: the probe has not answered; 0: the bridge has no scienceFrame
  // capabilities op (an older window); 1: overrides on the whole WebView.
  for (const contract of [null, 0, 1]) {
    assert.equal(render("running-host-render-available", { contract }).toggle, null, String(contract));
    assert.equal(render("running-host-render", { contract }).frame, false, String(contract));
  }
  assert.equal(hostRenderView({ data: body("running-host-render-available"), client: windowsWindow, bridge: true, running: true }).toggle, null, "no contract given");
  // Already on (set from a newer window): the switch stays so it can be
  // turned off, and says why there is no frame.
  const older = render("running-host-render", { contract: 1 });
  assert.equal(older.toggle?.checked, true);
  assert.equal(older.notice, hostRenderUpdateNotice);
  assert.deepEqual(render("running-host-render", { contract: null }), { toggle: null, frame: false, fallback: "", notice: "" });
});

test("host rendering: the toolbar says what the frame is doing", () => {
  const data = body("running-host-render");
  assert.deepEqual(hostFrameStatus(data, "preparing").text, "正在核对页面的时区与语言…");
  const ready = hostFrameStatus(data, "ready");
  assert.deepEqual(ready.chips.map((chip) => chip.text), ["v0.1.73", "已登录"]);
  assert.equal(ready.tone, "ok");
  const reported = hostFrameStatus({ ...data, host_render: { ...data.host_render, audit_hit: "timezone", audit_hits: 2 } }, "ready");
  assert.equal(reported.tone, "warn");
  assert.equal(reported.text, "页面发出过 2 次带有本机时区或语言的请求（已记录，未拦截）");
});
