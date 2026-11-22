// A hidden browser or Claude Desktop pane keeps its display stream with a
// server that speaks cage-display.v1, and shows it again without connecting.
// Real browser WebSockets against the RFB wire fixture; no guest is involved.
import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";
import { startDisplay } from "./rfb-wire-fixture.mjs";

const holdFor = 6000, emptyFor = 400;
const display = await startDisplay(process.env.CAGE_UI_URL || "http://127.0.0.1:5198", { displayControl: true, sharedClipboard: true, frameInterval: 30, holdFor, emptyFor });
const { browser, page, state, errors } = await createWorkbench({ url: display.url, mockWebSockets: false, viewport: { width: 1280, height: 820 } });
page.setDefaultTimeout(10000);
const resizes = [];
await page.route("**/api/vnc-credentials", route => route.fulfill({ json: { password: "fixture-only", display_scale: 2, dynamic_scale: true, pixel_wheel: false } }));
await page.route("**/api/browser/resize", route => { const body = route.request().postDataJSON(); resizes.push({ ...body, at: Date.now() }); return route.fulfill({ json: { ok: true, scale: body.scale } }); });
await page.route("**/api/claude-desktop", route => route.fulfill({ json: { installed: true, running: true, version: "1.2.3", failed: false, managed: true } }));
await page.route("**/api/claude-desktop/vnc-credentials", route => route.fulfill({ json: { password: "fixture-only", display_scale: 2, dynamic_scale: true } }));
await page.route("**/api/claude-desktop/resize", route => route.fulfill({ json: { ok: true, scale: route.request().postDataJSON().scale } }));
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const report = { checks: [] };
const watch = () => page.evaluate(() => {
  window.__seen = { connecting: 0, emptyDesktop: 0 };
  window.__watch?.disconnect();
  window.__watch = new MutationObserver(() => {
    if (document.body.innerText.includes("正在连接画面")) window.__seen.connecting++;
    if (document.querySelector(".desktop-empty")) window.__seen.emptyDesktop++;
  });
  window.__watch.observe(document.body, { childList: true, subtree: true, characterData: true });
});
const seen = () => page.evaluate(() => window.__seen);
const toTerminal = () => page.locator(".workspace-sidebar, nav, aside").getByText("终端 1", { exact: true }).first().click();
const toBrowser = () => page.getByRole("button", { name: "打开浏览器", exact: true }).first().click();
const browserPainted = () => page.waitForFunction(() => {
  const canvas = document.querySelector(".browser-lane .remote-canvas canvas");
  return canvas?.width > 0 && canvas.getContext("2d").getImageData(2, 2, 1, 1).data[0] > 0;
});
const streams = () => (display.stats.paths["/api/vnc"] || 0) + (display.stats.paths["/api/claude-desktop/vnc"] || 0);
const control = path => display.stats.control.filter(item => item.path === path).map(item => item.visible);
try {
  state.status.ready = state.status.vm_running = true;
  state.tabs = [{ id: "page", kind: "web", title: "示例页面", url: "https://example.test" }];
  state.active = "page";
  state.terminals = [{ id: "t1", title: "终端 1", workspace: state.settings.workspace }];
  await page.reload();
  await toBrowser();
  await page.getByText("正在连接画面", { exact: true }).waitFor({ state: "hidden" });
  await browserPainted();
  assert.ok(display.stats.protocols.some(item => item.includes("cage-display.v1")), "the client offers the display control protocol");
  assert.equal(display.stats.paths["/api/vnc"], 1, "one display stream");
  const firstSize = resizes.at(-1);
  assert.ok(firstSize?.scale, "a following guest is sized for the screen");

  // Hide: the stream stays, says it is hidden and stops asking for pixels.
  await toTerminal();
  await page.locator(".terminal-view:not(.concealed) .xterm").first().waitFor();
  await sleep(300);
  assert.deepEqual(control("/api/vnc"), [false]);
  const requests = display.stats.requests.length, frames = display.stats.frames;
  await sleep(600);
  assert.equal(display.stats.requests.length, requests, "a hidden pane requests no updates");
  assert.equal(display.stats.frames, frames, "and receives no frames");
  assert.equal(display.stats.closeCodes.length, 0, "the stream is kept");
  report.checks.push("hidden stream kept, no requests, no frames");

  // Show within the hold: no connecting state, no new stream, one full
  // update, and the last frame stays while the guest shows its empty desktop.
  await watch();
  const connections = streams();
  await page.evaluate(() => {
    window.__dark = 0;
    const check = () => {
      const canvas = document.querySelector(".browser-lane .remote-canvas canvas");
      if (canvas?.width && canvas.getContext("2d").getImageData(2, 2, 1, 1).data[0] === 0) window.__dark++;
      if (performance.now() < window.__until) requestAnimationFrame(check);
    };
    window.__until = performance.now() + 1500;
    requestAnimationFrame(check);
  });
  const shownAt = Date.now();
  await toBrowser();
  await browserPainted();
  report.switchBackMs = Date.now() - shownAt;
  await sleep(1300);
  assert.equal(streams(), connections, "no second connection");
  assert.equal((await seen()).connecting, 0, "no connecting state on a switch back within the hold");
  assert.deepEqual(control("/api/vnc"), [false, true]);
  const resumeRequest = display.stats.requests.find(item => item.at >= shownAt);
  assert.equal(resumeRequest?.incremental, false, "the first request after the show is a full update");
  assert.equal(await page.evaluate(() => window.__dark), 0, "the empty desktop is never painted over the last frame");
  assert.ok(resizes.every(item => item.at < shownAt), "a short hide does not ask for the size again");
  report.checks.push("switch back without reconnect, full update, last frame kept");

  // A long hide asks once for the unchanged size, which restores the guest's refresh rate.
  await toTerminal();
  await sleep(3300);
  const longShow = Date.now();
  await toBrowser();
  await browserPainted();
  await sleep(500);
  const again = resizes.filter(item => item.at >= longShow);
  assert.equal(again.length, 1, "one resize after a long hide");
  assert.deepEqual({ width: again[0].width, height: again[0].height, scale: again[0].scale }, { width: firstSize.width, height: firstSize.height, scale: firstSize.scale });
  report.checks.push("long hide restores the refresh rate with one unchanged resize");

  // Hidden past the server's hold: the server closes with 4001 and the pane
  // does not reconnect until it is shown, then shows its connecting state.
  await toTerminal();
  await sleep(holdFor + 400);
  assert.deepEqual(display.stats.closeCodes, [4001]);
  const afterClose = streams();
  await sleep(1500);
  assert.equal(streams(), afterClose, "no reconnect while hidden");
  await watch();
  await toBrowser();
  await page.getByText("正在连接画面", { exact: true }).waitFor({ state: "hidden" });
  await browserPainted();
  assert.equal(streams(), afterClose + 1, "one new stream on the next show");
  report.checks.push("server hold timeout: no reconnect while hidden, one connect on show");

  // Claude Desktop keeps its stream the same way.
  await page.getByRole("button", { name: "Claude Desktop", exact: true }).click();
  await page.locator(".claude-desktop-view .remote-canvas canvas").waitFor();
  await page.getByText("正在连接画面", { exact: true }).waitFor({ state: "hidden" });
  await toBrowser();
  await browserPainted();
  await sleep(300);
  assert.deepEqual(control("/api/claude-desktop/vnc"), [false]);
  await watch();
  const desktopConnections = streams();
  await page.getByRole("button", { name: "Claude Desktop", exact: true }).click();
  await page.locator(".claude-desktop-view .remote-canvas canvas").waitFor();
  await sleep(600);
  assert.equal(streams(), desktopConnections, "Claude Desktop shows its kept stream");
  assert.deepEqual(await seen(), { connecting: 0, emptyDesktop: 0 }, "no connecting state and no empty-state flash");
  assert.deepEqual(control("/api/claude-desktop/vnc"), [false, true]);
  report.checks.push("Claude Desktop kept across a switch");

  const perf = await page.evaluate(() => window.__cagePerf.summary());
  report.perf = perf.displays.map(({ kind, protocol, reconnect, framesPainted, framesHeld, holds, resumes, closeCode, updateIntervalMs }) =>
    ({ kind, protocol, reconnect, framesPainted, framesHeld, holds, resumes, closeCode, updateIntervalMs }));
  assert.ok(perf.displays.some(item => item.framesHeld > 0), "the frame gate held at least one empty desktop");
  assert.deepEqual(errors, []);
  console.log(JSON.stringify(report));
  console.log("PASS: hidden display streams are kept and shown again without reconnecting; long hides restore the rate; the server hold timeout reconnects on show");
} catch (error) {
  console.error(JSON.stringify({ report, stats: { ...display.stats, requests: display.stats.requests.length, pointers: undefined }, errors, body: await page.locator("body").innerText().catch(() => "") }));
  throw error;
} finally {
  await browser.close();
  await display.close();
}
