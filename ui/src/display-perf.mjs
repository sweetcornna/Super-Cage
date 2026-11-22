// Timing counters for the live panes, readable from tests and from the
// console as window.__cagePerf. Nothing here is shown in the interface.
//
//   __cagePerf.displays        one entry per display connection, newest last
//   __cagePerf.terminals       one entry per terminal view
//   __cagePerf.summary()       the same with percentiles, as plain data
//
// Samples are bounded, so a pane left open for days costs a fixed amount.

const sampleLimit = 240;
const entryLimit = 24;

function record(list, value, limit = sampleLimit) {
  list.push(value);
  if (list.length > limit) list.splice(0, list.length - limit);
}

export function percentile(values, fraction) {
  if (!values.length) return null;
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.min(sorted.length - 1, Math.max(0, Math.ceil(fraction * sorted.length) - 1))];
}

const round = (value) => value === null ? null : Math.round(value * 10) / 10;

function describe(values) {
  return { count: values.length, p50: round(percentile(values, 0.5)), p95: round(percentile(values, 0.95)), max: round(values.length ? Math.max(...values) : null) };
}

export function createPerfRegistry(now = () => globalThis.performance?.now() ?? Date.now()) {
  const displays = [], terminals = [];
  const reconnects = {};
  let sequence = 0;

  function display(kind, url = "") {
    const previous = reconnects[kind];
    reconnects[kind] = previous === undefined ? 0 : previous + 1;
    const entry = {
      id: ++sequence, kind, url, reconnect: reconnects[kind], protocol: "",
      openedAt: now(), connectedAt: 0, firstFrameAt: 0, closedAt: 0, closeCode: 0,
      framesPainted: 0, framesHeld: 0, bytesReceived: 0, messages: 0, holds: 0, resumes: 0,
      updateIntervals: [], inputToFrame: [], handleMs: [], drawMs: [], lastFrameAt: 0, pendingInputAt: 0,
      connected(protocol = "") { entry.protocol = protocol; entry.connectedAt = now(); },
      received(bytes) { entry.messages++; entry.bytesReceived += bytes; },
      // A paint follows input when input arrived since the last paint.
      input() { if (!entry.pendingInputAt) entry.pendingInputAt = now(); },
      painted() {
        const at = now();
        entry.framesPainted++;
        if (!entry.firstFrameAt) entry.firstFrameAt = at;
        if (entry.lastFrameAt) record(entry.updateIntervals, at - entry.lastFrameAt);
        entry.lastFrameAt = at;
        if (entry.pendingInputAt) { record(entry.inputToFrame, at - entry.pendingInputAt); entry.pendingInputAt = 0; }
      },
      held() { entry.framesHeld++; },
      // Main-thread time spent parsing and decoding stream messages, and
      // drawing decoded rectangles: the paint path.
      handled(ms) { record(entry.handleMs, ms); },
      drew(ms) { record(entry.drawMs, ms); },
      hidden() { entry.holds++; entry.pendingInputAt = 0; entry.lastFrameAt = 0; },
      shown() { entry.resumes++; entry.lastFrameAt = 0; },
      closed(code = 0) { if (!entry.closedAt) { entry.closedAt = now(); entry.closeCode = code; } },
      summary() {
        const live = (entry.closedAt || now()) - (entry.connectedAt || entry.openedAt);
        return {
          id: entry.id, kind: entry.kind, reconnect: entry.reconnect, protocol: entry.protocol,
          open: !entry.closedAt, closeCode: entry.closeCode,
          connectMs: entry.connectedAt ? round(entry.connectedAt - entry.openedAt) : null,
          firstFrameMs: entry.firstFrameAt ? round(entry.firstFrameAt - entry.openedAt) : null,
          framesPainted: entry.framesPainted, framesHeld: entry.framesHeld,
          bytesReceived: entry.bytesReceived, messages: entry.messages,
          holds: entry.holds, resumes: entry.resumes,
          framesPerSecond: live > 0 ? round(entry.framesPainted * 1000 / live) : null,
          updateIntervalMs: describe(entry.updateIntervals), inputToFrameMs: describe(entry.inputToFrame),
          handleMs: describe(entry.handleMs), drawMs: describe(entry.drawMs),
        };
      },
    };
    record(displays, entry, entryLimit);
    return entry;
  }

  function terminal(id, renderer = "dom") {
    const entry = {
      id, renderer, opened: 0, suspended: 0, resumed: 0, bytes: 0, writes: 0, writeMs: [],
      stream(event) { if (event in entry) entry[event]++; },
      wrote(bytes, ms) { entry.writes++; entry.bytes += bytes; if (ms !== undefined) record(entry.writeMs, ms); },
      summary() {
        return { id: entry.id, renderer: entry.renderer, opened: entry.opened, suspended: entry.suspended, resumed: entry.resumed,
          bytes: entry.bytes, writes: entry.writes, writeMs: describe(entry.writeMs) };
      },
    };
    const index = terminals.findIndex((item) => item.id === id);
    if (index >= 0) terminals.splice(index, 1);
    record(terminals, entry, entryLimit);
    return entry;
  }

  return {
    displays, terminals, reconnects, display, terminal,
    summary() {
      return { displays: displays.map((entry) => entry.summary()), terminals: terminals.map((entry) => entry.summary()), reconnects: { ...reconnects } };
    },
  };
}

// The page-wide registry. Tests create their own with createPerfRegistry.
export function cagePerf() {
  const scope = globalThis;
  if (!scope.__cagePerf) Object.defineProperty(scope, "__cagePerf", { value: createPerfRegistry(), configurable: true });
  return scope.__cagePerf;
}
