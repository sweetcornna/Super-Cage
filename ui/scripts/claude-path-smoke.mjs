// Guarantee G (design 9.1): for a Claude-only environment, the window does not
// get heavier or different because other clients can be added. The same
// scenario runs on the base commit (record) and on the branch (compare):
//   - the DOM of the Claude surfaces (sidebar, its navigation, the 新建 menu,
//     a Claude terminal tab, the home page stopped and running, the first-run
//     page, the settings navigation), normalized, in both themes at 1440x900;
//   - a screenshot of each state, byte for byte (fonts loaded, animations off,
//     the clock fixed), where the state has no allowed difference;
//   - every /api request from load to 60 s idle, then opening 新建 and a
//     Claude terminal, with its method, path and query, as multisets per phase
//     (a request that repeats three or more times over the path is a poll: per
//     phase it may differ by two, because a timer decides where it falls).
// The allowed differences are the closed list ALLOWED below and nothing else:
// anything else fails. Needs a Vite dev server (CAGE_UI_URL) on the tree under
// test, and for compare a baseline recorded from the base commit's tree.
//
//   git worktree add .claude/worktrees/g-base <base commit>      (once)
//   (in g-base/ui, dev server on 5361)  node scripts/claude-path-smoke.mjs record <dir>
//   (in this tree, dev server on 5360)  node scripts/claude-path-smoke.mjs compare <dir>
//
// CAGE_IDLE_MS shortens the 60 s idle phase (the report says so); record and
// compare must use the same value. The branch's own captures are kept in
// output/claude-path/ for review.
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { createWorkbench } from "./workbench-fixture.mjs";

const [mode = "compare", baselineArgument] = process.argv.slice(2);
assert.ok(["record", "compare"].includes(mode), "usage: claude-path-smoke.mjs record|compare <baseline directory>");
const idleMs = Number(process.env.CAGE_IDLE_MS || 60000);
const project = "/Users/shihao/.claude-cage-workspace";
const viewport = { width: 1440, height: 900 };
const fixedTime = new Date("2026-06-15T10:00:00+08:00");
const own = fileURLToPath(new URL("../../output/claude-path/", import.meta.url));
const target = mode === "record" ? path.resolve(baselineArgument || own) : own;
const baselineDirectory = mode === "compare" ? path.resolve(baselineArgument || process.env.CAGE_CLAUDE_PATH_BASELINE || "") : "";
if (mode === "compare") assert.ok(baselineArgument || process.env.CAGE_CLAUDE_PATH_BASELINE, "compare needs the baseline directory");
await fs.rm(target, { recursive: true, force: true });
await fs.mkdir(target, { recursive: true });

// The closed list of allowed differences (design 9.1). A node of these kinds is
// removed from the DOM on both sides before they are compared, and a state
// that had one is not compared pixel for pixel.
//   codex-menu-row     the Codex row of the 新建 menu (choice C)
//   codex-accounts-link the Codex 账号管理 link of the sidebar (choice C)
//   other-clients-nav  the settings navigation gains 其他客户端
// Requests: GET /api/codex-accounts may be gone, the session list's providers
// query may read claude instead of claude,codex (choice C), and opening the
// settings may make one plain GET /api/agents (the navigation row asks whether
// anything can be added); /api/agents anywhere else fails.
const ALLOWED = [
  { name: "codex-menu-row", find: (root) => [...root.querySelectorAll('[role="menuitem"]')].filter((node) => node.textContent.trim().startsWith("Codex")) },
  { name: "codex-accounts-link", find: (root) => [...root.querySelectorAll("button, a")].filter((node) => node.textContent.trim() === "Codex 账号管理") },
  { name: "other-clients-nav", find: (root) => [...root.querySelectorAll("button")].filter((node) => node.closest('nav[aria-label="设置分组"]') && node.textContent.trim() === "其他客户端") },
];

// Runs in the page: the normalized markup of a surface with the allowed nodes removed.
function surfaceOf({ selector, allowed }) {
  const root = document.querySelector(selector);
  if (!root) return { missing: true };
  const clone = root.cloneNode(true);
  let removed = 0;
  for (const { find } of allowed.map((item) => ({ find: new Function("root", "return (" + item + ")(root)") }))) {
    for (const node of find(clone)) {
      const parent = node.parentElement;
      node.remove();
      removed++;
      if (parent && parent !== clone && !parent.children.length && !parent.textContent.trim()) parent.remove();
    }
  }
  const html = clone.outerHTML.replace(/:r[0-9a-z]+:/g, ":rX:").replace(/\s+/g, " ");
  return { html, removed };
}

const sha = (value) => createHash("sha256").update(value).digest("hex");
const states = {};
const phases = {};

