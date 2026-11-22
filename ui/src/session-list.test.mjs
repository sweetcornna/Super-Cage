import assert from "node:assert/strict";
import { test } from "node:test";
import { createSessionList } from "./session-list.mjs";

// A fake clock and a request queue: each request waits until the test answers it.
function harness() {
  let time = 0;
  const timers = [];
  const calls = [];
  const env = {
    now: () => time,
    setTimer: (fn, ms) => { const timer = { at: time + ms, fn }; timers.push(timer); return timer; },
    clearTimer: (timer) => { const at = timers.indexOf(timer); if (at >= 0) timers.splice(at, 1); },
    request: (path, { signal }) => new Promise((resolve, reject) => {
      const call = { path, signal, resolve, reject };
      signal.addEventListener("abort", () => { call.aborted = true; reject(new DOMException("aborted", "AbortError")); });
      calls.push(call);
    }),
  };
  const flush = () => new Promise((resolve) => setTimeout(resolve, 0));
  const advance = async (ms) => {
    await flush();
    const until = time + ms;
    for (;;) {
      timers.sort((a, b) => a.at - b.at);
      const next = timers[0];
      if (!next || next.at > until) break;
      timers.shift();
      time = Math.max(time, next.at);
      next.fn();
      await flush();
    }
    time = until;
    await flush();
  };
  return { env, calls, advance, flush, inFlight: () => calls.filter((call) => !call.done && !call.aborted) };
}
const answer = (call, value) => { call.done = true; call.resolve(value); };
const fail = (call, status, message = "失败") => { call.done = true; call.reject(Object.assign(new Error(message), { status })); };
const query = (call) => Object.fromEntries(new URL(call.path, "http://x").searchParams);
const rev = (n) => String(n).repeat(32).slice(0, 32);

test("held list: one request in flight, unchanged answers change nothing", async () => {
  const h = harness();
  const list = createSessionList(h.env);
  let emits = 0;
  list.subscribe(() => emits++);
  list.start();
  await h.advance(0);
  assert.equal(h.calls.length, 1);
  assert.deepEqual(query(h.calls[0]), { providers: "claude,codex" });
  answer(h.calls[0], { sessions: [{ id: "a", title: "A" }], notice: "", revision: rev(1) });
  await h.flush();
  assert.equal(emits, 1);
  const first = list.getSnapshot();
  assert.equal(first.held, true);
  await h.advance(999);
  assert.equal(h.calls.length, 1, "never sooner than 1 s after the last start");
  await h.advance(1);
  assert.equal(h.calls.length, 2);
  assert.deepEqual(query(h.calls[1]), { providers: "claude,codex", after: rev(1), wait: "25" });
  await h.advance(25000);
  answer(h.calls[1], null);
  await h.flush();
  assert.equal(list.getSnapshot(), first, "a 204 keeps the same snapshot");
  await h.advance(0);
  assert.equal(h.calls.length, 3, "the next held request goes at once");
  answer(h.calls[2], { sessions: [{ id: "a", title: "A" }], notice: "", revision: rev(1) });
  await h.flush();
  assert.equal(list.getSnapshot(), first, "the same content keeps the same snapshot");
  assert.equal(emits, 1);
  await h.advance(1000);
  answer(h.calls[3], { sessions: [{ id: "a", title: "A" }, { id: "b", title: "B" }], notice: "", revision: rev(2) });
  await h.flush();
  assert.equal(emits, 2);
  assert.equal(list.getSnapshot().sessions[0], first.sessions[0], "unchanged rows keep their objects");
  assert.equal(h.inFlight().length <= 1, true);
  list.stop();
});

test("capabilities come from the answer; an older server falls back to 5 s polling", async () => {
  const h = harness();
  const list = createSessionList(h.env);
  list.start();
  await h.advance(0);
  answer(h.calls[0], { sessions: [], notice: "" });
  await h.flush();
  assert.equal(list.getSnapshot().capabilities.known, false);
  assert.equal(list.getSnapshot().held, false);
  await h.advance(4999);
  assert.equal(h.calls.length, 1);
  await h.advance(1);
  assert.equal(h.calls.length, 2);
  assert.deepEqual(query(h.calls[1]), { providers: "claude,codex" });
  answer(h.calls[1], { sessions: [], notice: "", capabilities: { codex_sessions: true } });
  await h.flush();
  assert.equal(list.getSnapshot().capabilities.sessions, true);
  list.stop();
});

test("400 to the held form drops to plain polling without an error", async () => {
  const h = harness();
  const list = createSessionList(h.env);
  list.start();
  await h.advance(0);
  answer(h.calls[0], { sessions: [], notice: "", revision: rev(1) });
  await h.advance(1000);
  fail(h.calls[1], 400, "会话列表版本无效");
  await h.flush();
  assert.equal(list.getSnapshot().error, "");
  assert.equal(list.mode, "plain");
  await h.advance(1000);
  assert.deepEqual(query(h.calls[2]), { providers: "claude,codex" });
  list.stop();
});

