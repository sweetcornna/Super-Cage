import assert from "node:assert/strict";
import { test } from "node:test";
import { afterOutcome, backoffDelay, initialPoll, pollQuery, requestDeadline, requestDeadlineMs } from "./session-poll.mjs";

const at = (started, ended, wait = 0, interval = 5000) => ({ started, ended, wait, interval });

test("the first request is plain; a revision turns on the hold", () => {
  let poll = initialPoll();
  assert.deepEqual(pollQuery(poll), { after: "", wait: 0 });
  const answer = afterOutcome(poll, { status: 200, revision: "a".repeat(32) }, at(0, 120));
  poll = answer.poll;
  assert.equal(poll.mode, "held");
  assert.equal(answer.delay, 880, "at least 1 s between the starts of two requests");
  assert.deepEqual(pollQuery(poll), { after: "a".repeat(32), wait: 25 });
});

test("a held answer goes again at once, never sooner than 1 s after the last start", () => {
  const poll = { mode: "held", revision: "r1", failures: 0 };
  assert.equal(afterOutcome(poll, { status: 200, revision: "r2" }, at(0, 9000, 25)).delay, 0);
  assert.equal(afterOutcome(poll, { status: 200, revision: "r2" }, at(0, 300, 25)).delay, 700);
  const unchanged = afterOutcome(poll, { status: 204 }, at(0, 25000, 25));
  assert.equal(unchanged.delay, 0);
  assert.equal(unchanged.poll.revision, "r1");
});

test("a 204 that comes back early was not held: wait the usual interval", () => {
  const poll = { mode: "held", revision: "r1", failures: 0 };
  assert.equal(afterOutcome(poll, { status: 204 }, at(0, 40, 25)).delay, 5000);
  assert.equal(afterOutcome(poll, { status: 204 }, at(0, 40, 25, 4000)).delay, 4000);
  assert.equal(afterOutcome(poll, { status: 204 }, at(0, 24100, 25)).delay, 0, "within the last second counts as held");
});

test("an older server: no revision, or 400/404 to the held form, means plain polling for good", () => {
  const none = afterOutcome(initialPoll(), { status: 200 }, at(0, 50));
  assert.equal(none.poll.mode, "plain");
  assert.equal(none.delay, 5000);
  assert.deepEqual(pollQuery(none.poll), { after: "", wait: 0 });
  const stays = afterOutcome(none.poll, { status: 200, revision: "r9" }, at(0, 50));
  assert.equal(stays.poll.mode, "plain", "plain is sticky for this loop");
  for (const status of [400, 404]) {
    const refused = afterOutcome({ mode: "held", revision: "r1", failures: 0 }, { status }, at(0, 30, 25));
    assert.equal(refused.poll.mode, "plain");
    assert.equal(refused.delay, 970, "the plain request follows at once (1 s spacing)");
  }
  assert.equal(afterOutcome(initialPoll({ held: false }), { status: 200, revision: "x" }, at(0, 10)).poll.mode, "plain");
});

test("errors back off 1, 2, 5 s and more, capped, and a success resets", () => {
  let poll = { mode: "held", revision: "r1", failures: 0 };
  const delays = [];
  for (let i = 0; i < 8; i++) {
    const next = afterOutcome(poll, { error: true }, at(0, 10, 25));
    delays.push(next.delay);
    poll = next.poll;
  }
  assert.deepEqual(delays, [1000, 2000, 5000, 10000, 20000, 30000, 30000, 30000]);
  assert.equal(poll.revision, "r1", "the hold resumes after recovery");
  const ok = afterOutcome(poll, { status: 204 }, at(0, 25000, 25));
  assert.equal(ok.poll.failures, 0);
  assert.equal(backoffDelay(0), 1000);
  // A 404 in plain mode is an error, not a reason to change mode.
  const plain = afterOutcome({ mode: "plain", revision: "", failures: 0 }, { status: 404 }, at(0, 10));
  assert.equal(plain.poll.failures, 1);
  assert.equal(plain.delay, 1000);
});

test("numeric view revisions are kept as text", () => {
  const view = afterOutcome(initialPoll(), { status: 200, revision: 7 }, at(0, 10, 0, 1000));
  assert.deepEqual(pollQuery(view.poll), { after: "7", wait: 25 });
});

test("a request's deadline: its wait plus 10 s, or 35 s without a wait", () => {
  assert.equal(requestDeadlineMs(25), 35000);
  assert.equal(requestDeadlineMs(5), 15000);
  assert.equal(requestDeadlineMs(0), 35000);
});

test("the deadline signal follows its parent and expires on its own", () => {
  const timers = [];
  const clock = { setTimer: (fn) => { timers.push(fn); return timers.length; }, clearTimer: (id) => { timers[id - 1] = null; } };
  const parent = new AbortController();
  const first = requestDeadline(parent.signal, 1000, clock);
  parent.abort(new Error("stop"));
  assert.equal(first.signal.aborted, true);
  assert.equal(first.expired(), false, "aborted by its parent, not expired");
  first.done();

  const other = new AbortController();
  const second = requestDeadline(other.signal, 1000, clock);
  timers[1]();
  assert.equal(second.signal.aborted, true);
  assert.equal(second.expired(), true);
  assert.equal(other.signal.aborted, false, "the parent goes on");
  second.done();

  const third = requestDeadline(other.signal, 1000, clock);
  third.done();
  assert.equal(timers[2], null, "done clears the timer");
  other.abort();
  assert.equal(third.signal.aborted, false, "done stops following the parent");
  assert.equal(requestDeadline(parent.signal, 1000, clock).signal.aborted, true, "an aborted parent aborts at once");
});