async function settle(page) {
  await page.evaluate(async () => { await document.fonts.ready; await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))); });
  await page.waitForTimeout(350);
}
async function remember(page, theme, name, selectors) {
  await settle(page);
  const key = theme + "/" + name;
  const surfaces = {};
  for (const [surface, selector] of Object.entries(selectors)) {
    surfaces[surface] = await page.evaluate(surfaceOf, { selector, allowed: ALLOWED.map((item) => item.find.toString()) });
  }
  const png = await page.screenshot({ animations: "disabled", caret: "initial" });
  const file = key.replace("/", "-") + ".png";
  await fs.writeFile(path.join(target, file), png);
  states[key] = { surfaces, png: file, pngHash: sha(png) };
}
const logger = (page, phase) => {
  const list = (phases[phase] ||= []);
  const listener = (request) => {
    const url = new URL(request.url());
    if (url.pathname.startsWith("/api/")) list.push(request.method() + " " + url.pathname + url.search);
  };
  page.on("request", listener);
  return () => page.off("request", listener);
};
const prepare = (theme) => async (page) => {
  await page.emulateMedia({ colorScheme: theme, reducedMotion: "reduce" });
  await page.clock.setFixedTime(fixedTime);
};

for (const theme of ["light", "dark"]) {
  // First run (the environment has not been prepared yet).
  {
    const fixture = await createWorkbench({
      viewport, initialization: { required: true, tracked: true, started: false },
      clientContext: { mode: "local", connection_id: "local", client_platform: "linux", server_platform: "linux" },
      beforeNavigate: async (page, state) => {
        await prepare(theme)(page);
        await page.addInitScript(() => localStorage.setItem("cage.audio", JSON.stringify({ muted: true })));
        state.status.local_vm_storage = true;
        await page.route("**/api/environment/storage", (route) => route.fulfill({ json: { directory: "/preview/cage-state", has_environment: false } }));
        await page.route("**/api/environment/guest-systems", (route) => route.fulfill({ json: { fresh: true, available: true, system: "ubuntu-22.04", linux_systems: ["ubuntu-22.04"] } }));
      },
    });
    try { await remember(fixture.page, theme, "first-run", { page: "#root" }); assert.deepEqual(fixture.errors, []); }
    finally { await fixture.browser.close(); }
  }
  // Stopped home, then settings, with the environment stopped.
  {
    const fixture = await createWorkbench({ viewport, beforeNavigate: prepare(theme) });
    const { page } = fixture;
    try {
      await remember(page, theme, "home-stopped", { sidebar: "aside.workspace-sidebar", navigation: "nav.sidebar-navigation", main: "main.workspace" });
      const stop = logger(page, theme === "light" ? "settings" : "settings-dark");
      await page.getByRole("button", { name: "设置", exact: true }).click();
      await page.getByRole("navigation", { name: "设置分组" }).getByRole("button", { name: "通用", exact: true }).waitFor();
      await settle(page);
      stop();
      await remember(page, theme, "settings", { navigation: 'nav[aria-label="设置分组"]', page: ".settings-page" });
      assert.deepEqual(fixture.errors, []);
    } finally { await fixture.browser.close(); }
  }
  // A running environment: home, the 新建 menu, a Claude terminal tab. The
  // light run also carries the request log of the whole path, with the idle wait.
  {
    const fixture = await createWorkbench({ viewport, beforeNavigate: prepare(theme) });
    const { page, state } = fixture;
    try {
      state.status.vm_running = true;
      state.status.ready = true;
      const phase = (name) => (theme === "light" ? name : name + "-dark");
      let stop = logger(page, phase("load"));
      await page.reload();
      const trigger = page.locator('button[data-terminal-location="' + project + '"]:not([disabled])');
      await trigger.waitFor();
      await remember(page, theme, "home-running", { sidebar: "aside.workspace-sidebar", navigation: "nav.sidebar-navigation", main: "main.workspace" });
      stop();
      if (theme === "light") {
        stop = logger(page, "idle");
        await page.waitForTimeout(idleMs);
        stop();
      }
      stop = logger(page, phase("new"));
      await trigger.click();
      const menu = page.getByRole("menu", { name: "新建终端" });
      await menu.waitFor();
      // The fixture is a Claude-only environment whose presence is known: no Codex row (choice C).
      assert.deepEqual(await menu.getByRole("menuitem").allTextContents(), ["普通终端", "Claude 终端"], "the new-terminal menu of a Claude-only environment");
      await remember(page, theme, "new-menu", { menu: "#terminal-create-menu", sidebar: "aside.workspace-sidebar" });
      await menu.getByRole("menuitem", { name: "Claude 终端", exact: true }).click();
      await page.locator(".xterm-rows").filter({ hasText: "Linux terminal" }).first().waitFor();
      await remember(page, theme, "claude-terminal", { sidebar: "aside.workspace-sidebar", main: "main.workspace" });
      stop();
      assert.deepEqual(fixture.errors, []);
    } finally { await fixture.browser.close(); }
  }
}
await fs.writeFile(path.join(target, "capture.json"), JSON.stringify({ idleMs, fixedTime, states, phases }, null, 1));
console.log("captured " + Object.keys(states).length + " states and " + Object.values(phases).reduce((sum, list) => sum + list.length, 0) + " requests into " + target);
if (mode === "record") process.exit(0);

