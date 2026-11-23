import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

// One terminal shown on the computer and on the phone at once, the way the
// owner uses it. The terminal is a stand-in: a full-screen program (alternate
// screen, mouse reports) that draws its whole screen at the terminal's size
// and again whenever the size changes, with the size rules of the server:
// the computer's typing takes the size, a phone resize applies at once, a
// computer resize applies while nobody who typed owns the size. Its history
// is kept short, so replays start trimmed: with the modes to set first and a
// repaint (one row less, then the size back) that every viewer sees.
// Walked: the computer shows and types, the phone opens and types, the phone
// is hidden and shown, the computer switches tabs (briefly, and for longer
// than the suspend delay), each page reloads, both type in turn. After every
// step neither view may be blank, unsized or without its stream. Failed
// reads of the status and the terminal list leave the computer's view.
const ID = "two-viewers", OTHER = "two-viewers-other";
const LIMIT = 2500, PREAMBLE = "\x1b[?1049h\x1b[?1002h\x1b[?1006h";
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const until = async (condition, message, tries = 200) => {
  for (let i = 0; i < tries; i++) {
    if (await condition()) return;
    await sleep(25);
  }
  throw new Error(message);
};

// The stand-in terminal.
const term = { chunks: [], start: 0, written: 0, cols: 80, rows: 24, owner: "", typist: false, typed: "", desk: null, phone: null,
  phoneSeen: 0, held: [], sockets: [], deskSizes: [], phoneSizes: [], repaints: 0, phoneText: "" };
const size = () => term.cols + "x" + term.rows;
const frame = () => {
  let out = "\x1b[H\x1b[2J\x1b[1;1HSIZE " + size() + "\x1b[2;1HTYPED " + term.typed.slice(-30);
  for (let row = 3; row < term.rows; row++) out += "\x1b[" + row + ";1H" + "row " + row;
  return out + "\x1b[" + term.rows + ";1HEND " + size();
};
const emit = (text) => {
  const data = Buffer.from(text);
  term.chunks.push(data);
  term.written += data.length;
  // Whole frames leave the kept history, as the server trims it.
  while (term.written - term.start > LIMIT && term.chunks.length > 1) term.start += term.chunks.shift().length;
  for (const entry of term.sockets) if (entry.open && entry.id === ID) entry.socket.send(data);
  for (const release of term.held.splice(0)) release();
};
const kept = () => Buffer.concat(term.chunks);
const setSize = (cols, rows) => {
  if (cols === term.cols && rows === term.rows) return; // the guest signals only a change
  term.cols = cols;
  term.rows = rows;
  emit(frame());
};
const live = (who) => who === "desk" ? term.sockets.some((entry) => entry.open && entry.id === ID) : Date.now() - term.phoneSeen < 30_000;
const deskResize = (cols, rows) => {
  term.deskSizes.push([cols, rows]);
  term.desk = [cols, rows];
  if (term.owner === "desk" || !term.typist || !live(term.owner)) {
    if (term.owner !== "desk") term.typist = false;
    term.owner = "desk";
    setSize(cols, rows);
  }
};
const type = (who, data) => {
  term.typed += data;
  emit("\x1b[2;1H\x1b[2KTYPED " + term.typed.slice(-30));
};
// After a trimmed replay that leaves the viewer in the program's screen the
// server asks for a whole screen: one row less, then the size back.
const repaint = () => {
  term.repaints++;
  const [cols, rows] = [term.cols, term.rows];
  setSize(cols, rows - 1);
  setTimeout(() => setSize(term.owner === "phone" && term.phone ? term.phone[0] : cols, term.owner === "phone" && term.phone ? term.phone[1] : rows), 120);
};
emit(PREAMBLE + frame());

