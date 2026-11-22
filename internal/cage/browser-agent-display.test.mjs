// Tests for the browser agent's display code. The agent is a program, not a
// module, so the code under test is cut out of its source between two lines
// that frame it and run with a fake xrandr and a fake /proc. Set
// CAGE_AGENT_SOURCE to run the same tests against another copy of the agent.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { fileURLToPath } from "node:url";

const source = readFileSync(process.env.CAGE_AGENT_SOURCE ?? fileURLToPath(new URL("./browser-agent.mjs", import.meta.url)), "utf8");

function between(start, end) {
  const from = source.indexOf(start);
  const to = source.indexOf(end, from);
  assert.ok(from >= 0 && to > from, `the agent no longer has ${start} before ${end}`);
  return source.slice(from, to);
}

const tick = () => new Promise(resolve => setImmediate(resolve));
async function quiet(calls) {
  for (let seen = -1; seen !== calls.length;) {
    seen = calls.length;
    for (let i = 0; i < 5; i++) await tick();
  }
}

// An X server with the modes it has, the modes added to VNC-0 and the mode in
// use. It refuses what a real one refuses, and every call takes a few ticks so
// that calls which overlap would interleave.
function display({ server = [], output = [] } = {}) {
  const state = { server: new Set(server), output: new Set(output), current: "" };
  const calls = [];
  const refuse = message => { throw new Error(message); };
  const runFile = async (file, args) => {
    assert.equal(file, "/usr/bin/xrandr");
    calls.push(args.join(" "));
    await tick();
    const [action, first, second, third] = args;
    switch (action) {
      case "--query":
        return { stdout: ["Screen 0: minimum 64 x 64", "VNC-0 connected primary", ...[...state.output].map(mode => `   ${mode}     60.01*+`), "   1024x768     60.00", ""].join("\n") };
      case "--newmode":
        if (state.server.has(first)) refuse("BadName");
        state.server.add(first);
        break;
      case "--addmode":
        if (!state.server.has(second)) refuse("cannot find mode");
        state.output.add(second);
        break;
      case "--output":
        if (!state.output.has(third)) refuse("cannot find mode");
        state.current = third;
        break;
      case "--delmode":
        if (state.current === second) refuse("in use");
        state.output.delete(second);
        break;
      case "--rmmode":
        if (state.output.has(first)) refuse("in use");
        state.server.delete(first);
        break;
    }
    return { stdout: "" };
  };
  return { state, calls, runFile };
}

function resizer(options) {
  const x = display(options);
  const timers = [];
  const setTimeout = (fn, ms) => { const timer = { fn, ms, cleared: false, unref() {} }; timers.push(timer); return timer; };
  const clearTimeout = timer => { if (timer) timer.cleared = true; };
  const body = between('let displaySize = "";', "let mutations = Promise.resolve();");
  const make = new Function("runFile", "displaySettings", "displayDensity", "displayScale", "setTimeout", "clearTimeout", body + "\nreturn resizeDisplay;");
  const resizeDisplay = make(x.runFile, { available: false }, value => value, 2, setTimeout, clearTimeout);
  // The pending removal of old modes, as the timer would run it.
  const sweep = async () => {
    const pending = timers.filter(timer => !timer.cleared);
    assert.equal(pending.length, 1, "exactly one removal should be waiting");
    pending[0].cleared = true;
    pending[0].fn();
    await quiet(x.calls);
  };
  const startSweep = () => {
    const pending = timers.filter(timer => !timer.cleared);
    assert.equal(pending.length, 1, "exactly one removal should be waiting");
    pending[0].cleared = true;
    pending[0].fn();
  };
  return { ...x, resizeDisplay, timers, sweep, startSweep, take: () => x.calls.splice(0) };
}

const size = (w, h) => [w, h];
const modeCalls = name => [/^--newmode cage-/, `--addmode VNC-0 ${name}`, `--output VNC-0 --mode ${name}`];
function assertCalls(actual, expected) {
  assert.equal(actual.length, expected.length, `calls: ${JSON.stringify(actual)}`);
  expected.forEach((want, i) => {
    if (want instanceof RegExp) assert.match(actual[i], want);
    else assert.equal(actual[i], want);
  });
}

test("the first resize reads the modes once and creates the size in three calls", async () => {
  const r = resizer();
  await r.resizeDisplay(...size(1000, 800));
  assertCalls(r.take(), ["--query", ...modeCalls("cage-1000x800")]);
  assert.equal(r.state.current, "cage-1000x800");
  // No removal in front of the answer.
  assert.equal(r.calls.length, 0);
});