// ---- compare ----------------------------------------------------------------
const base = JSON.parse(await fs.readFile(path.join(baselineDirectory, "capture.json"), "utf8"));
const failures = [];
const notes = [];
assert.equal(base.idleMs, idleMs, "the baseline was recorded with another idle wait (CAGE_IDLE_MS)");
for (const key of new Set([...Object.keys(base.states), ...Object.keys(states)])) {
  const before = base.states[key], after = states[key];
  if (!before || !after) { failures.push(key + ": state only on one side"); continue; }
  let before_removed = 0, after_removed = 0;
  for (const surface of new Set([...Object.keys(before.surfaces), ...Object.keys(after.surfaces)])) {
    const left = before.surfaces[surface], right = after.surfaces[surface];
    if (!left || !right || left.missing || right.missing) { if (JSON.stringify(left) !== JSON.stringify(right)) failures.push(key + " " + surface + ": surface missing on one side"); continue; }
    before_removed += left.removed;
    after_removed += right.removed;
    if (left.html !== right.html) {
      let at = 0;
      while (at < left.html.length && left.html[at] === right.html[at]) at++;
      failures.push(key + " " + surface + ": the DOM differs at " + at + "\n    base:    …" + left.html.slice(Math.max(0, at - 60), at + 140) + "\n    branch:  …" + right.html.slice(Math.max(0, at - 60), at + 140));
    }
  }
  // The screenshot is compared unless an allowed node is on one side only.
  if (before_removed !== after_removed) notes.push(key + ": " + before_removed + " allowed node(s) in the baseline, " + after_removed + " here: screenshot not compared");
  else if (before.pngHash !== after.pngHash) failures.push(key + ": the screenshot differs (" + before.png + " in the baseline, " + after.png + " here)");
}

const gone = /^GET \/api\/codex-accounts(\?|$)/;
const canonical = (line) => line.replace(/providers=claude(%2C|,)codex/, "providers=claude");
const multiset = (lines) => lines.reduce((map, line) => map.set(line, (map.get(line) || 0) + 1), new Map());
const filtered = (phase, source) => {
  const lines = (source[phase] || []).filter((line) => !gone.test(line)).map(canonical);
  // Opening the settings may ask once whether anything can be added.
  return phase.startsWith("settings") ? lines.filter((line, index) => !(line === "GET /api/agents" && lines.indexOf(line) === index)) : lines;
};
const everything = (source) => multiset(Object.keys(source).flatMap((phase) => filtered(phase, source)));
const wholeBase = everything(base.phases), wholeHere = everything(phases);
// A polled request is one the baseline makes while the window idles: a timer made
// it, so per phase it is compared within two requests (a poll may land just before or
// after a phase's edge), and over the whole path it must exist on both sides. Decided
// by the baseline alone, so a one-shot request that the branch makes twice as often
// stays exact. Everything else is compared exactly.
const polled = new Set(filtered("idle", base.phases));
for (const phase of new Set([...Object.keys(base.phases), ...Object.keys(phases)])) {
  const left = multiset(filtered(phase, base.phases));
  const right = multiset(filtered(phase, phases));
  for (const line of new Set([...left.keys(), ...right.keys()])) {
    const was = left.get(line) || 0, now = right.get(line) || 0;
    if (/\/api\/agents/.test(line)) { failures.push(phase + ": " + line + " (" + now + "×) is a request of the feature on the Claude path"); continue; }
    if (polled.has(line) ? Math.abs(was - now) > 2 : was !== now) failures.push(phase + ": " + line + " " + was + "× in the baseline, " + now + "× here");
  }
}
for (const line of polled) {
  if (!wholeBase.get(line) || !wholeHere.get(line)) failures.push("over the whole path: the polled request " + line + " is on one side only (" + (wholeBase.get(line) || 0) + "× in the baseline, " + (wholeHere.get(line) || 0) + "× here)");
}
for (const note of notes) console.log("note: " + note);
if (failures.length) { console.error(failures.join("\n")); process.exit(1); }
console.log("PASS: claude path unchanged (" + Object.keys(states).length + " states, " + Object.keys(phases).length + " request phases, idle " + idleMs + " ms)");
