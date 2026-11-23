import assert from "node:assert/strict";
import test from "node:test";
import { attachTouchScroll, flingSpeed, takeLines, touchScrollMode, touchSlop } from "./terminal-touch-scroll.mjs";

test("a drag scrolls like a wheel: reports, arrow keys or the scrollback", () => {
  assert.equal(touchScrollMode({ mouseTracking: true, alternate: true }), "wheel");
  assert.equal(touchScrollMode({ mouseTracking: true, alternate: false }), "wheel");
  assert.equal(touchScrollMode({ mouseTracking: false, alternate: true }), "wheel");
  assert.equal(touchScrollMode({ mouseTracking: false, alternate: false }), "lines");
});

test("finger travel makes whole lines and keeps the rest", () => {
  const state = { rest: 0 };
  assert.equal(takeLines(state, 10, 16), 0);
  assert.equal(takeLines(state, 10, 16), 1);
  assert.equal(state.rest, 4);
  assert.equal(takeLines(state, -40, 16), -2);
  assert.equal(takeLines(state, 50, 0), 0, "no cell height, no lines");
});

test("a flick slows down and stops", () => {
  let velocity = 2, steps = 0;
  while (velocity && steps < 1000) { velocity = flingSpeed(velocity, 16); steps++; }
  assert.equal(velocity, 0);
  assert.ok(steps > 10 && steps < 200, "stops after " + steps + " frames");
});

// A host element and a terminal in the normal screen, 20 rows of 16 px.
function setup(alternate = false, mouse = "none") {
  const listeners = {}, scrolled = [], frames = [], wheels = [];
  const host = {
    addEventListener: (type, fn) => { listeners[type] = fn; },
    removeEventListener: (type) => { delete listeners[type]; },
  };
  const terminal = {
    rows: 20,
    element: { querySelector: () => ({ getBoundingClientRect: () => ({ height: 320 }) }), dispatchEvent: (e) => wheels.push(e) },
    modes: { mouseTrackingMode: mouse },
    buffer: { active: { type: alternate ? "alternate" : "normal" } },
    scrollLines: (n) => scrolled.push(n),
  };
  let clock = 0;
  const stop = attachTouchScroll(terminal, host, {
    frame: (fn) => frames.push(fn),
    cancelFrame: () => { frames.length = 0; },
    now: () => clock,
  });
  const touch = (type, y, x = 100, count = 1) => {
    let prevented = false;
    const touches = Array.from({ length: type === "touchend" ? 0 : count }, () => ({ clientX: x, clientY: y }));
    listeners[type]({ touches, preventDefault: () => { prevented = true; } });
    return prevented;
  };
  return { listeners, scrolled, frames, wheels, touch, stop, tick: (ms) => { clock += ms; } };
}

test("a vertical drag moves the scrollback line by line after the slop", () => {
  const t = setup();
  t.touch("touchstart", 300);
  assert.equal(t.touch("touchmove", 300 - touchSlop + 1), false, "inside the slop the page keeps the touch");
  t.tick(16);
  assert.equal(t.touch("touchmove", 260), true, "a drag is the terminal's");
  t.tick(16);
  t.touch("touchmove", 200);
  assert.deepEqual(t.scrolled, [2, 4], "finger up shows newer lines, 16 px a line");
  t.touch("touchmove", 280);
  assert.deepEqual(t.scrolled, [2, 4, -4], "the 4 px left over count against the way back");
});

test("a horizontal swipe, a pinch and a tap leave the terminal alone", () => {
  const t = setup();
  t.touch("touchstart", 300, 100);
  assert.equal(t.touch("touchmove", 290, 160), false);
  t.touch("touchend");
  t.touch("touchstart", 300, 100, 2);
  assert.equal(t.touch("touchmove", 200, 100, 2), false);
  t.touch("touchend");
  t.touch("touchstart", 300);
  t.touch("touchend");
  assert.deepEqual(t.scrolled, []);
  let stopped = false;
  t.listeners.click({ stopPropagation: () => { stopped = true; }, preventDefault() {} });
  assert.equal(stopped, false, "a tap still focuses");
});

test("a flick keeps scrolling the scrollback until it stops; nothing is left running", () => {
  const t = setup();
  t.touch("touchstart", 600);
  for (let y = 580; y >= 300; y -= 40) { t.tick(8); t.touch("touchmove", y); }
  const dragged = t.scrolled.reduce((a, b) => a + b, 0);
  t.touch("touchend");
  let stopped = false;
  t.listeners.click({ stopPropagation: () => { stopped = true; }, preventDefault() {} });
  assert.equal(stopped, true, "the click after a drag does not focus");
  let frames = 0;
  while (t.frames.length && frames < 1000) { const fn = t.frames.shift(); t.tick(16); fn(); frames++; }
  assert.equal(t.frames.length, 0, "the flick ends by itself");
  assert.ok(t.scrolled.reduce((a, b) => a + b, 0) > dragged, "the flick moved further");
  t.stop();
  assert.deepEqual(Object.keys(t.listeners), []);
});

test("in a full-screen program a drag turns the wheel once a line and does not fling", () => {
  const saved = globalThis.WheelEvent;
  globalThis.WheelEvent = class { constructor(type, init) { this.type = type; Object.assign(this, init); } static DOM_DELTA_LINE = 1; };
  try {
    for (const [alternate, mouse] of [[true, "none"], [false, "any"]]) {
      const t = setup(alternate, mouse);
      t.touch("touchstart", 600);
      for (let y = 580; y >= 300; y -= 40) { t.tick(8); t.touch("touchmove", y); }
      t.touch("touchend");
      assert.deepEqual(t.scrolled, [], "a full-screen program has no scrollback to move");
      assert.equal(t.wheels.length, 18, "300 px at 16 px a line");
      assert.ok(t.wheels.every((e) => e.type === "wheel" && e.deltaY === 1 && e.deltaMode === 1 && e.clientX === 100));
      assert.equal(t.frames.length, 0, "keys are not sent after the finger lifts");
      t.touch("touchstart", 300);
      t.touch("touchmove", 340);
      assert.equal(t.wheels.at(-1).deltaY, -1, "finger down turns the wheel up");
    }
  } finally { globalThis.WheelEvent = saved; }
});