test("a later size needs neither a query nor a removal before it is shown", async () => {
  const r = resizer();
  await r.resizeDisplay(...size(1000, 800));
  r.take();
  await r.resizeDisplay(...size(900, 700));
  assertCalls(r.take(), modeCalls("cage-900x700"));
});

test("returning to a size that is still defined is one call", async () => {
  const r = resizer();
  await r.resizeDisplay(...size(1000, 800));
  await r.resizeDisplay(...size(900, 700));
  r.take();
  await r.resizeDisplay(...size(1000, 800));
  assertCalls(r.take(), ["--output VNC-0 --mode cage-1000x800"]);
  await r.resizeDisplay(...size(900, 700));
  assertCalls(r.take(), ["--output VNC-0 --mode cage-900x700"]);
});

test("a viewer attaching at the unchanged size still restores the full rate, in one call", async () => {
  const r = resizer();
  await r.resizeDisplay(...size(1000, 800));
  r.take();
  await r.resizeDisplay(...size(1000, 800));
  assertCalls(r.take(), ["--output VNC-0 --mode cage-1000x800"]);
});

test("a mode whose name only starts like the wanted one does not count as it", async () => {
  const r = resizer({ server: ["cage-1000x80"], output: ["cage-1000x80"] });
  await r.resizeDisplay(...size(1000, 800));
  assertCalls(r.take(), ["--query", ...modeCalls("cage-1000x800")]);
  assert.equal(r.state.current, "cage-1000x800");
});

test("old modes go after the sizes settle, newest four stay, the mode in use never goes", async () => {
  const r = resizer();
  for (const [w, h] of [[1000, 800], [900, 700], [800, 600], [700, 500]]) await r.resizeDisplay(w, h);
  assert.equal(r.timers.filter(timer => !timer.cleared).length, 0, "four sizes need no removal");
  await r.resizeDisplay(600, 490);
  r.take();
  assert.deepEqual([...r.state.output], ["cage-1000x800", "cage-900x700", "cage-800x600", "cage-700x500", "cage-600x490"]);
  await r.sweep();
  assertCalls(r.take(), ["--delmode VNC-0 cage-1000x800", "--rmmode cage-1000x800"]);
  assert.deepEqual([...r.state.output], ["cage-900x700", "cage-800x600", "cage-700x500", "cage-600x490"]);
  assert.deepEqual([...r.state.server], [...r.state.output]);
  assert.equal(r.state.current, "cage-600x490");
  // The sweep waits for quiet: a resize in between restarts it.
  await r.resizeDisplay(500, 485);
  await r.resizeDisplay(400, 482);
  assert.equal(r.timers.filter(timer => !timer.cleared).length, 1);
  await r.sweep();
  assert.deepEqual([...r.state.output], ["cage-700x500", "cage-600x490", "cage-500x485", "cage-400x482"]);
  assert.equal(r.state.current, "cage-400x482");
});

test("a resize that arrives during the removal of old modes goes first and pauses it", async () => {
  const r = resizer();
  for (const [w, h] of [[1000, 800], [900, 700], [800, 600], [700, 500], [600, 490], [500, 485], [400, 482]]) await r.resizeDisplay(w, h);
  r.take();
  // Three old modes wait. The first removal starts...
  r.startSweep();
  await tick();
  // ...and a resize arrives before it is done.
  const resized = r.resizeDisplay(450, 483);
  await resized;
  await quiet(r.calls);
  const calls = r.take();
  const first = calls.indexOf("--delmode VNC-0 cage-1000x800");
  const second = calls.findIndex(call => call.startsWith("--delmode") && call !== "--delmode VNC-0 cage-1000x800");
  const switched = calls.indexOf("--output VNC-0 --mode cage-450x483");
  assert.ok(first === 0 && switched > first, "the resize runs once the removal in flight is done: " + JSON.stringify(calls));
  assert.equal(calls[1], "--rmmode cage-1000x800", "a removal is never split: " + JSON.stringify(calls));
  assert.equal(second, -1, "no further removal ran in front of or after the resize: " + JSON.stringify(calls));
  // The removal resumes once the sizes have settled.
  await r.sweep();
  assert.equal(r.state.output.size, 4);
  assert.equal(r.state.current, "cage-450x483");
});

