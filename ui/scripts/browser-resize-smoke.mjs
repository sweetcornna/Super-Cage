import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";
import { startDisplay } from "./rfb-wire-fixture.mjs";

// The pane grows while the guest still shows its old size: the old frame
// stays at its own size in the corner (never stretched), pointer input maps
// onto it, and the guest's new size then fills the pane over the same stream.
const display = await startDisplay(process.env.CAGE_UI_URL || "http://127.0.0.1:5198", { frameInterval: 200 });
const { browser, page, state, errors } = await createWorkbench({
  url: display.url, mockWebSockets: false, viewport: { width: 1000, height: 740 },
});
page.setDefaultTimeout(10000);
const requests = [];
await page.route("**/api/browser/resize", route => {
  requests.push({ ...route.request().postDataJSON(), at: Date.now() });
  return route.fulfill({ json: { ok: true } });
});
try {
  state.status.ready = state.status.vm_running = true;
  state.tabs = [{ id: "page", kind: "web", title: "Resize", url: "https://example.test" }];
  state.active = "page";
  await page.reload();
  await page.getByRole("button", { name: "打开浏览器", exact: true }).click();
  const canvas = page.locator(".browser-lane .remote-canvas canvas");
  const pixelAt = (x, y) => canvas.evaluate((c, [x, y]) => [...c.getContext("2d").getImageData(x, y, 1, 1).data], [x, y]);
  await page.waitForFunction(() => {
    const c = document.querySelector(".browser-lane canvas");
    return c?.width && c.getContext("2d").getImageData(0, 0, 1, 1).data[0] > 0;
  });
  display.pauseFrames(true);
  const oldPixel = await pixelAt(639, 479);
  const connections = display.stats.connections;
  const oldBox = await canvas.boundingBox();
  // A guest at a fixed density 2 is drawn at half size: 640 x 480 is 320 x 240 CSS pixels.
  assert.ok(Math.abs(oldBox.width - 320) < 1 && Math.abs(oldBox.height - 240) < 1, "the frame is drawn at its intended size");
  const pane = await page.locator(".browser-lane .remote-canvas").boundingBox();
  assert.ok(Math.abs(oldBox.x - pane.x) < 1 && Math.abs(oldBox.y - pane.y) < 1, "anchored in the corner of the pane");
  const before = requests.length;
  await page.setViewportSize({ width: 1240, height: 760 });
  await page.waitForTimeout(50);
  const box = await page.locator(".browser-lane .remote-canvas").boundingBox();
  const shown = await canvas.boundingBox();
  assert.ok(Math.abs(shown.width - oldBox.width) < 1 && Math.abs(shown.height - oldBox.height) < 1,
    "the previous frame keeps its size while the guest resizes: letterboxed, never stretched");
  const x = Math.floor(shown.width * .75), y = Math.floor(shown.height * .25);
  await canvas.click({ position: { x, y } });
  // Mouse events carry whole CSS pixels: one of them is two framebuffer pixels here.
  assert.ok(Math.abs(display.stats.pointer.x - x / shown.width * 640) <= 640 / shown.width + 0.5);
  assert.ok(Math.abs(display.stats.pointer.y - y / shown.height * 480) <= 480 / shown.height + 0.5);
  // One final size for the new pane, not one per layout step.
  await page.waitForFunction(() => true, null, { timeout: 100 }).catch(() => {});
  for (let i = 0; i < 40 && requests.length === before; i++) await page.waitForTimeout(25);
  await page.waitForTimeout(300);
  const sent = requests.slice(before);
  assert.equal(sent.length, 1, "one resize request after the pane settled");
  const desired = sent[0];
  assert.equal(desired.width, Math.floor(box.width * 2));
  display.resize(desired); // Send dimensions now, withhold all pixel updates.
  await page.waitForFunction(w => document.querySelector(".browser-lane canvas")?.width === w, desired.width);
  const filled = await canvas.boundingBox();
  assert.ok(Math.abs(filled.width - box.width) < 1 && Math.abs(filled.height - box.height) < 1, "the new size fills the pane");
  assert.deepEqual(await pixelAt(639, 479), oldPixel, "the old frame stays unscaled in the corner until new pixels arrive");
  assert.equal((await pixelAt(desired.width - 1, desired.height - 1))[3], 0, "the rest of the larger frame is empty, not stretched");
  display.pauseFrames(false);
  await page.waitForFunction(value => {
    const c = document.querySelector(".browser-lane canvas");
    const p = c.getContext("2d").getImageData(c.width - 1, c.height - 1, 1, 1).data;
    return p[3] === 255 && p[0] !== value;
  }, oldPixel[0]);
  assert.equal(display.stats.connections, connections);
  assert.deepEqual(errors, []);
  console.log("PASS: browser resize keeps the last frame unstretched in the corner, maps the pointer onto it, sends one final size, then repaints over the same RFB connection");
} catch (error) {
  console.error({ errors, stats: display.stats, requests, body: await page.locator("body").innerText() });
  throw error;
} finally {
  await browser.close();
  await display.close();
}
