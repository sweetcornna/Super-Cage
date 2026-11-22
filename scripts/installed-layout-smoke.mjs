// Regression smoke: the installed layout opens the local workbench.
//
// In an installed app the slim cage-attach sits beside cage (macOS: both in
// Super Cage.app/Contents/Resources; Windows: beside cage.exe), built with
// one stamp. A controller started from that layout names the companion in its
// /api/launch answer and not in its status answer, and the desktop once took
// the two for different controllers, so the window stopped at "本机后台身份已变化"
// (0a3e34b). Nothing ran the installed layout end to end. This does, with the
// real binaries, a temporary HOME and no VM:
//
//   1. a fresh layout: cage and cage-attach, one stamp, in a directory shaped
//      like the app (built with the Makefile's own flags, or copied from a
//      prebuilt layout);
//   2. the cheap CLI paths with nothing running;
//   3. cage desktop --json, what the window starts, then the page it loads, in
//      headless Chrome with the native bridge mirrored: the page enters the
//      local workbench by itself on a fresh launch;
//   4. the window's own requests: the read routes that its settings and pages
//      call (devices, other computers, about and updates, the SSH entry, the
//      terminal app, the agents list ...), asked with exactly what the page
//      holds (its session cookie from the launch ticket, and the headers the
//      UI's api() helper adds), none refused with 401 or 403 and none missing
//      (404); the agent transport's header on the same route is refused. Real
//      Go tests hand-set the cookie of the launcher's own jar and the UI
//      smokes fake the API, so a gate that demands what the webview never
//      holds (the trusted-view cookie) refused the real window unnoticed;
//   5. the picker's own choice of the local environment (candidate, commit).
//      Its disconnect releases the idle controller the gateway started, so the
//      choice again starts a second one. After each start the gateway has no
//      defunct child: the starter of a controller ends with it, and the
//      gateway lives on;
//   6. the local controller's launch answer names the companion and its
//      status answer does not (the layout really is the installed one), and
//      it is the instance the gateway reports;
//   7. cage attach and cage entry status against the running controller, and
//      a cage-attach of another build stamp refused;
//   8. every process stopped by the PID this run started, the ports free, the
//      temporary directories removed.
//
// The window's page stays on the desktop gateway's origin (the app only loads
// that origin) and the gateway proxies the local controller, so "reaches the
// local workbench" is the page's context saying local and connected, the
// workbench's own first screen showing, and no navigation leaving the gateway.
//
// Usage: node scripts/installed-layout-smoke.mjs [--no-browser] [LAYOUT_DIR]
//   LAYOUT_DIR (or CAGE_INSTALLED_LAYOUT) holds a built cage and cage-attach,
//   for example "Super Cage.app/Contents/Resources"; without it both are built
//   here (the workbench UI first, if internal/workbench/web has none).
//   CAGE_PLAYWRIGHT_MODULE and CAGE_TEST_BROWSER_PATH name Playwright and a
//   Chrome as for the workbench smokes in ui/scripts. --no-browser (or
//   CAGE_SMOKE_NO_BROWSER=1) drives the desktop gateway with the requests the
//   page makes instead of the page itself.
//   CAGE_SMOKE_KEEP=1 keeps the temporary directory for inspection.
// It starts no VM and no window, and never reads or writes the real profile:
// every Cage process gets a temporary HOME and a profile inside it.
import assert from "node:assert/strict";
import { execFile, execFileSync, spawnSync } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import { createRequire } from "node:module";
import { copyFile, chmod, mkdir, mkdtemp, readFile, realpath, rename, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { connectionHeaders } from "../ui/src/client-context.mjs";

const run = promisify(execFile);
const repo = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const args = process.argv.slice(2);
const noBrowser = args.includes("--no-browser") || process.env.CAGE_SMOKE_NO_BROWSER === "1";
const prebuilt = args.find((a) => !a.startsWith("--")) || process.env.CAGE_INSTALLED_LAYOUT || "";
const keep = process.env.CAGE_SMOKE_KEEP === "1";
if (process.platform === "win32") {
  console.error("installed-layout-smoke: Windows is not covered yet (POSIX signals and lsof).");
  process.exit(2);
}

const started = Date.now();
const procs = new Map(); // pid -> label: processes this run started
const ports = new Set();
let root = "", home = "", appDir = "", cageBin = "", attachBin = "", otherAttach = "", stamp = "";
let env = {}, desktopEnv = {};
let browser = null, windowPage = null;

async function step(name, fn) {
  const at = Date.now();
  console.log("... " + name);
  try {
    const value = await fn();
    console.log("ok  " + name + " (" + (Date.now() - at) + " ms)");
    return value;
  } catch (error) {
    console.error("FAIL " + name + " (" + (Date.now() - at) + " ms)");
    throw error;
  }
}
const exists = (p) => stat(p).then(() => true, () => false);
const sha256 = async (p) => createHash("sha256").update(await readFile(p)).digest("hex");
const readJSON = async (p) => JSON.parse(await readFile(p, "utf8"));
const alive = (pid) => { try { process.kill(pid, 0); return true; } catch { return false; } };
const commandOf = (pid) => spawnSync("ps", ["-p", String(pid), "-o", "command="], { encoding: "utf8" }).stdout.trim();
function cli(bin, argv, extraEnv = {}) {
  const r = spawnSync(bin, argv, { env: { ...env, ...extraEnv }, encoding: "utf8", timeout: 60_000, stdio: ["ignore", "pipe", "pipe"] });
  assert.ifError(r.error);
  return { code: r.status, out: r.stdout + r.stderr };
}

// ---- 1. the layout -------------------------------------------------------
// The Makefile's own variables (its tags, version, lima version and ldflags),
// read through a one-rule makefile so that nothing in the repository is built
// or overwritten; the stamp is given on its command line, as make cage-attach
// gets it from make cage.
async function makeFlags(stampValue) {
  const fragment = path.join(root, "flags.mk");
  const variables = ["GO", "CGO_ENABLED", "TAGS", "LDFLAGS", "EXE", "GOOS"];
  await writeFile(fragment, ".PHONY: installed-layout-flags\ninstalled-layout-flags:\n" + variables.map((v) => "\t@echo \"" + v + "=$(" + v + ")\"\n").join(""));
  const text = execFileSync("make", ["-s", "-f", "Makefile", "-f", fragment, "STAMP=" + stampValue, "installed-layout-flags"], { cwd: repo, encoding: "utf8" });
  return Object.fromEntries(text.trim().split("\n").map((line) => [line.slice(0, line.indexOf("=")), line.slice(line.indexOf("=") + 1)]));
}
function goBuild(flags, target, out, { cgo }) {
  const argv = ["build", "-trimpath", ...(cgo ? ["-tags", flags.TAGS] : []), "-ldflags", flags.LDFLAGS, "-o", out, target];
  const r = spawnSync(flags.GO, argv, { cwd: repo, env: { ...process.env, CGO_ENABLED: cgo ? flags.CGO_ENABLED : "0" }, encoding: "utf8", timeout: 20 * 60_000 });
  assert.ifError(r.error);
  assert.equal(r.status, 0, "go build " + target + " failed:\n" + r.stdout + r.stderr);
}
async function buildLayout() {
  if (!(await exists(path.join(repo, "internal/workbench/web/index.html")))) {
    // As make cage orders it: the workbench is embedded, so it is built first.
    console.log("    internal/workbench/web has no built workbench: make cage-ui");
    const r = spawnSync("make", ["cage-ui"], { cwd: repo, stdio: "inherit", timeout: 20 * 60_000 });
    assert.equal(r.status, 0, "make cage-ui failed");
  }
  stamp = randomBytes(12).toString("hex");
  const flags = await makeFlags(stamp);
  goBuild(flags, "./cmd/cage", cageBin, { cgo: true });
  if (flags.GOOS === "darwin") {
    execFileSync("codesign", ["-f", "--entitlements", path.join(repo, "third_party/lima/vz.entitlements"), "-s", "-", cageBin], { cwd: repo, stdio: "pipe" });
  }
  goBuild(flags, "./cmd/cage-attach", attachBin, { cgo: false });
}
async function otherStampAttach() {
  // The same program of another build: refused by a controller of this one.
  let other;
  do { other = randomBytes(12).toString("hex"); } while (other === stamp);
  goBuild(await makeFlags(other), "./cmd/cage-attach", otherAttach, { cgo: false });
}

// ---- the desktop gateway, as the page and the picker use it ---------------
async function request(method, url, body, cookie = "", headers = {}) {
  const origin = new URL(url).origin;
  const res = await fetch(url, {
    method, signal: AbortSignal.timeout(15_000),
    headers: { Origin: origin, ...(body === undefined ? {} : { "Content-Type": "application/json" }), ...(cookie ? { Cookie: cookie } : {}), ...headers },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let json;
  try { json = JSON.parse(text); } catch { /* not JSON */ }
  return { status: res.status, text, json, headers: res.headers };
}
async function gatewaySession(entryURL) {
  const u = new URL(entryURL);
  const res = await request("POST", u.origin + "/api/session", { token: u.hash.slice(1) });
  assert.equal(res.status, 200, "the launch ticket was refused: " + res.text);
  const cookie = res.headers.getSetCookie().map((c) => c.split(";")[0]).join("; ");
  return { origin: u.origin, get: (p, headers) => request("GET", u.origin + p, undefined, cookie, headers), post: (p, b, headers) => request("POST", u.origin + p, b, cookie, headers) };
}
// The picker's choice of 本机: a candidate, then its commit, as requestTarget
// and verified do. Throws the candidate's own message when it fails.
async function chooseLocal(gw) {
  let context = (await gw.get("/api/client/context")).json;
  if (context.mode === "local") {
    const left = await gw.post("/api/client/disconnect", { connection_id: context.connection_id });
    assert.equal(left.status, 200, "disconnect: " + left.text);
    context = left.json;
  }
  const pending = await gw.post("/api/client/candidate", { connection_id: context.connection_id, mode: "local" });
  assert.equal(pending.status, 202, "candidate: " + pending.text);
  for (let i = 0; i < 120; i++) {
    context = (await gw.get("/api/client/context")).json;
    const state = context.candidate?.connection;
    if (state?.state === "failed") throw new Error("the local candidate failed: " + state.message);
    if (state?.state === "connected") break;
    await sleep(250);
  }
  assert.equal(context.candidate?.connection?.state, "connected", "the local candidate never connected");
  const done = await gw.post("/api/client/commit", { connection_id: context.connection_id, candidate_id: context.candidate.id, local_action: "auto" });
  assert.equal(done.status, 200, "commit: " + done.text);
  return done.json;
}
function assertLocalConnected(context, where) {
  assert.equal(context.desktop_gateway, true, where + ": not the desktop gateway");
  assert.equal(context.mode, "local", where + ": mode " + context.mode);
  assert.equal(context.connection?.state, "connected", where + ": " + JSON.stringify(context.connection));
  assert.match(context.instance_id, /^[0-9a-f]{64}$/, where + ": instance");
  assert.equal(context.connection.manifest?.platform, process.platform, where + ": manifest platform");
}

// ---- the window's own requests -----------------------------------------------
// The read routes that the window's settings and pages call, as the UI names
// them (the path after /api), listed from the api() calls of ui/src:
// about-settings and app-updates (about, updates), devices-page and
// devices-dialogs (设备, 加入其他电脑), other-clients-* (agents), local-terminal-*
// and local-entry (terminal app, SSH entry), settings, remote-hosts (the
// picker), project-dialog and browser-extensions (the client's own stores).
// Only reads: nothing here changes a setting, mints a code or opens a window.
const windowReads = [
  // Answered by the desktop gateway itself; the UI names no connection for them.
  "/client/context", "/client/about", "/client/local-status", "/client/hosts",
  "/client/devices", "/client/devices/join?wait=0",
  "/client/local-terminal", "/client/local-terminal/app",
  "/client/browser-extensions", "/client/project-shares",
  // Handed on to the local controller, which the UI names in X-Cage-Connection.
  "/status", "/account", "/settings", "/devices", "/devices/invite?wait=0", "/mobile-pair",
  "/agents", "/app-update", "/app-update/interruptions", "/entry", "/entry/include",
];
// A 404 is a route that is not there, which the page shows as a broken screen.
// The desktop gateway and the local controller carry every route above (the
// SSH entry and devices routes answer 404 only in a gateway-only or client-only
// process, the terminal app's only outside the desktop), so none is documented
// as absent in this process kind. A route that is missing by design goes into
// this map: route -> the reason.
const windowAbsent = new Map();
// The window holds a session cookie from its launch ticket and nothing more:
// the trusted-view cookie lives only in the cookie jars of the Go launchers.
// ask(path, headers) sends one GET /api + path with that and returns its status.
async function assertWindowRoutesAnswered(ask) {
  const context = JSON.parse((await ask("/client/context", {})).text);
  assertLocalConnected(context, "the window's context");
  const rows = [];
  for (const route of windowReads) {
    const answer = await ask(route, connectionHeaders(route, context));
    rows.push({ route, status: answer.status, text: answer.text });
    console.log("    GET /api" + route.padEnd(32) + " " + answer.status);
  }
  // 401 and 403 refuse the window; 404 is a route that is not there. 409, 423
  // and 503 are fine: no VM runs, the account of a fresh profile is not set up.
  const refused = rows.filter((r) => r.status === 401 || r.status === 403);
  const missing = rows.filter((r) => r.status === 404 && !windowAbsent.has(r.route));
  const describe = (r) => "GET /api" + r.route + " -> " + r.status + " " + r.text.slice(0, 120).replace(/\s+/g, " ");
  assert.deepEqual(refused.map(describe), [], "the window's own requests are refused");
  assert.deepEqual(missing.map(describe), [], "the window's own requests find no route");
  // The reverse, once: what the agent transport sends stays refused on a route
  // that the window may use.
  const agent = await ask("/client/devices", { "X-Cage-Agent": "1" });
  assert.equal(agent.status, 403, "GET /api/client/devices with X-Cage-Agent: " + agent.status + " " + agent.text.slice(0, 120));
  console.log("    GET /api" + "/client/devices with X-Cage-Agent: 1".padEnd(32) + " " + agent.status);
}

// What Cage.swift does for the window: cage desktop --json with the window's pid.
async function windowEntry() {
  const { stdout } = await run(cageBin, ["desktop", "--json"], { env: desktopEnv, timeout: 90_000 });
  const entry = JSON.parse(stdout);
  assert.equal(entry.status, "ready", stdout);
  assert.match(entry.url, /^http:\/\/localhost:\d+\/\?launch=[0-9a-f]+#[0-9a-f]+$/, "the window's entry URL");
  return entry;
}

// A child of pid that ended and was never waited for: a zombie, state Z in ps.
function defunctChildren(pid) {
  const r = spawnSync("ps", ["-axo", "pid=,ppid=,stat=,command="], { encoding: "utf8" });
  assert.ifError(r.error);
  return r.stdout.split("\n").map((line) => line.trim().match(/^(\d+)\s+(\d+)\s+(\S+)\s*(.*)$/))
    .filter((m) => m && Number(m[2]) === pid && m[3].startsWith("Z")).map((m) => m[1] + " " + m[3] + " " + m[4]);
}
// The gateway lives on after it starts a local controller, so whatever ends
// under it has to be waited for. The starter of a controller ends with it:
// the gateway gets a moment to wait for that, no more.
async function assertNoDefunctChild(gatewayPid, where) {
  let zombies = defunctChildren(gatewayPid);
  for (let i = 0; i < 30 && zombies.length; i++) {
    await sleep(100);
    zombies = defunctChildren(gatewayPid);
  }
  assert.deepEqual(zombies, [], "the gateway (pid " + gatewayPid + ") keeps defunct children " + where + ":\n" + zombies.join("\n"));
}

// ---- stopping what this run started ---------------------------------------
async function stopPid(pid, label) {
  if (!alive(pid)) return;
  const command = commandOf(pid);
  // A child that ended and was not waited for is a zombie: gone for our purposes.
  if (command === "" || command === "<defunct>") return;
  if (!command.includes(appDir)) return console.log("    pid " + pid + " (" + label + ") is no longer ours: " + command);
  process.kill(pid, "SIGTERM");
  for (let i = 0; i < 100 && alive(pid); i++) await sleep(100);
  if (alive(pid) && commandOf(pid) === command) {
    console.log("    pid " + pid + " (" + label + ") ignored SIGTERM; SIGKILL");
    process.kill(pid, "SIGKILL");
    for (let i = 0; i < 50 && alive(pid); i++) await sleep(100);
  }
}
async function noteControllers() {
  for (const [file, label] of [[path.join(home, ".claude-cage", "control.json"), "local controller"], [path.join(home, ".claude-cage", "desktop-entry", "control.json"), "desktop gateway"]]) {
    try {
      const record = await readJSON(file);
      ports.add(record.port);
      if (!procs.has(record.pid)) procs.set(record.pid, label);
      // cage log-relay controller stands between the gateway and a controller it started.
      const parent = Number(spawnSync("ps", ["-p", String(record.pid), "-o", "ppid="], { encoding: "utf8" }).stdout.trim());
      if (parent > 1 && commandOf(parent).includes(appDir + path.sep + "cage log-relay") && !procs.has(parent)) procs.set(parent, "log relay of the " + label);
    } catch { /* not there (yet) */ }
  }
}
async function cleanup(failed) {
  const problems = [];
  try { await browser?.close(); } catch { /* already gone */ }
  if (home) await noteControllers();
  // The local controller (and the relay in front of it) first, then the gateway.
  const order = [...procs].sort(([, a], [, b]) => (a.includes("gateway") ? 1 : 0) - (b.includes("gateway") ? 1 : 0));
  for (const [pid, label] of order) await stopPid(pid, label);
  for (const [pid, label] of procs) if (alive(pid) && commandOf(pid).includes(appDir)) problems.push("still running: pid " + pid + " (" + label + ")");
  for (const port of ports) {
    const r = spawnSync("lsof", ["-nP", "-iTCP:" + port, "-sTCP:LISTEN"], { encoding: "utf8" });
    if (r.stdout.trim()) problems.push("port " + port + " is still listened on:\n" + r.stdout);
  }
  if (root) {
    // Anything left that runs from this run's own directory is a leak.
    const left = spawnSync("ps", ["-axo", "pid=,command="], { encoding: "utf8" }).stdout.split("\n").filter((l) => l.includes(root));
    if (left.length) problems.push("processes left from " + root + ":\n" + left.join("\n"));
    if (failed && home) {
      for (const log of [path.join(home, ".claude-cage", "logs", "events.jsonl"), path.join(home, ".claude-cage", "desktop-entry", "logs", "events.jsonl")]) {
        const tail = (await readFile(log, "utf8").catch(() => "")).trim().split("\n").slice(-6).join("\n");
        if (tail) console.error("--- " + log.replace(root, "<tmp>") + "\n" + tail);
      }
    }
    if (keep) console.log("kept " + root);
    else await rm(root, { recursive: true, force: true });
  }
  return problems;
}

let cleaning = null;
const finish = (failed) => (cleaning ||= cleanup(failed));
for (const signal of ["SIGINT", "SIGTERM"]) process.on(signal, () => finish(true).finally(() => process.exit(130)));

let failure = null;
try {
  root = await realpath(await mkdtemp(path.join(tmpdir(), "cage-layout-")));
  home = path.join(root, "home");
  appDir = process.platform === "darwin" ? path.join(root, "Super Cage.app", "Contents", "Resources") : path.join(root, "app");
  cageBin = path.join(appDir, "cage");
  attachBin = path.join(appDir, "cage-attach");
  otherAttach = path.join(root, "other-stamp", "cage-attach");
  await Promise.all([appDir, path.dirname(otherAttach), path.join(root, "tmp"), path.join(root, "browser-home"), home].map((d) => mkdir(d, { recursive: true })));
  const profile = path.join(home, ".claude-cage");
  // Everything a Cage process may find a home in is inside the temporary root;
  // nothing of the caller's environment (proxies, tokens, profiles) is passed.
  env = {
    PATH: "/usr/bin:/bin:/usr/sbin:/sbin", HOME: home, TMPDIR: path.join(root, "tmp"), LANG: "en_US.UTF-8",
    XDG_CONFIG_HOME: path.join(home, ".config"), XDG_CACHE_HOME: path.join(home, ".cache"), XDG_DATA_HOME: path.join(home, ".local", "share"), XDG_STATE_HOME: path.join(home, ".local", "state"),
    CLAUDE_CAGE_CONFIG: path.join(profile, "config.json"),
  };
  // The window pins its profile and tells cage desktop its own pid; this
  // process stands in for the window, so a gateway outlives it by a second at most.
  desktopEnv = { ...env, CAGE_DESKTOP_OWNER_PID: String(process.pid) };

  await step("a layout with cage-attach beside cage, one stamp", async () => {
    if (prebuilt) {
      const dir = path.resolve(prebuilt);
      for (const name of ["cage", "cage-attach"]) {
        assert.ok(await exists(path.join(dir, name)), dir + " has no " + name);
        await copyFile(path.join(dir, name), path.join(appDir, name));
        await chmod(path.join(appDir, name), 0o755);
      }
    } else {
      await buildLayout();
    }
    await otherStampAttach();
    assert.equal(path.dirname(cageBin), path.dirname(attachBin));
    const version = cli(cageBin, ["version"]);
    assert.equal(version.code, 0, version.out);
    if (!prebuilt) {
      const verify = cli(cageBin, ["verify-web-assets"]);
      assert.equal(verify.code, 0, "the embedded workbench is damaged: " + verify.out);
    }
  });

  await step("nothing running: cage attach, cage-attach and cage entry status refuse politely", async () => {
    for (const [label, bin, argv] of [["cage attach", cageBin, ["attach"]], ["cage-attach", attachBin, []], ["cage entry status", cageBin, ["entry", "status"]]]) {
      const r = cli(bin, argv);
      assert.equal(r.code, 1, label + " exited " + r.code + ": " + r.out);
      assert.match(r.out, /没有在运行/, label + ": " + r.out);
      assert.doesNotMatch(r.out, /goroutine|panic|\.go:\d+/, label + " crashed: " + r.out);
    }
    assert.ok(!(await exists(profile)), "a command that only looks for a controller created the profile");
  });

  const entry = await step("cage desktop --json, as the window starts it", async () => {
    const result = await windowEntry();
    procs.set(result.controller.pid, "desktop gateway");
    await noteControllers();
    const record = await readJSON(path.join(profile, "desktop-entry", "control.json"));
    assert.equal(record.pid, result.controller.pid, "the gateway is not the process that published its record");
    assert.ok(result.controller.stamp && (!stamp || result.controller.stamp === stamp), "the gateway's build stamp: " + result.controller.stamp);
    // The gateway has cage-attach beside it: the launch identity the window gets names it.
    assert.ok(result.controller.attach, "the gateway's identity names no companion: the layout is not the installed one");
    assert.equal(result.controller.attach.path, attachBin);
    assert.equal(result.controller.attach.sha256, await sha256(attachBin));
    return result;
  });

  const page = { errors: [], navigations: [] };
  if (!noBrowser) {
    await step("the window's page enters the local workbench (headless Chrome)", async () => {
      let engines;
      try { engines = createRequire(import.meta.url)(process.env.CAGE_PLAYWRIGHT_MODULE || "playwright"); }
      catch (e) { throw new Error("Playwright is not available (" + e.message.split("\n")[0] + "). Set CAGE_PLAYWRIGHT_MODULE and CAGE_TEST_BROWSER_PATH as for the workbench smokes in ui/scripts, or pass --no-browser."); }
      browser = await engines[process.env.CAGE_TEST_BROWSER || "chromium"].launch({
        headless: true, executablePath: process.env.CAGE_TEST_BROWSER_PATH || undefined,
        env: { PATH: env.PATH, HOME: path.join(root, "browser-home"), TMPDIR: env.TMPDIR, LANG: env.LANG },
      });
      const context = await browser.newContext({ viewport: { width: 1280, height: 850 } });
      const tab = await context.newPage();
      const origin = new URL(entry.url).origin;
      tab.on("console", (m) => { if (m.type() === "error") page.errors.push("console: " + m.text() + " " + m.location().url); });
      tab.on("pageerror", (e) => page.errors.push("pageerror: " + e.message));
      // A diagnostics report in flight when the page moves from its launch URL
      // to the workbench is cancelled by that navigation; nothing else may fail.
      tab.on("requestfailed", (r) => {
        if (r.method() === "POST" && r.url() === origin + "/api/diagnostics/events" && r.failure()?.errorText === "net::ERR_ABORTED") return;
        page.errors.push("request failed: " + r.method() + " " + r.url() + " " + r.failure()?.errorText);
      });
      tab.on("response", (r) => { if (r.status() >= 400) page.errors.push("HTTP " + r.status() + " " + r.request().method() + " " + r.url()); });
      tab.on("framenavigated", (f) => { if (f === tab.mainFrame()) page.navigations.push(f.url()); });
      // The WKWebView's "cage" message handler, as Cage.swift answers the calls
      // the first screens make: the window is ready, the appearance is applied,
      // a session is renewed by asking cage desktop --json again. The Keychain
      // is not mirrored (the account's device login answers "unsupported") and
      // neither are the file panels: a call for one is refused.
      const nativeCalls = [];
      await tab.exposeFunction("cageNativeRequest", async (body) => {
        nativeCalls.push(body.action);
        if (body.action === "workbenchReady" || body.action === "appearance") return true;
        if (body.action === "accountDevice") return { supported: false };
        if (body.action === "refreshSession") return { url: (await windowEntry()).url };
        throw new Error("未知操作");
      });
      await tab.addInitScript(() => {
        window.webkit = { messageHandlers: { cage: { postMessage: (body) => window.cageNativeRequest(body) } } };
        window.cageNativeIMEAvailable = true;
      });
      await tab.goto(entry.url, { waitUntil: "domcontentloaded" });
      let context1;
      for (let i = 0; i < 160; i++) {
        await noteControllers();
        context1 = await tab.evaluate(() => fetch("/api/client/context").then((r) => r.json())).catch(() => null);
        const candidate = context1?.candidate?.connection;
        if (candidate?.state === "failed") throw new Error("the page could not enter the local workbench: " + candidate.message);
        if (context1?.mode === "local" && context1.connection?.state === "connected") break;
        await sleep(250);
      }
      assert.ok(context1, "the page never answered");
      assertLocalConnected(context1, "page context");
      // The first local workbench document: not the environment picker, no
      // refusal, a first-run (or locked) account screen of the local controller.
      await tab.waitForSelector(".client-workbench", { timeout: 20_000 });
      await tab.waitForFunction(() => /注册 Super Cage 账户|解锁/.test(document.body.innerText), null, { timeout: 20_000 });
      const text = await tab.evaluate(() => document.body.innerText);
      assert.doesNotMatch(text, /本机后台身份已变化|请重新选择|本机工作台连接未完成/, "the page shows a refusal: " + text.slice(0, 300));
      const account = await tab.evaluate((id) => fetch("/api/account", { headers: { "X-Cage-Connection": id } }).then(async (r) => ({ status: r.status, body: await r.json() })), context1.connection_id);
      assert.equal(account.status, 200, "the local workbench's account answer: " + JSON.stringify(account));
      await sleep(500); // late errors of the first screens
      for (const url of page.navigations) assert.ok(url.startsWith(origin + "/"), "the window left the gateway origin: " + url);
      assert.deepEqual(page.errors, [], "errors in the page:\n" + page.errors.join("\n"));
      assert.ok(nativeCalls.includes("workbenchReady"), "the page never told the window it was ready: " + nativeCalls);
      windowPage = tab; // kept open for the next step; cleanup closes the browser on a failure
    });
    await step("the window's own requests are answered (from the page: its session cookie and the UI's headers)", async () => {
      await assertWindowRoutesAnswered(async (route, headers) => windowPage.evaluate(async ({ route, headers }) => {
        const response = await fetch("/api" + route, { credentials: "same-origin", headers });
        return { status: response.status, text: await response.text() };
      }, { route, headers }));
      await browser.close();
      browser = null;
      windowPage = null;
    });
  }

  const gw = await step("the window's entry is redeemed for a gateway session", async () => gatewaySession((await windowEntry()).url));
  if (noBrowser) {
    // Without the page, the picker's choice is what starts the local controller.
    await step("the picker's choice of the local environment starts the local controller", async () => {
      assertLocalConnected(await chooseLocal(gw), "first choice");
      await noteControllers();
    });
    // The page's requests, made with the page's credentials: the session cookie of a redeemed launch ticket.
    await step("the window's own requests are answered (the gateway session's cookie and the UI's headers)", () => assertWindowRoutesAnswered((route, headers) => gw.get("/api" + route, headers)));
  }
  await step("the gateway has no defunct child while the local controller runs", () => assertNoDefunctChild(entry.controller.pid, "with the local controller running"));

  let localContext;
  await step("the picker's choice again: disconnect releases the local controller and a second one starts (candidate, commit)", async () => {
    const context = await chooseLocal(gw);
    assertLocalConnected(context, "after the choice");
    localContext = context;
    const local = await gw.get("/api/client/local-status");
    assert.equal(local.status, 200, local.text);
    assert.equal(local.json.available, true);
    assert.equal(local.json.known, true);
    assert.equal(local.json.service_running, true, "the gateway does not see the local controller it connected to: " + local.text);
    // A request for the workbench itself names the connection it is for.
    const account = await gw.get("/api/account", { "X-Cage-Connection": context.connection_id });
    assert.equal(account.status, 200, "account through the gateway: " + account.text);
  });
  await noteControllers();
  await step("the gateway has no defunct child after the controller it released started again", () => assertNoDefunctChild(entry.controller.pid, "after the first local controller ended"));

  await step("the local controller names the companion in its launch answer, and not in its status answer", async () => {
    const record = await readJSON(path.join(profile, "control.json"));
    ports.add(record.port);
    const origin = "http://localhost:" + record.port;
    const launch = await request("POST", origin + "/api/launch", { token: record.token });
    const status = await request("POST", origin + "/api/desktop-local/status", { token: record.token });
    assert.equal(launch.status, 200, launch.text);
    assert.equal(status.status, 200, status.text);
    const named = launch.json.controller;
    assert.ok(named.attach, "the launch answer names no companion: the layout is not the installed one");
    assert.equal(named.attach.path, attachBin);
    assert.equal(named.attach.sha256, await sha256(attachBin));
    assert.equal(named.pid, record.pid);
    const bare = status.json.identity;
    assert.equal(bare.attach, undefined, "the status answer names the companion; the bug's precondition is gone");
    for (const field of ["version", "build", "protocol", "pid", "stamp"]) assert.equal(named[field], bare[field], "identity " + field);
    assert.ok(!launch.json.desktop_gateway, "the workload controller claims to be the gateway");
    // The gateway's "local, connected" is this controller: its own session
    // answers with the instance the gateway reports.
    const own = new URL(launch.json.url);
    const session = await request("POST", origin + "/api/session", { token: own.hash.slice(1) });
    assert.equal(session.status, 200, session.text);
    const cookie = session.headers.getSetCookie().map((c) => c.split(";")[0]).join("; ");
    const manifest = await request("GET", origin + "/api/remote/manifest", undefined, cookie);
    assert.equal(manifest.status, 200, manifest.text);
    assert.equal(manifest.json.instance_id, localContext.instance_id, "the gateway's local workbench is another instance than the local controller");
    assert.equal(manifest.json.origin, origin);
  });

  await step("the window's second entry reuses the same gateway", async () => {
    assert.equal((await windowEntry()).controller.pid, entry.controller.pid);
  });

  await step("with a controller running: cage attach and cage entry status answer", async () => {
    const attach = cli(cageBin, ["attach", "--terminal", "0123456789abcdef"]);
    // Past the identity checks (the companion is this very file), the first
    // thing it meets is the account lock of a fresh profile.
    assert.equal(attach.code, 3, "cage attach: " + attach.out);
    assert.match(attach.out, /已锁定/, attach.out);
    assert.doesNotMatch(attach.out, /不是同一个版本|没有在运行/, attach.out);
    const direct = cli(attachBin, ["--terminal", "0123456789abcdef"]);
    assert.equal(direct.code, 3, "cage-attach: " + direct.out);
    const status = cli(cageBin, ["entry", "status"]);
    assert.equal(status.code, 1, "cage entry status: " + status.out);
    assert.match(status.out, /已锁定/, status.out);
    assert.doesNotMatch(status.out, /没有在运行|不是同一个版本/, status.out);
  });

  await step("a cage-attach of another stamp is refused, beside cage and elsewhere", async () => {
    const refusal = /不是同一个版本/;
    const elsewhere = cli(otherAttach, ["--terminal", "0123456789abcdef"]);
    assert.equal(elsewhere.code, 1, elsewhere.out);
    assert.match(elsewhere.out, refusal, elsewhere.out);
    // In place, so that only the stamp differs from the real companion: cage
    // hands its attach to the program beside it, and the controller refuses it.
    const original = path.join(root, "other-stamp", "cage-attach.original");
    const staged = attachBin + ".staged";
    await rename(attachBin, original);
    try {
      await copyFile(otherAttach, staged);
      await chmod(staged, 0o755);
      await rename(staged, attachBin);
      const beside = cli(cageBin, ["attach", "--terminal", "0123456789abcdef"]);
      assert.equal(beside.code, 1, "cage attach with another stamp beside it: " + beside.out);
      assert.match(beside.out, refusal, "cage did not hand its attach to the program beside it: " + beside.out);
    } finally {
      await rm(staged, { force: true });
      await rename(original, attachBin);
    }
    const again = cli(cageBin, ["attach", "--terminal", "0123456789abcdef"]);
    assert.equal(again.code, 3, "cage attach after the companion was put back: " + again.out);
  });
} catch (error) {
  failure = error;
}

const problems = await finish(Boolean(failure));
if (failure) {
  console.error("FAIL installed-layout-smoke: " + (failure.stack || failure));
  for (const p of problems) console.error("also: " + p);
  process.exit(1);
}
if (problems.length) {
  for (const p of problems) console.error("FAIL cleanup: " + p);
  process.exit(1);
}
console.log("PASS installed-layout-smoke (" + ((Date.now() - started) / 1000).toFixed(1) + " s)" + (noBrowser ? ", without the browser" : ""));
