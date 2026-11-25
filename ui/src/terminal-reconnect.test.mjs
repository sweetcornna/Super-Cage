import test from "node:test";
import assert from "node:assert/strict";
import {
  terminalHiddenKeepaliveMs, terminalHistoryPlan, terminalKeepaliveMessage, terminalKeepaliveMs, terminalLiveness,
  terminalProbeMs, terminalReconnectLimit, terminalReconnectPlan, terminalSilenceMs, terminalStreamPath, terminalViewer,
} from "./terminal-reconnect.mjs";

test("a stream dropped after being open reconnects at once and resets the count", () => {
  assert.deepEqual(terminalReconnectPlan(4, 60_000), { failures: 1, delay: 500 });
});

test("quick failures back off and stop at the limit", () => {
  let failures = 0;
  const delays = [];
  for (;;) {
    const plan = terminalReconnectPlan(failures, 0);
    failures = plan.failures;
    if (!plan.delay) break;
    delays.push(plan.delay);
  }
  assert.deepEqual(delays, [500, 1000, 2000, 4000, 8000, 15000]);
  assert.equal(delays.length, terminalReconnectLimit);
});

test("the keepalive is the ping the terminal stream answers", () => {
  assert.deepEqual(JSON.parse(terminalKeepaliveMessage), { type: "ping" });
});

test("the stream resumes after the last received byte", () => {
  assert.equal(terminalStreamPath("0123456789abcdef", null), "/terminals/0123456789abcdef/stream?after=0");
  assert.equal(terminalStreamPath("0123456789abcdef", 1234), "/terminals/0123456789abcdef/stream?after=1234");
  assert.equal(terminalStreamPath("0123456789abcdef", 7, "web-0123456789ab"), "/terminals/0123456789abcdef/stream?after=7&viewer=web-0123456789ab");
  for (const viewer of ["short", "has space 12345", "a".repeat(65), 42])
    assert.equal(terminalStreamPath("0123456789abcdef", 7, viewer), "/terminals/0123456789abcdef/stream?after=7");
  for (const invalid of [-1, 1.5, NaN, Number.MAX_SAFE_INTEGER + 2, "12", undefined])
    assert.equal(terminalStreamPath("a", invalid), "/terminals/a/stream?after=0");
});

test("a history marker continues the screen only from the same offset", () => {
  assert.deepEqual(terminalHistoryPlan(100, { type: "history", offset: 100, reset: false, size: 20 }),
    { resumable: true, reset: false, offset: 100, size: 20 });
  // An offset the server no longer keeps, or a different one, clears the screen.
  assert.equal(terminalHistoryPlan(100, { type: "history", offset: 400, reset: true, size: 20 }).reset, true);
  assert.equal(terminalHistoryPlan(100, { type: "history", offset: 90, reset: false, size: 0 }).reset, true);
  // A new view starts from whatever history is kept.
  assert.deepEqual(terminalHistoryPlan(null, { type: "history", offset: 0, reset: false, size: 5 }),
    { resumable: true, reset: true, offset: 0, size: 5 });
  assert.equal(terminalHistoryPlan(7, { type: "history", offset: 7, size: -1 }).size, 0);
});

test("a server without offsets replays everything and is never resumed", () => {
  assert.deepEqual(terminalHistoryPlan(100, { type: "history" }), { resumable: false, reset: true, offset: null, size: null });
  assert.equal(terminalHistoryPlan(100, { type: "history", offset: "100" }).resumable, false);
});

test("keepalive pings follow page visibility", () => {
  const now = 1_000_000;
  const base = { now, lastReceived: now, probe: 0, heartbeats: true };
  assert.equal(terminalLiveness({ ...base, lastPing: now - terminalKeepaliveMs, visible: true }), "ping");
  assert.equal(terminalLiveness({ ...base, lastPing: now - terminalKeepaliveMs, visible: false }), "idle");
  assert.equal(terminalLiveness({ ...base, lastPing: now - terminalHiddenKeepaliveMs, visible: false }), "ping");
});

test("a silent stream is probed before it is dropped", () => {
  const now = 1_000_000;
  const silent = { now, lastReceived: now - terminalSilenceMs, lastPing: now - 1000, visible: false, heartbeats: true };
  assert.equal(terminalLiveness(silent), "probe");
  // Anything received after the probe keeps the stream.
  assert.equal(terminalLiveness({ ...silent, probe: now - terminalProbeMs, lastReceived: now - 1 }), "idle");
  assert.equal(terminalLiveness({ ...silent, probe: now - 1000 }), "idle");
  assert.equal(terminalLiveness({ ...silent, probe: now - terminalProbeMs }), "dead");
});

test("a server that never sent a heartbeat is not judged silent", () => {
  const now = 1_000_000;
  const quiet = { now, lastReceived: 0, lastPing: now, probe: now - 60_000, visible: true, heartbeats: false };
  assert.equal(terminalLiveness(quiet), "idle");
});

test("a page names itself once per terminal, across reconnects", () => {
  const first = terminalViewer("0123456789abcdef");
  assert.match(first, /^web-[0-9a-f]{24}$/);
  assert.equal(terminalViewer("0123456789abcdef"), first);
  assert.notEqual(terminalViewer("fedcba9876543210"), first);
  assert.ok(terminalStreamPath("0123456789abcdef", 5, first).endsWith("?after=5&viewer=" + first));
});

test("terminalViewer map stays bounded across many closed terminals", () => {
  const old = terminalViewer("stale-terminal-0");
  for (let i = 1; i <= 200; i++) terminalViewer(`ephemeral-terminal-${i}`);
  // After 200 distinct terminals, the evicted earliest terminal receives a fresh viewer ID.
  assert.notEqual(terminalViewer("stale-terminal-0"), old, "terminalViewers map must evict old entries instead of growing unboundedly");
});
