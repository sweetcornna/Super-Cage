import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

// The Claude Desktop pane opens the installed application once, on its first
// status read, unless the user already started a job. Its poll outlives
// renders: a read that finishes after the user's click, before the workbench
// status reports the job, must still see that job (the poll once read the job
// from the render that started it and opened the app a second time).
const { browser, page, state, errors } = await createWorkbench();
const desktop = { installed: false, running: false, version: "4.4212.0", failed: false };
const actions = [];
let holdStatus = false;
const held = [];
await page.route("**/api/claude-desktop**", async route => {
  const path = new URL(route.request().url()).pathname;
  if (path.endsWith("/vnc-credentials")) return route.fulfill({ json: { password: "fixture-only" } });
  if (route.request().method() === "GET") {
    if (holdStatus) await new Promise(resolve => held.push(resolve));
    return route.fulfill({ json: { ...desktop } });
  }
  const action = path.split("/").at(-1);
  actions.push(action);
  const job = { id: `desktop-${actions.length}`, kind: `claude-desktop-${action}`, running: true, message: "正在安装 Claude Desktop…" };
  // The workbench status has not reported the job yet.
  return route.fulfill({ json: job });
});
try {
  state.status.vm_running = state.status.ready = true;
  state.status.guest_system = "debian-13";
  await page.reload();
  const open = () => page.getByRole("button", { name: "Claude Desktop", exact: true }).click();
  await open();
  await page.getByRole("button", { name: "下载并安装", exact: true }).waitFor();
  await page.getByRole("button", { name: "工作台", exact: true }).first().click();
  // Installed meanwhile; the next visit's first status read is slow and the
  // user repairs the installation before it returns.
  Object.assign(desktop, { installed: true });
  holdStatus = true;
  await open();
  await page.getByRole("button", { name: "下载并安装", exact: true }).click();
  for (let i = 0; i < 200 && !actions.length; i++) await new Promise(resolve => setTimeout(resolve, 25));
  assert.deepEqual(actions, ["install"]);
  for (let i = 0; i < 200 && !held.length; i++) await new Promise(resolve => setTimeout(resolve, 25));
  holdStatus = false;
  held.splice(0).forEach(resolve => resolve());
  // The click cancels the read in flight; the next one comes five seconds on.
  const reads = [];
  page.on("request", request => { if (request.method() === "GET" && new URL(request.url()).pathname === "/api/claude-desktop") reads.push(Date.now()); });
  for (let i = 0; i < 400 && !reads.length; i++) await new Promise(resolve => setTimeout(resolve, 25));
  assert.ok(reads.length, "the pane did not read its status again");
  await new Promise(resolve => setTimeout(resolve, 800));
  assert.deepEqual(actions, ["install"], "a status read that finished after the user's job must not open the app again");
  assert.deepEqual(errors, []);
  console.log("PASS: a status read that finishes after the user starts a job does not open the app a second time");
} finally {
  await browser.close();
}
