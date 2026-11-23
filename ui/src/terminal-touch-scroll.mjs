// Touch scrolling for xterm.js terminals. xterm.js 6 scrolls on wheel events
// only, so a finger dragged over a terminal moved nothing: not the scrollback,
// not a full-screen program. A vertical drag becomes what a wheel does there,
// line by line as the finger travels one cell height:
//   - the program reads the mouse (mouse tracking on): one wheel report per
//     line, which xterm encodes itself, the way the program asked;
//   - the alternate screen without the mouse (less, man, a TUI): one arrow key
//     per line, as xterm sends for a wheel there (application cursor keys
//     respected);
//   - the normal screen: the scrollback moves with the finger, and keeps moving
//     a little after a quick flick.
// Taps stay taps (focus, the on-screen keyboard): a touch scrolls only once it
// moved touchSlop pixels, mostly vertically. Nothing runs while nobody touches
// the terminal; a flick ends by itself.

export const touchSlop = 8;
// A flick keeps this share of its speed per 16 ms and stops under the floor.
export const flingFriction = 0.95;
export const flingFloor = 0.05; // px per ms
// At most this many wheel reports or arrow keys for one move event.
export const touchLineBurst = 24;

// What a vertical drag does now, by xterm's own wheel rules.
export function touchScrollMode({ mouseTracking, alternate }) {
  return mouseTracking || alternate ? "wheel" : "lines";
}

// Adds finger travel in pixels (positive: the finger moved up, towards newer
// output) and returns the whole lines it makes; the remainder is kept.
export function takeLines(state, travel, cellHeight) {
  if (!(cellHeight > 0)) return 0;
  state.rest += travel;
  const lines = Math.trunc(state.rest / cellHeight);
  state.rest -= lines * cellHeight;
  return lines;
}

// The speed of a flick after dt milliseconds, or 0 once it is too slow.
export function flingSpeed(velocity, dt) {
  const next = velocity * Math.pow(flingFriction, dt / 16);
  return Math.abs(next) < flingFloor ? 0 : next;
}

// attachTouchScroll(terminal, host) listens on host (an element holding the
// opened terminal) and returns a function that stops listening.
export function attachTouchScroll(terminal, host, {
  frame = (callback) => globalThis.requestAnimationFrame(callback),
  cancelFrame = (id) => globalThis.cancelAnimationFrame(id),
  now = () => globalThis.performance.now(),
} = {}) {
  let touch = null, fling = 0, suppressClick = 0;
  const cellHeight = () => {
    const screen = terminal.element?.querySelector(".xterm-screen");
    const height = screen?.getBoundingClientRect().height || 0;
    return terminal.rows > 0 ? height / terminal.rows : 0;
  };
  const mode = () => touchScrollMode({
    mouseTracking: terminal.modes?.mouseTrackingMode !== "none",
    alternate: terminal.buffer?.active?.type === "alternate",
  });
  const stopFling = () => {
    if (fling) cancelFrame(fling);
    fling = 0;
  };
  // Positive lines show newer output, as a wheel turned down.
  const scroll = (lines, x, y) => {
    if (!lines) return;
    if (mode() === "lines") {
      terminal.scrollLines(lines);
      return;
    }
    const target = terminal.element;
    if (!target) return;
    for (let i = 0; i < Math.min(Math.abs(lines), touchLineBurst); i++) {
      target.dispatchEvent(new WheelEvent("wheel", {
        deltaY: Math.sign(lines), deltaMode: WheelEvent.DOM_DELTA_LINE,
        clientX: x, clientY: y, bubbles: true, cancelable: true,
      }));
    }
  };
  const start = (event) => {
    stopFling();
    if (event.touches.length !== 1) { touch = null; return; }
    const point = event.touches[0];
    touch = { x0: point.clientX, y0: point.clientY, x: point.clientX, y: point.clientY, t: now(),
      moving: false, velocity: 0, lines: { rest: 0 }, cell: cellHeight() };
  };
  const move = (event) => {
    if (!touch) return;
    if (event.touches.length !== 1) { touch = null; return; }
    const point = event.touches[0];
    if (!touch.moving) {
      const dx = point.clientX - touch.x0, dy = point.clientY - touch.y0;
      if (Math.abs(dy) < touchSlop || Math.abs(dy) < Math.abs(dx)) return;
      touch.moving = true;
      touch.y = touch.y0;
    }
    event.preventDefault();
    const at = now(), travel = touch.y - point.clientY;
    const dt = Math.max(1, at - touch.t);
    touch.velocity = 0.8 * (travel / dt) + 0.2 * touch.velocity;
    touch.x = point.clientX;
    touch.y = point.clientY;
    touch.t = at;
    scroll(takeLines(touch.lines, travel, touch.cell), touch.x, touch.y);
  };
  const end = () => {
    const done = touch;
    touch = null;
    if (!done?.moving) return;
    // A drag is not a tap: the click that may follow does not focus.
    suppressClick = now() + 400;
    if (mode() !== "lines" || Math.abs(done.velocity) < flingFloor * 4) return;
    let velocity = done.velocity, last = now();
    const step = () => {
      const at = now(), dt = Math.max(1, at - last);
      last = at;
      scroll(takeLines(done.lines, velocity * dt, done.cell), done.x, done.y);
      velocity = flingSpeed(velocity, dt);
      fling = velocity ? frame(step) : 0;
    };
    fling = frame(step);
  };
  const click = (event) => {
    if (now() < suppressClick) { event.stopPropagation(); event.preventDefault(); }
  };
  host.addEventListener("touchstart", start, { passive: true });
  host.addEventListener("touchmove", move, { passive: false });
  host.addEventListener("touchend", end, { passive: true });
  host.addEventListener("touchcancel", end, { passive: true });
  host.addEventListener("click", click, true);
  return () => {
    stopFling();
    host.removeEventListener("touchstart", start);
    host.removeEventListener("touchmove", move);
    host.removeEventListener("touchend", end);
    host.removeEventListener("touchcancel", end);
    host.removeEventListener("click", click, true);
  };
}
