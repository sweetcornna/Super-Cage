// After a reset the browser controller restarts. If the page's browser event
// stream reconnects meanwhile, the host answers with an error and EventSource
// closes the stream for good. The next status poll opens it again (review 3,
// finding 5), and terminals stay in the tab list when the new stream's first
// snapshot arrives.
// CAGE_UI_URL=... node scripts/browser-stream-reconnect-smoke.mjs
import assert from "node:assert/strict";
import { setTimeout as delay } from "node:timers/promises";
import { createWorkbench } from "./workbench-fixture.mjs";

const { browser, page, state, errors } = await createWorkbench();
page.setDefaultTimeout(15000);
const checks = [];
try {
  state.status.ready = state.status.vm_running = true;
  state.tabs = [{ id: "page-one", title: "网页一", kind: "web", url: "https://example.test/one" }];
  state.active = "page-one";
  state.terminals = [{ id: "terminal-one", title: "工作终端一", workspace: state.settings.workspace }];
  const streams = [];
  await page.route(/\/api\/browser\/events(\?|$)/, route => {
    streams.push(Date.now());
    if (streams.length === 1) return route.fulfill({ status: 502, contentType: "text/plain; charset=utf-8", body: "浏览器没有响应，请确认环境仍在运行后重试" });
    // A long retry: the stream ends with this body, and the browser's own
    // reconnection must not be what this test sees.
    const snapshot = { tabs: [{ id: "page-two", title: "网页二", kind: "web", url: "https://example.test/two" }], active: "page-two" };
    return route.fulfill({ contentType: "text/event-stream", body: "retry: 600000\ndata: " + JSON.stringify(snapshot) + "\n\n" });
  });
  await page.reload();
  await page.getByRole("tab", { name: "工作终端一", exact: true }).waitFor();
  for (let i = 0; i < 75 && streams.length < 2; i++) await delay(200);
  assert.ok(streams.length >= 2, "a stream that ended with an error is opened again by the status poll: " + streams.length);
  checks.push(`the closed stream was opened again after ${streams[1] - streams[0]} ms (status poll)`);
  await page.getByRole("button", { name: "打开浏览器", exact: true }).click();
  await page.getByRole("tab", { name: "网页二", exact: true }).waitFor();
  assert.equal(await page.getByRole("tab", { name: "工作终端一", exact: true }).count(), 1);
  assert.equal(await page.getByRole("tab", { name: "网页一", exact: true }).count(), 0, "the snapshot replaced the browser tabs");
  checks.push("the new stream's snapshot arrives and terminals stay in the tab list");
  await delay(1500);
  assert.equal(streams.length, 2, "one reopening, no loop");
  checks.push("one reopening, no loop");
  assert.deepEqual(errors, []);
  console.log(JSON.stringify({ passed: true, checks }, null, 1));
} finally {
  await browser.close();
}