const fixture = await createWorkbench({
  viewport: { width: 1440, height: 900 },
  mockWebSockets: false,
  beforeNavigate: async (page) => {
    await page.context().addInitScript(() => localStorage.setItem("cage-terminal-renderer", "dom"));
    await page.clock.install();
  },
});
const { browser, page, errors, state } = fixture;
const phoneContext = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
const phone = await phoneContext.newPage();
const phoneErrors = [];
phone.on("pageerror", (error) => phoneErrors.push(error.message));
try {
  await page.routeWebSocket("**/api/**", (socket) => {
    if (!socket.url().includes("/terminals/")) return socket.close();
    const url = new URL(socket.url());
    const entry = { socket, id: url.pathname.split("/").at(-2), after: url.searchParams.get("after"), open: true };
    term.sockets.push(entry);
    socket.onClose(() => { entry.open = false; });
    socket.onMessage((message) => {
      const value = JSON.parse(message);
      if (value.type === "ping") socket.send(JSON.stringify({ type: "pong" }));
      if (entry.id !== ID) return;
      if (value.type === "resize") deskResize(value.cols, value.rows);
      if (value.type === "input" && !value.data.startsWith("\x1b")) {
        term.owner = "desk"; term.typist = true;
        if (term.desk) setSize(...term.desk);
        type("desk", value.data);
      }
    });
    if (entry.id !== ID) return void socket.send(JSON.stringify({ type: "history", offset: 0, reset: false, size: 0 }));
    const after = Number(entry.after);
    if (entry.after !== null && after >= term.start && after <= term.written) {
      const tail = kept().subarray(after - term.start);
      socket.send(JSON.stringify({ type: "history", offset: after, reset: false, size: tail.length, answered: term.written }));
      if (tail.length) socket.send(tail);
      return;
    }
    const data = kept();
    socket.send(JSON.stringify({ type: "history", offset: term.start, reset: true, size: data.length, answered: term.written, ...(term.start ? { state: PREAMBLE } : {}) }));
    socket.send(data);
    if (term.start) repaint();
  });
  const phoneRoutes = async (target) => {
    await target.route("**/api/**", (route) => route.fulfill({ json: { ok: true } }));
    await target.route("**/api/mobile/terminals/" + ID + "/output**", async (route) => {
      term.phoneSeen = Date.now();
      const after = Number(new URL(route.request().url()).searchParams.get("after"));
      if (after >= term.start && after === term.written) {
        await new Promise((resolve) => { term.held.push(resolve); setTimeout(resolve, 3000); });
      }
      term.phoneSeen = Date.now();
      if (after === term.written) return route.fulfill({ status: 204 }).catch(() => {});
      const reset = after < term.start || after > term.written;
      const data = reset ? kept() : kept().subarray(after - term.start);
      term.phoneText += data.toString();
      await route.fulfill({ json: { data: data.toString("base64"), next: term.written, reset, closed: false, exit_code: null, answered: term.written, ...(reset && term.start ? { state: PREAMBLE } : {}) } }).catch(() => {});
      if (reset && term.start) repaint();
    });
    await target.route("**/api/mobile/terminals/" + ID + "/input", (route) => {
      const data = route.request().postDataJSON().data;
      if (!data.startsWith("\x1b")) type("phone", data); // the phone's typing never takes the size
      return route.fulfill({ status: 204 });
    });
    await target.route("**/api/mobile/terminals/" + ID + "/resize", (route) => {
      const { cols, rows } = route.request().postDataJSON();
      term.phoneSizes.push([cols, rows]);
      term.phone = [cols, rows];
      term.owner = "phone"; term.typist = false; // applied at once, as for every viewer without identity
      setSize(cols, rows);
      return route.fulfill({ status: 204 });
    });
  };
  await phoneRoutes(phone);
  const openPhone = async (visible = true) => {
    await phone.goto((process.env.CAGE_UI_URL || "http://127.0.0.1:5198") + "/two-viewers-phone");
    await phone.evaluate(async () => {
      const { default: RefreshRuntime } = await import("/@react-refresh");
      RefreshRuntime.injectIntoGlobalHook(window);
      window.$RefreshReg$ = () => {};
      window.$RefreshSig$ = () => (type) => type;
      window.__vite_plugin_react_preamble_installed__ = true;
      await import("/scripts/terminal-touch-fixture.jsx");
    });
    await phone.evaluate(([id, shown]) => window.showTestTerminal({ phone: true, id, visible: shown }), [ID, visible]);
  };
  await phone.route("**/two-viewers-phone", (route) => route.fulfill({ contentType: "text/html",
    body: '<meta name="viewport" content="width=device-width, initial-scale=1"><div id="terminal" style="height:100vh;display:flex;flex-direction:column"></div>' }));

  // What each side shows.
  const deskView = () => page.evaluate(() => {
    const view = document.querySelector(".terminal-lane:not([hidden]) .terminal-view:not(.concealed)");
    if (!view) return null;
    return { rows: view.querySelectorAll(".xterm-rows > div").length, text: view.querySelector(".xterm-rows")?.textContent || "" };
  });
  const phoneView = () => phone.evaluate(() => ({
    rows: document.querySelectorAll(".terminal-screen .xterm-rows > div").length,
    text: document.querySelector(".terminal-screen .xterm-rows")?.textContent || "",
  }));
  const openStreams = () => term.sockets.filter((entry) => entry.open && entry.id === ID).length;
  // Neither view blank, unsized or without its stream; both show the size
  // the terminal has (the program drew its last screen for it).
  const both = async (step, owner) => {
    const expected = owner === "desk" ? term.desk.join("x") : term.phone.join("x");
    await until(() => size() === expected, step + ": the terminal should have the " + owner + "'s size " + expected + ", has " + size());
    await until(async () => (await deskView())?.text.includes("SIZE " + expected), step + ": the computer's view does not show the screen at " + expected + ": " + JSON.stringify(await deskView()));
    await until(async () => (await phoneView()).text.includes("SIZE " + expected), step + ": the phone does not show the screen at " + expected + ": " + JSON.stringify((await phoneView()).text.slice(0, 120)));
    const typed = "TYPED " + term.typed.slice(-30);
    await until(async () => (await deskView())?.text.includes(typed) && (await phoneView()).text.includes(typed), step + ": a view misses the typing " + JSON.stringify(term.typed));
    const desk = await deskView(), mobile = await phoneView();
    assert.ok(desk.rows >= 2 && mobile.rows >= 2, step + ": a view has no rows");
    assert.equal(openStreams(), 1, step + ": the computer's view must have exactly one stream");
    assert.ok(Date.now() - term.phoneSeen < 4000 || term.held.length > 0, step + ": the phone stopped reading");
  };
  const deskType = async (text) => {
    await page.locator(".terminal-lane:not([hidden]) .xterm-helper-textarea").focus();
    await page.keyboard.type(text);
    await until(() => term.typed.endsWith(text), "the computer's typing did not arrive");
  };
  const phoneType = async (text) => {
    await phone.locator(".xterm-helper-textarea").focus();
    await phone.keyboard.type(text);
    await until(() => term.typed.endsWith(text), "the phone's typing did not arrive");
  };
  const tab = (name) => page.getByRole("tab", { name, exact: true }).first();

  state.terminals.push({ id: ID, title: "两端终端", workspace: state.settings.workspace }, { id: OTHER, title: "另一个终端", workspace: state.settings.workspace });
  await page.reload();
  await page.getByRole("button", { name: "启动 Super Cage 环境", exact: true }).click();
  await tab("两端终端").click();
  await until(() => openStreams() === 1 && term.desk, "the computer's view did not connect and size");

  // The computer shows the terminal and types: the size is the computer's.
  await deskType("a");
  await until(() => size() === term.desk.join("x"), "the computer's typing did not take the size");
  // The phone opens it: a phone resize applies at once.
  await openPhone();
  await until(() => term.phone, "the phone sent no size");
  await both("phone opened", "phone");
  await phoneType("b");
  await both("phone typed", "phone");
  await deskType("c");
  await both("computer typed", "desk");
  // The phone hidden and shown again (back from another app) sends its size.
  await phone.evaluate((id) => window.showTestTerminal({ phone: true, id, visible: false }), ID);
  await sleep(300);
  await phone.evaluate((id) => window.showTestTerminal({ phone: true, id, visible: true }), ID);
  await both("phone shown again", "phone");
  await deskType("d");
  await both("computer typed again", "desk");
  await sleep(5200);
  await phoneType("e");
  await both("phone typed after five seconds", "phone");

  // The computer switches to another tab and back: same stream, its size.
  await tab("另一个终端").click();
  await sleep(300);
  await tab("两端终端").click();
  await both("computer tab shown again", "desk");
  assert.equal(term.sockets.filter((entry) => entry.id === ID).length, 1, "a short switch must not reconnect");
  // Away for longer than the suspend delay: the stream is given up and the
  // view resumes after its offset when shown; the phone kept going meanwhile.
  await tab("另一个终端").click();
  for (let left = 125_000; left > 0; left -= 5_000) { await page.clock.fastForward(5_000); await sleep(30); }
  await until(() => openStreams() === 0, "a computer view hidden for two minutes kept its stream");
  await phoneType("f");
  await until(async () => (await phoneView()).text.includes("TYPED " + term.typed.slice(-30)), "the phone did not show its typing while the computer's view slept");
  await tab("两端终端").click();
  await until(() => openStreams() === 1, "the computer's view did not resume when shown");
  assert.notEqual(term.sockets.filter((entry) => entry.id === ID).at(-1).after, "0", "the resume continues after its offset");
  await both("computer view resumed", "desk");

  // Each page reloads. The kept history is trimmed by now: each replay comes
  // with the modes and a repaint that the other viewer sees too.
  assert.ok(term.start > 0, "the stand-in history should be trimmed");
  const repaints = term.repaints;
  const phoneBefore = term.phoneText.length;
  await page.reload();
  if (!(await deskView())) await tab("两端终端").click();
  await until(() => term.repaints > repaints, "a trimmed replay asked for no repaint");
  await until(() => term.phoneText.slice(phoneBefore).includes("SIZE " + term.desk[0] + "x" + (term.desk[1] - 1)), "the repaint did not reach the phone");
  await both("computer page reloaded", "desk");
  await openPhone();
  await both("phone page reloaded", "phone");

  // Both type in turn: the size follows the typing, and no view ends blank,
  // unsized or detached.
  for (const [i, who] of ["desk", "phone", "desk", "phone", "desk"].entries()) {
    if (who === "desk") await deskType(String(i));
    else await phoneType(String(i));
    await both("typing in turn " + i, who === "desk" ? "desk" : term.owner === "phone" ? "phone" : "desk");
  }
  // A failed read of the status or the terminal list (a gateway limiting
  // requests) keeps the view and its stream; only a list without the
  // terminal or an environment reported stopped shows the home page.
  await page.route("**/api/status", (route) => route.fulfill({ status: 429, contentType: "text/plain", body: "too many requests" }));
  await page.route("**/api/terminals", (route) => route.fulfill({ status: 429, contentType: "text/plain", body: "too many requests" }));
  state.status.terminals_revision = "limited";
  for (let i = 0; i < 3; i++) { await page.clock.fastForward(5_000); await sleep(200); }
  await page.unroute("**/api/status");
  await page.unroute("**/api/terminals");
  assert.ok(await deskView(), "a failed read removed the computer's view");
  assert.equal(openStreams(), 1, "a failed read closed the computer's stream");
  assert.ok([...term.deskSizes, ...term.phoneSizes].every(([cols, rows]) => cols >= 2 && rows >= 2), "a view sent an empty size: " + JSON.stringify([term.deskSizes, term.phoneSizes]));
  assert.deepEqual(errors, []);
  assert.deepEqual(phoneErrors, []);
  console.log("PASS: one terminal on the computer and the phone: sizes follow the typing and the phone, both views redraw at each size and see the repaint of a trimmed replay; tab switches, a two-minute suspend, the phone hidden and shown and reloads on both sides leave neither view blank, unsized or without its stream");
} finally {
  await browser.close();
}
