import assert from "node:assert/strict";
import { createRequire } from "node:module";
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.CAGE_PLAYWRIGHT_MODULE || "playwright");

// Touch and wheel scrolling in a terminal, and what a page that opens a
// terminal late is shown. Every stream is synthetic, made up here.
//
// Phone (390x844, touch): a finger dragged over the terminal moves the
// scrollback in the normal screen, sends arrow keys in a full-screen program
// without the mouse (less), and wheel reports to one that reads the mouse
// (Codex); a tap still focuses. Its first read answers only the queries the
// server did not mark answered, a reset read applies the modes in its state,
// and the phone sends its size again each time it is shown and when its
// user types.
// Desktop (1440x900): the wheel does the same three things; a finger drag
// works there too; a reset replay applies the state of its history marker
// and answers only the unanswered queries; a view mounted hidden opens its
// stream only when it is shown, at its own size.
const browser = await chromium.launch({ headless: true, executablePath: process.env.CAGE_TEST_BROWSER_PATH });
const origin = process.env.CAGE_UI_URL || "http://127.0.0.1:5173";
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const until = async (condition, message) => {
  for (let i = 0; i < 200; i++) {
    if (await condition()) return;
    await sleep(25);
  }
  throw new Error(message);
};
const lines = (from, to, prefix = "LINE-") => Array.from({ length: to - from + 1 }, (_, i) => prefix + String(from + i).padStart(3, "0")).join("\r\n");
const DA1 = "\x1b[c", DA1_ANSWER = "\x1b[?1;2c", DSR = "\x1b[5n", DSR_ANSWER = "\x1b[0n";

async function open(page, html) {
  await page.route("**/api/**", (route) => route.fulfill({ json: { ok: true } }));
  await page.route("**/api/appearance", (route) => route.fulfill({ json: {} }));
  await page.route("**/touch-test-page", (route) => route.fulfill({ contentType: "text/html", body: html }));
  await page.goto(origin + "/touch-test-page");
  await page.evaluate(async () => {
    const { default: RefreshRuntime } = await import("/@react-refresh");
    RefreshRuntime.injectIntoGlobalHook(window);
    window.$RefreshReg$ = () => {};
    window.$RefreshSig$ = () => (type) => type;
    window.__vite_plugin_react_preamble_installed__ = true;
    await import("/scripts/terminal-touch-fixture.jsx");
  });
}

// A finger on the screen, through the browser's own touch input.
async function drag(page, cdp, from, to, steps = 10) {
  const box = await page.locator(".xterm-screen").boundingBox();
  const x = box.x + box.width / 2, y0 = box.y + box.height * from, y1 = box.y + box.height * to;
  await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x, y: y0 }] });
  for (let i = 1; i <= steps; i++) {
    await cdp.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x, y: y0 + ((y1 - y0) * i) / steps }] });
    await sleep(16);
  }
  await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
}
const screenText = (page) => page.evaluate(() => document.querySelector(".xterm-rows")?.textContent || "");
const shownLines = async (page) => (await screenText(page)).match(/LINE-\d{3}/g)?.map((line) => Number(line.slice(5))) || [];
// Waits for a moving screen (a flick) to settle.
const settled = async (page) => {
  let last = await screenText(page);
  for (let i = 0; i < 40; i++) {
    await sleep(150);
    const next = await screenText(page);
    if (next === last) return next;
    last = next;
  }
  throw new Error("the terminal kept scrolling");
};

