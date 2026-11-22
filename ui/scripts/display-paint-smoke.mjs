// Replays a recorded-style display stream (a page scrolled at 60 updates a
// second, each a full-pane Tight JPEG at the pixel density of a 2x screen)
// through the real noVNC client and the workbench, and checks the paint path
// for long tasks. Real browser WebSockets against the RFB wire fixture:
// Playwright's mocked WebSocket lacks what noVNC checks on its socket.
//   CAGE_PAINT_SECONDS (default 5), CAGE_PAINT_REPORT=<file> for the numbers,
//   CAGE_PAINT_CPU_THROTTLE=4 to emulate a slower computer (CDP CPU throttling).
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { createWorkbench } from "./workbench-fixture.mjs";
import { startDisplay, tightJpegUpdate } from "./rfb-wire-fixture.mjs";

const seconds = Number(process.env.CAGE_PAINT_SECONDS || 5);
const size = { width: 2352, height: 1624 };
const display = await startDisplay(process.env.CAGE_UI_URL || "http://127.0.0.1:5198", { frameInterval: 16, size });
const { browser, page, state, errors } = await createWorkbench({ url: display.url, mockWebSockets: false, viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2 });
page.setDefaultTimeout(15000);
await page.route("**/api/vnc-credentials", route => route.fulfill({ json: { password: "fixture-only", display_scale: 2, dynamic_scale: true, pixel_wheel: false } }));
await page.route("**/api/browser/resize", route => route.fulfill({ json: { ok: true, scale: route.request().postDataJSON().scale } }));
const report = {};
try {
  state.status.ready = state.status.vm_running = true;
  state.tabs = [{ id: "page", kind: "web", title: "长页面", url: "https://example.test/long" }];
  state.active = "page";
  await page.reload();
  // A long article scrolled by 48 pixels per update, encoded as the guest would.
  const frames = await page.evaluate(async ({ width, height }) => {
    const canvas = document.createElement("canvas");
    canvas.width = width; canvas.height = height;
    const context = canvas.getContext("2d");
    const out = [];
    for (let frame = 0; frame < 24; frame++) {
      context.fillStyle = "#ffffff";
      context.fillRect(0, 0, width, height);
      context.fillStyle = "#1f1e1d";
      context.font = "30px -apple-system, sans-serif";
      for (let line = 0; line < 60; line++) {
        const y = line * 56 - (frame * 48) % 56 + 40;
        const n = line + Math.floor(frame * 48 / 56);
        context.fillText(`第 ${n} 行 · The quick brown fox jumps over the lazy dog ${n * 7919 % 10007} — 滚动中的网页文字与图片`, 80, y);
        if (n % 9 === 0) { context.fillStyle = `hsl(${n * 37 % 360} 40% 70%)`; context.fillRect(1500, y - 30, 600, 200); context.fillStyle = "#1f1e1d"; }
      }
      const blob = await new Promise(resolve => canvas.toBlob(resolve, "image/jpeg", 0.8));
      const bytes = new Uint8Array(await blob.arrayBuffer());
      let binary = "";
      for (let i = 0; i < bytes.length; i += 32768) binary += String.fromCharCode(...bytes.subarray(i, i + 32768));
      out.push(btoa(binary));
    }
    return out;
  }, size);
  const messages = frames.map(data => tightJpegUpdate(Buffer.from(data, "base64"), { width: size.width, height: size.height }));
  report.jpegKiB = Math.round(messages.reduce((sum, item) => sum + item.length, 0) / messages.length / 1024);
  display.replay(messages);
  await page.getByRole("button", { name: "打开浏览器", exact: true }).click();
  await page.getByText("正在连接画面", { exact: true }).waitFor({ state: "hidden" });
  await page.waitForFunction(() => window.__cagePerf?.summary().displays.at(-1)?.framesPainted > 10);
  await page.waitForTimeout(500);
  const throttle = Number(process.env.CAGE_PAINT_CPU_THROTTLE || 1);
  if (throttle > 1) {
    const cdp = await page.context().newCDPSession(page);
    await cdp.send("Emulation.setCPUThrottlingRate", { rate: throttle });
    report.cpuThrottle = throttle;
  }
  const pointersBefore = display.stats.pointers.length, framesBefore = display.stats.frames, bytesBefore = display.stats.replayBytes || 0;
  await page.evaluate((ms) => {
    window.__paint = { longTasks: [], frameTimes: [], until: performance.now() + ms };
    new PerformanceObserver(list => { for (const entry of list.getEntries()) window.__paint.longTasks.push(Math.round(entry.duration)); }).observe({ type: "longtask" });
    let last = performance.now();
    const tick = (now) => { window.__paint.frameTimes.push(now - last); last = now; if (now < window.__paint.until) requestAnimationFrame(tick); };
    requestAnimationFrame(tick);
  }, seconds * 1000);
  // Pointer input during the stream goes out without waiting for a frame,
  // at most about once per display frame.
  // 240 moves dispatched within about 100 ms, faster than any display frame.
  const burst = await page.evaluate(async () => {
    const canvas = document.querySelector(".browser-lane .remote-canvas canvas");
    const box = canvas.getBoundingClientRect();
    const started = performance.now();
    for (let i = 0; i < 240; i++) {
      canvas.dispatchEvent(new MouseEvent("mousemove", { clientX: box.left + 40 + i, clientY: box.top + 200, bubbles: true }));
      if (i % 24 === 23) await new Promise(resolve => setTimeout(resolve, 8));
    }
    return { ms: performance.now() - started, lastX: Math.floor((box.left + 40 + 239 - box.left) * canvas.width / box.width) };
  });
  const moveMs = burst.ms;
  // Wheel steps count as input: the time to the next painted frame is measured.
  const pane = await page.locator(".browser-lane .remote-canvas").boundingBox();
  await page.mouse.move(pane.x + pane.width / 2, pane.y + pane.height / 2);
  for (let i = 0; i < 10; i++) { await page.mouse.wheel(0, 120); await page.waitForTimeout(150); }
  await page.waitForTimeout(Math.max(0, seconds * 1000 - moveMs - 1500) + 200);
  const measured = await page.evaluate(() => {
    const times = window.__paint.frameTimes.slice(1).sort((a, b) => a - b);
    const at = (p) => Math.round(times[Math.min(times.length - 1, Math.ceil(p * times.length) - 1)] * 10) / 10;
    const display = window.__cagePerf.summary().displays.at(-1);
    return { longTasks: window.__paint.longTasks, rafP50: at(0.5), rafP95: at(0.95), rafMax: at(1), display };
  });
  report.longTasks = measured.longTasks;
  report.frameTimeMs = { p50: measured.rafP50, p95: measured.rafP95, max: measured.rafMax };
  report.updatesSent = display.stats.frames - framesBefore;
  report.updatesPerSecond = Math.round(report.updatesSent / seconds);
  report.mbPerSecond = Math.round(((display.stats.replayBytes || 0) - bytesBefore) / seconds / 1e5) / 10;
  report.paint = { framesPainted: measured.display.framesPainted, handleMs: measured.display.handleMs, drawMs: measured.display.drawMs, updateIntervalMs: measured.display.updateIntervalMs, inputToFrameMs: measured.display.inputToFrameMs };
  const sentMoves = display.stats.pointers.slice(pointersBefore).filter(item => item.y === display.stats.pointers[pointersBefore]?.y);
  report.pointer = { moves: 240, moveMs: Math.round(moveMs), sent: sentMoves.length, last: sentMoves.at(-1)?.x, expectedLast: burst.lastX };
  if (process.env.CAGE_PAINT_REPORT) await fs.writeFile(process.env.CAGE_PAINT_REPORT, JSON.stringify(report, null, 2));
  console.log(JSON.stringify(report));
  assert.ok(report.pointer.sent > 0, "pointer input reached the guest during the stream");
  assert.ok(report.pointer.sent <= Math.ceil(moveMs / 16) + 2, "pointer moves are coalesced to about one per display frame");
  assert.ok(Math.abs(report.pointer.last - report.pointer.expectedLast) <= 2, "the last position is sent");
  assert.ok(measured.display.inputToFrameMs.count > 0, "input to the next frame is measured");
  assert.ok(measured.display.handleMs.max < 50, "no stream message takes 50 ms or more to handle");
  assert.ok(measured.display.drawMs.max < 50, "no decoded rectangle takes 50 ms or more to draw");
  assert.deepEqual(report.longTasks.filter(ms => ms >= 50), [], "no long task of 50 ms or more while frames stream");
  assert.deepEqual(errors, []);
  console.log("PASS: replayed display stream painted without long tasks; pointer input coalesced and not held behind frames");
} finally {
  await browser.close();
  await display.close();
}
