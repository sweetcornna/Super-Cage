import assert from "node:assert/strict";
import test from "node:test";
import {
  createDisplaySessions, displayControlProtocol, displayFrameGateMs, displayHoldMs, displayProtocols, uniformPixels,
} from "./display-session.mjs";
import { createPerfRegistry } from "./display-perf.mjs";

// A stand-in for the RFB client and the server behind it. The opener connects
// on demand; the tests drive the events a real stream would deliver.
function harness({ protocol = displayControlProtocol } = {}) {
  let clock = 1_000;
  const timers = new Map();
  let nextTimer = 0;
  const opened = [];
  const perf = createPerfRegistry(() => clock);
  const host = () => ({ parentNode: null, remove() { this.parentNode = null; } });
  const sessions = createDisplaySessions({
    now: () => clock,
    setTimer: (fn, ms) => { const id = ++nextTimer; timers.set(id, { fn, at: clock + ms }); return id; },
    clearTimer: (id) => timers.delete(id),
    createHost: host,
    open(session) {
      const client = {
        cageProtocol: protocol, sent: [], paused: false, disconnected: 0,
        cageSendControl(message) { client.sent.push(message); return true; },
        cageSetUpdatesPaused(value) { client.paused = value; },
        disconnect() { client.disconnected++; },
      };
      session.client = client;
      session.perf = perf.display(session.options.desktop ? "desktop" : "browser", session.key);
      session.info = { density: 2, followScreen: true };
      opened.push(session);
    },
  });
  const advance = (ms) => {
    clock += ms;
    for (const [id, timer] of [...timers]) if (timer.at <= clock) { timers.delete(id); timer.fn(); }
  };
  const box = () => ({ appendChild(child) { child.parentNode = this; } });
  const view = () => {
    const events = [];
    return { events, connected(_session, detail) { events.push(["connected", detail]); }, lost(_session, detail) { events.push(["lost", detail]); }, sizeError() {} };
  };
  return { sessions, opened, advance, box, view, perf, now: () => clock };
}

test("the client offers the display control protocol after the older one", () => {
  assert.deepEqual(displayProtocols, ["binary", "cage-display.v1"]);
});

test("a hidden pane keeps its stream and shows it again without connecting", () => {
  const h = harness();
  const first = h.view();
  const session = h.sessions.acquire("ws://vnc", { desktop: false });
  const detach = session.attach(h.box(), first);
  session.handle("connect");
  assert.deepEqual(first.events, [["connected", { resumed: false, hiddenFor: 0 }]]);
  detach();
  assert.equal(session.state, "connected");
  assert.deepEqual(session.client.sent, [{ type: "visibility", visible: false }]);
  assert.equal(session.client.paused, true, "a hidden stream asks for no pixels");
  assert.equal(session.host.parentNode, null);
  h.advance(5_000);
  const again = h.sessions.acquire("ws://vnc", { desktop: false });
  assert.equal(again, session, "no second connection");
  const second = h.view();
  const box = h.box();
  again.attach(box, second);
  assert.equal(h.opened.length, 1, "no credentials fetch or handshake on return");
  assert.deepEqual(second.events, [["connected", { resumed: true, hiddenFor: 5_000 }]]);
  assert.deepEqual(session.client.sent.at(-1), { type: "visibility", visible: true });
  assert.equal(session.client.paused, false);
  assert.equal(session.host.parentNode, box, "the canvas moves into the new pane");
  assert.equal(session.holdsFrame(), true, "the last frame stays while the guest restores its window");
  h.advance(displayFrameGateMs);
  assert.equal(session.holdsFrame(), false, "but not for longer than the gate");
  assert.equal(session.perf.holds, 1);
  assert.equal(session.perf.resumes, 1);
});

test("an older server gets today's behaviour: hiding closes the stream", () => {
  const h = harness({ protocol: "" });
  const view = h.view();
  const session = h.sessions.acquire("ws://vnc", {});
  const detach = session.attach(h.box(), view);
  session.handle("connect");
  detach();
  assert.equal(session.state, "closed");
  assert.equal(session.client.disconnected, 1);
  assert.deepEqual(session.client.sent, [], "no control message to a server that did not select the protocol");
  const next = h.sessions.acquire("ws://vnc", {});
  assert.notEqual(next, session);
  assert.equal(h.opened.length, 2);
});