async function phone() {
  const context = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, colorScheme: "light" });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const inputs = [], resizes = [], reads = [], held = [];
  await open(page, '<meta name="viewport" content="width=device-width, initial-scale=1"><div id="terminal" style="height:100vh;display:flex;flex-direction:column"></div>');
  await page.route("**/api/mobile/terminals/touch-test/output**", (route) => {
    reads.push(Number(new URL(route.request().url()).searchParams.get("after")));
    held.push(route);
  });
  await page.route("**/api/mobile/terminals/touch-test/input", (route) => { inputs.push(route.request().postDataJSON().data); return route.fulfill({ status: 204 }); });
  await page.route("**/api/mobile/terminals/touch-test/resize", (route) => { resizes.push(route.request().postDataJSON()); return route.fulfill({ status: 204 }); });
  let next = 0;
  // Answers the read the phone holds open.
  const serve = async (reply) => {
    await until(() => held.length > 0, "the phone is not reading");
    const route = held.shift();
    const data = Buffer.from(reply.text || "");
    next = reply.reset ? reply.from + data.length : next + data.length;
    await route.fulfill({ json: { data: data.toString("base64"), next, reset: !!reply.reset, closed: false, exit_code: null, answered: reply.answered ?? 0, ...(reply.state ? { state: reply.state } : {}) } });
  };
  const typed = () => inputs.join("");
  const cdp = await context.newCDPSession(page);
  try {
    await page.evaluate(() => window.showTestTerminal({ phone: true }));
    await page.locator(".xterm-screen").waitFor();
    await until(() => resizes.length === 1, "the phone did not send its size");
    assert.ok(resizes[0].cols < 80 && resizes[0].rows > 24, "the phone's own size, not 80x24: " + JSON.stringify(resizes[0]));

    // The first read replays history. The query in the part a viewer was
    // shown before is not answered again; the one after it is.
    const answeredPart = DA1 + lines(1, 100) + "\r\n";
    await serve({ text: answeredPart + lines(101, 200) + "\r\n" + DSR + "$ ", answered: Buffer.byteLength(answeredPart) });
    await until(async () => (await screenText(page)).includes("LINE-200"), "the replay was not shown");
    await until(() => typed().includes(DSR_ANSWER), "the unanswered query was not answered");
    await sleep(300);
    assert.ok(!typed().includes(DA1_ANSWER), "a query answered before must not be answered again: " + JSON.stringify(typed()));

    // (a) Normal screen: the finger pulls older lines down into view, and
    // pushes them back; scrolling types nothing.
    const before = inputs.length;
    const bottom = Math.min(...(await shownLines(page)));
    await drag(page, cdp, 0.2, 0.8);
    await settled(page);
    const up = Math.min(...(await shownLines(page)));
    assert.ok(up < bottom - 5, "dragging down shows older lines: " + bottom + " -> " + up);
    await drag(page, cdp, 0.9, 0.1);
    await settled(page);
    assert.ok(Math.min(...(await shownLines(page))) > up, "dragging up shows newer lines");
    assert.equal(inputs.length, before, "scrolling the scrollback sends nothing");
    // A tap still focuses the terminal (and opens the keyboard).
    await page.evaluate(() => document.activeElement?.blur());
    const box = await page.locator(".xterm-screen").boundingBox();
    await page.touchscreen.tap(box.x + box.width / 2, box.y + box.height / 2);
    await until(() => page.evaluate(() => document.activeElement?.classList.contains("xterm-helper-textarea")), "a tap did not focus the terminal");

    // (b) Alternate screen without the mouse (less): one arrow key a line,
    // as xterm sends for a wheel there; application cursor keys respected.
    await serve({ text: "\x1b[?1049h\x1b[H\x1b[2JLESS-SCREEN" });
    await until(async () => (await screenText(page)).includes("LESS-SCREEN"), "the full-screen program was not shown");
    inputs.length = 0;
    await drag(page, cdp, 0.3, 0.7);
    await until(() => typed().includes("\x1b[A"), "dragging down in less sent no arrow up");
    await sleep(300);
    assert.match(typed(), /^(\x1b\[A)+$/, "only arrow keys: " + JSON.stringify(typed()));
    const arrows = typed().length / 3;
    assert.ok(arrows >= 5, "one arrow a line, got " + arrows);
    await serve({ text: "\x1b[?1h" });
    await sleep(200);
    inputs.length = 0;
    await drag(page, cdp, 0.7, 0.3);
    await until(() => typed().includes("\x1bOB"), "dragging up sent no application arrow down");
    await sleep(300);
    assert.match(typed(), /^(\x1bOB)+$/);

    // (c) A reset read: the screen clears, the state sets the alternate
    // screen and SGR mouse the kept output was written in, and a drag turns
    // the wheel for the program that reads the mouse (Codex).
    await serve({ reset: true, from: 9_000_000, state: "\x1b[?1049h\x1b[?1002h\x1b[?1006h", text: "\x1b[HMOUSE-SCREEN", answered: 0 });
    await until(async () => (await screenText(page)).includes("MOUSE-SCREEN"), "the reset read was not shown");
    assert.ok(!(await screenText(page)).includes("LESS-SCREEN"), "a reset clears the screen");
    inputs.length = 0;
    await drag(page, cdp, 0.3, 0.7);
    await until(() => /\x1b\[<64;\d+;\d+M/.test(typed()), "dragging down sent no wheel-up report");
    await sleep(300);
    assert.match(typed(), /^(\x1b\[<64;\d+;\d+M)+$/, "wheel reports only: " + JSON.stringify(typed()));
    inputs.length = 0;
    await drag(page, cdp, 0.7, 0.3);
    await until(() => /\x1b\[<65;\d+;\d+M/.test(typed()), "dragging up sent no wheel-down report");

    // Shown again (back from another app): the phone sends its size even
    // though it did not change, as a computer may have set another meanwhile.
    const sized = resizes.length;
    await page.evaluate(() => window.showTestTerminal({ phone: true, visible: false }));
    await sleep(100);
    await page.evaluate(() => window.showTestTerminal({ phone: true, visible: true }));
    await until(() => resizes.length > sized, "the phone did not send its size when shown again");
    assert.deepEqual(resizes.at(-1), resizes[0]);
    // Typing on the phone takes the size back, as the computer does when
    // its user types there; at most once every five seconds.
    await sleep(5200);
    const typedSized = resizes.length;
    await page.locator(".xterm-helper-textarea").focus();
    await page.keyboard.type("x");
    await until(() => resizes.length > typedSized, "typing on the phone did not send its size");
    await page.keyboard.type("y");
    await sleep(400);
    assert.equal(resizes.length, typedSized + 1, "at most one size every five seconds");
    assert.deepEqual(errors, []);
  } finally {
    await context.close();
  }
}

async function desktop() {
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, hasTouch: true });
  await context.addInitScript(() => localStorage.setItem("cage-terminal-renderer", "dom"));
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const sockets = [];
  // Before the page loads: the stand-in replaces the page's WebSocket.
  await page.routeWebSocket("**/api/terminals/**", (socket) => {
    const url = new URL(socket.url());
    const entry = { socket, id: url.pathname.split("/").at(-2), after: url.searchParams.get("after"), messages: [] };
    socket.onMessage((message) => entry.messages.push(JSON.parse(message)));
    sockets.push(entry);
  });
  await open(page, '<div id="terminal" style="height:860px;display:flex;flex-direction:column"></div>');
  const typed = (entry) => entry.messages.filter((m) => m.type === "input").map((m) => m.data).join("");
  const history = (entry, marker, text) => {
    const data = Buffer.from(text);
    entry.socket.send(JSON.stringify({ type: "history", size: data.length, ...marker }));
    entry.socket.send(data);
  };
  const wheel = async (deltaY, times = 3) => {
    const box = await page.locator(".xterm-screen").boundingBox();
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    for (let i = 0; i < times; i++) { await page.mouse.wheel(0, deltaY); await sleep(40); }
  };
  const cdp = await context.newCDPSession(page);
  try {
    await page.evaluate(() => window.showTestTerminal());
    await until(() => sockets.length === 1, "the terminal did not connect");
    const first = sockets[0];
    const answeredPart = DA1 + lines(1, 100) + "\r\n";
    history(first, { offset: 0, reset: false, answered: Buffer.byteLength(answeredPart) }, answeredPart + lines(101, 200) + "\r\n" + DSR + "$ ");
    await until(async () => (await screenText(page)).includes("LINE-200"), "the replay was not shown");
    await until(() => typed(first).includes(DSR_ANSWER), "the unanswered query was not answered");
    await sleep(300);
    assert.ok(!typed(first).includes(DA1_ANSWER), "a query answered before must not be answered again");

    // (a) Normal screen: the wheel and a finger move the scrollback.
    const bottom = Math.min(...(await shownLines(page)));
    await wheel(-300);
    await settled(page);
    const up = Math.min(...(await shownLines(page)));
    assert.ok(up < bottom, "the wheel shows older lines: " + bottom + " -> " + up);
    await wheel(2000);
    await settled(page);
    await drag(page, cdp, 0.2, 0.8);
    await settled(page);
    assert.ok(Math.min(...(await shownLines(page))) < bottom, "a finger drag shows older lines on the desktop too");

    // (b) Alternate screen without the mouse: the wheel sends arrow keys.
    first.socket.send(Buffer.from("\x1b[?1049h\x1b[H\x1b[2JLESS-SCREEN"));
    await until(async () => (await screenText(page)).includes("LESS-SCREEN"), "the full-screen program was not shown");
    first.messages.length = 0;
    await wheel(-100);
    await until(() => typed(first).includes("\x1b[A"), "the wheel sent no arrow up in less");

    // (c) A reconnect whose offset is gone: the replay comes with the state
    // the kept output was written in; the wheel then reaches the program as
    // SGR mouse reports.
    first.socket.close();
    await until(() => sockets.length === 2, "the terminal did not reconnect");
    const second = sockets[1];
    history(second, { offset: 9_000_000, reset: true, answered: 0, state: "\x1b[?1049h\x1b[?1002h\x1b[?1006h" }, "\x1b[HMOUSE-SCREEN" + DA1);
    await until(async () => (await screenText(page)).includes("MOUSE-SCREEN"), "the reset replay was not shown");
    assert.ok(!(await screenText(page)).includes("LESS-SCREEN"), "a reset clears the screen");
    await until(() => typed(second).includes(DA1_ANSWER), "a query nobody was shown before must be answered");
    second.messages.length = 0;
    await wheel(-100);
    await until(() => /\x1b\[<64;\d+;\d+M/.test(typed(second)), "the wheel sent no SGR report");

    // A view mounted hidden opens no stream until shown, then asks for the
    // whole kept history and takes its own size, not xterm's 80x24.
    await page.evaluate(() => window.showTestTerminal({ id: "hidden-test", visible: false }));
    await sleep(600);
    assert.equal(sockets.filter((entry) => entry.id === "hidden-test").length, 0, "a hidden view must not connect");
    await page.evaluate(() => window.showTestTerminal({ id: "hidden-test", visible: true }));
    await until(() => sockets.some((entry) => entry.id === "hidden-test"), "a shown view did not connect");
    const shown = sockets.find((entry) => entry.id === "hidden-test");
    assert.equal(shown.after, "0");
    await until(() => shown.messages.some((m) => m.type === "resize"), "the shown view sent no size");
    const size = shown.messages.find((m) => m.type === "resize");
    assert.ok(size.cols > 120, "fitted to the window before the replay: " + JSON.stringify(size));
    assert.deepEqual(errors, []);
  } finally {
    await context.close();
  }
}

try {
  await phone();
  await desktop();
  console.log("PASS: a finger drag scrolls the phone terminal in the normal screen, sends arrows in a full-screen program and wheel reports to one that reads the mouse; the desktop wheel does the same; replays apply their modes and leave answered queries unanswered; the phone re-sends its size when shown and when its user types; a hidden desktop view connects when shown");
} finally {
  await browser.close();
}