test("modes left by an earlier agent count as old ones", async () => {
  const left = ["cage-640x480", "cage-800x600", "cage-1024x768", "cage-1280x720", "cage-1366x768"];
  const r = resizer({ server: left, output: left });
  await r.resizeDisplay(...size(1000, 800));
  r.take();
  await r.sweep();
  assert.deepEqual([...r.state.output], ["cage-1024x768", "cage-1280x720", "cage-1366x768", "cage-1000x800"]);
  assert.equal(r.state.current, "cage-1000x800");
});

test("a server that lost its modes is read again, once, and the size still applies", async () => {
  const r = resizer();
  await r.resizeDisplay(...size(1000, 800));
  await r.resizeDisplay(...size(900, 700));
  // The display restarted: the agent remembers modes that are gone.
  r.state.server.clear();
  r.state.output.clear();
  r.take();
  await r.resizeDisplay(...size(1000, 800));
  assert.equal(r.state.current, "cage-1000x800");
  const calls = r.take();
  assert.equal(calls[0], "--output VNC-0 --mode cage-1000x800", "the remembered mode is tried first");
  assert.ok(calls.includes("--query"), "then the server is asked: " + JSON.stringify(calls));
});

test("a mode that exists but was never added is added, not refused", async () => {
  const r = resizer({ server: ["cage-1000x800"] });
  await r.resizeDisplay(...size(1000, 800));
  assert.equal(r.state.current, "cage-1000x800");
});

test("a size that is too small is refused before the display is touched", async () => {
  const r = resizer();
  await r.resizeDisplay(...size(1000, 800));
  r.take();
  await assert.rejects(r.resizeDisplay(100, 100), /Invalid display size/);
  assert.equal(r.calls.length, 0);
  await r.resizeDisplay(...size(900, 700));
  assert.equal(r.state.current, "cage-900x700");
});

test("overlapping resizes run one after another", async () => {
  const r = resizer();
  const first = r.resizeDisplay(...size(1000, 800));
  const second = r.resizeDisplay(...size(900, 700));
  await Promise.all([first, second]);
  assertCalls(r.take(), ["--query", ...modeCalls("cage-1000x800"), ...modeCalls("cage-900x700")]);
});

// The host says how many streams it keeps open for a hidden pane. Those are
// not legacy viewers; anything beyond them is.
function viewers() {
  const row = (n, port) => `  ${n}: 0100007F:${port} 0100007F:C350 01 00000000:00000000 00:00000000 00000000  1000        0 ${n} 1`;
  let table = [];
  const files = {
    "/proc/net/tcp": () => ["  sl  local_address rem_address   st", ...table.map((port, n) => row(n, port))].join("\n"),
    "/proc/net/tcp6": () => "  sl  local_address rem_address   st",
  };
  const readFileSync = file => files[file]();
  const body = between("let browserVNCViewers = false;", "const visibility = new BrowserVisibility(");
  const make = new Function("readFileSync", body + "\nreturn { canHideBrowser, hold(n) { heldBrowserStreams = n; }, held: () => heldBrowserStreams, legacy: () => browserVNCViewers };");
  const agent = make(readFileSync);
  return { ...agent, connect: (...ports) => { table = ports; } };
}

test("with nothing connected the browser may hide", () => {
  const v = viewers();
  v.connect();
  assert.equal(v.canHideBrowser(), true);
});

test("a connection nobody announced is a legacy viewer, as before", () => {
  const v = viewers();
  v.connect("3E1C");
  assert.equal(v.canHideBrowser(), false);
  assert.equal(v.legacy(), true);
});

test("connections to other ports are not viewers", () => {
  const v = viewers();
  v.connect("1F90", "0050");
  assert.equal(v.canHideBrowser(), true);
});

test("a stream the host announced as held does not keep the browser visible", () => {
  const v = viewers();
  v.connect("3E1C");
  v.hold(1);
  assert.equal(v.canHideBrowser(), true);
  assert.equal(v.legacy(), false);
});

test("a legacy viewer next to a held stream still keeps the browser visible", () => {
  const v = viewers();
  v.connect("3E1C", "3E1C");
  v.hold(1);
  assert.equal(v.canHideBrowser(), false);
  assert.equal(v.legacy(), true);
});

test("the announced count shrinks with the streams, so a later legacy viewer is not hidden from", () => {
  const v = viewers();
  v.connect("3E1C");
  v.hold(1);
  assert.equal(v.canHideBrowser(), true);
  v.connect();
  assert.equal(v.canHideBrowser(), true);
  assert.equal(v.held(), 0);
  v.connect("3E1C");
  assert.equal(v.canHideBrowser(), false);
});