test("the server's hidden timeout ends the stream without a reconnect; the next show connects", () => {
  const h = harness();
  const view = h.view();
  const session = h.sessions.acquire("ws://vnc", {});
  const detach = session.attach(h.box(), view);
  session.handle("connect");
  detach();
  // Code 4001 from the server, or any other close while hidden.
  session.handle("disconnect");
  assert.equal(session.state, "closed");
  assert.deepEqual(view.events.map(([name]) => name), ["connected"], "a hidden pane is not told to reconnect");
  assert.equal(h.sessions.size, 0);
  const next = h.sessions.acquire("ws://vnc", {});
  const shown = h.view();
  next.attach(h.box(), shown);
  assert.equal(h.opened.length, 2);
  assert.deepEqual(shown.events, [], "the new connection shows its connecting state until it opens");
});

test("a stream held past the client's own bound is closed: no leaked socket", () => {
  const h = harness();
  const session = h.sessions.acquire("ws://vnc", {});
  const detach = session.attach(h.box(), h.view());
  session.handle("connect");
  detach();
  h.advance(displayHoldMs - 1);
  assert.equal(session.state, "connected");
  h.advance(1);
  assert.equal(session.state, "closed");
  assert.equal(session.client.disconnected, 1);
});

test("a pane closed for good releases its held stream at once", () => {
  const h = harness();
  const session = h.sessions.acquire("ws://desktop", { desktop: true });
  const detach = session.attach(h.box(), h.view());
  session.handle("connect");
  detach();
  h.sessions.release("ws://desktop");
  assert.equal(session.state, "closed");
  assert.equal(session.client.disconnected, 1);
});

test("a pane hidden while still connecting closes that connection", () => {
  const h = harness();
  const session = h.sessions.acquire("ws://vnc", {});
  const detach = session.attach(h.box(), h.view());
  detach();
  assert.equal(session.state, "closed");
  // A connect event that arrives after the close is ignored.
  session.handle("connect");
  assert.equal(session.state, "closed");
});

test("a security failure followed by its disconnect asks the pane to retry once", () => {
  const h = harness();
  const view = h.view();
  const session = h.sessions.acquire("ws://vnc", {});
  session.attach(h.box(), view);
  session.handle("securityfailure");
  session.handle("disconnect");
  assert.deepEqual(view.events, [["lost", { failed: true }]]);
});

test("a visible stream that drops asks its pane to reconnect", () => {
  const h = harness();
  const view = h.view();
  const session = h.sessions.acquire("ws://vnc", {});
  session.attach(h.box(), view);
  session.handle("connect");
  session.handle("disconnect");
  assert.deepEqual(view.events.at(-1), ["lost", { failed: false }]);
});

test("an empty desktop is one colour; a window is not", () => {
  const pixels = (values) => Uint8ClampedArray.from(values.flatMap((value) => [value, value, value, 255]));
  assert.equal(uniformPixels(pixels([0, 0, 1, 2, 0])), true);
  assert.equal(uniformPixels(pixels([0, 0, 0, 40])), false);
});

test("display counters: frames, bytes, intervals, input to the next frame, reconnects", () => {
  let clock = 0;
  const perf = createPerfRegistry(() => clock);
  const first = perf.display("browser");
  first.connected("cage-display.v1");
  first.received(1000);
  clock = 16; first.painted();
  clock = 20; first.input();
  clock = 25; first.input();
  clock = 33; first.painted();
  const summary = first.summary();
  assert.equal(summary.framesPainted, 2);
  assert.equal(summary.bytesReceived, 1000);
  assert.deepEqual(summary.updateIntervalMs, { count: 1, p50: 17, p95: 17, max: 17 });
  assert.deepEqual(summary.inputToFrameMs, { count: 1, p50: 13, p95: 13, max: 13 }, "measured from the first input since the last frame");
  assert.equal(perf.display("browser").reconnect, 1);
  assert.equal(perf.display("desktop").reconnect, 0);
  assert.deepEqual(perf.reconnects, { browser: 1, desktop: 0 });
});