test("errors back off and mark nothing running; refresh replaces the held request", async () => {
  const h = harness();
  const list = createSessionList(h.env);
  list.start();
  await h.advance(0);
  answer(h.calls[0], { sessions: [{ id: "a", running: true, active: true }], notice: "", revision: rev(1) });
  await h.advance(1000);
  fail(h.calls[1], 502, "后台暂不可用");
  await h.flush();
  assert.equal(list.getSnapshot().error, "后台暂不可用");
  assert.equal(list.getSnapshot().sessions[0].running, false);
  await h.advance(999);
  assert.equal(h.calls.length, 2, "backs off 1 s");
  await h.advance(1);
  fail(h.calls[2], 502, "后台暂不可用");
  await h.advance(1999);
  assert.equal(h.calls.length, 3, "then 2 s");
  await h.advance(1);
  assert.equal(h.calls.length, 4);
  // A refresh drops the held request and asks again (1 s after the last start).
  list.refresh({ user: true });
  assert.equal(h.calls[3].aborted, true);
  assert.equal(list.getSnapshot().loading, true);
  await h.advance(1000);
  assert.equal(h.inFlight().length, 1);
  answer(h.calls[4], { sessions: [], notice: "", revision: rev(3) });
  await h.flush();
  assert.equal(list.getSnapshot().loading, false);
  assert.equal(list.getSnapshot().error, "");
  list.stop();
  assert.equal(h.inFlight().length, 0, "stop aborts the held request");
  await h.advance(60000);
  assert.equal(h.calls.length, 5, "nothing runs after stop");
});

test("a notice shows no session as running", async () => {
  const h = harness();
  const list = createSessionList(h.env);
  list.start();
  await h.advance(0);
  answer(h.calls[0], { sessions: [{ id: "a", running: true, active: true }], notice: "无法确认", revision: rev(1) });
  await h.flush();
  assert.equal(list.getSnapshot().sessions[0].active, false);
  assert.equal(list.getSnapshot().error, "无法确认");
  list.stop();
});

test("a held request that never answers is dropped after its wait plus 10 s and asked again", async () => {
  const h = harness();
  const list = createSessionList(h.env);
  list.start();
  await h.advance(0);
  answer(h.calls[0], { sessions: [{ id: "a", running: true }], notice: "", revision: rev(1) });
  await h.advance(1000);
  assert.equal(query(h.calls[1]).wait, "25");
  // A half-open connection: no answer, no error.
  await h.advance(34999);
  assert.equal(h.inFlight().length, 1, "still waiting within wait + 10 s");
  await h.advance(1);
  assert.equal(h.calls[1].aborted, true, "dropped at wait + 10 s");
  assert.equal(list.getSnapshot().error, "会话列表暂时没有回应，正在重试");
  assert.equal(list.getSnapshot().sessions[0].running, false);
  await h.advance(999);
  assert.equal(h.calls.length, 2, "backs off 1 s like any error");
  await h.advance(1);
  assert.equal(h.calls.length, 3);
  assert.equal(query(h.calls[2]).after, rev(1), "still held: the revision is kept");
  answer(h.calls[2], { sessions: [{ id: "a", running: true }], notice: "", revision: rev(2) });
  await h.flush();
  assert.equal(list.getSnapshot().error, "");
  // An answered request leaves no deadline behind.
  await h.advance(1000);
  answer(h.calls[3], null);
  await h.advance(40000);
  assert.notEqual(h.calls[3].aborted, true, "the deadline of an answered request does not fire");
  list.stop();
});

test("a plain request gets 35 s too", async () => {
  const h = harness();
  const list = createSessionList(h.env);
  list.start();
  await h.advance(0);
  await h.advance(35000);
  assert.equal(h.calls[0].aborted, true);
  assert.equal(list.getSnapshot().error, "会话列表暂时没有回应，正在重试");
  list.stop();
});

test("the user's retry asks at once; an action's refresh keeps the 1 s spacing", async () => {
  const h = harness();
  const list = createSessionList(h.env);
  list.start();
  await h.advance(0);
  answer(h.calls[0], { sessions: [], notice: "", revision: rev(1) });
  await h.advance(200);
  list.refresh();
  await h.advance(799);
  assert.equal(h.calls.length, 1, "an action's refresh waits for 1 s after the last start");
  await h.advance(1);
  assert.equal(h.calls.length, 2);
  await h.advance(100);
  list.refresh({ user: true });
  assert.equal(h.calls[1].aborted, true);
  await h.advance(0);
  assert.equal(h.calls.length, 3, "the user's retry goes at once");
  assert.equal(h.inFlight().length, 1);
  list.stop();
});

test("the providers follow Codex presence: another set drops the held request and its revision", async () => {
  const h = harness();
  const list = createSessionList({ ...h.env, providers: "claude" });
  list.start();
  await h.advance(0);
  assert.deepEqual(query(h.calls[0]), { providers: "claude" });
  answer(h.calls[0], { sessions: [], notice: "", revision: rev(1) });
  await h.advance(1000);
  assert.deepEqual(query(h.calls[1]), { providers: "claude", after: rev(1), wait: "25" });
  list.setProviders("claude");
  await h.flush();
  assert.equal(h.calls[1].aborted, undefined, "the same set changes nothing");
  list.setProviders("claude,codex");
  await h.flush();
  assert.equal(h.calls[1].aborted, true);
  await h.advance(1000);
  assert.equal(h.inFlight().length, 1);
  assert.deepEqual(query(h.calls[2]), { providers: "claude,codex" }, "a fresh list, without the old revision");
  answer(h.calls[2], { sessions: [{ id: "c", provider: "codex" }], notice: "", revision: rev(2) });
  await h.flush();
  assert.equal(list.getSnapshot().sessions.length, 1);
  list.stop();
  list.setProviders("claude");
  await h.advance(5000);
  assert.equal(h.calls.length, 3, "a stopped list asks for nothing");
  list.start();
  await h.advance(0);
  assert.deepEqual(query(h.calls[3]), { providers: "claude" });
  list.stop();
});
