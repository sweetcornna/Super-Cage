import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

// A terminal hidden for two minutes gives its stream up and, when shown,
// resumes after the last byte it received: output produced meanwhile appears
// once, with no gap and no repeat. The visible terminal keeps its stream
// however long it stays shown; a session that ended while its view was
// suspended still shows its last output and the end; a terminal restored
// hidden after a reload opens no stream until it is shown. The terminal server is a stand-in
// that keeps each session's output and replays it after the requested offset.
const fixture = await createWorkbench({ mockWebSockets: false, beforeNavigate: page => page.clock.install() });
const { browser, page, errors, state } = fixture;
const sockets = [], kept = new Map(), ended = new Set();
const until = async (condition, message) => {
  for (let i = 0; i < 400; i++) {
    if (await condition()) return;
    await new Promise(resolve => setTimeout(resolve, 25));
  }
  throw new Error(message);
};
const of = id => sockets.filter(entry => entry.id === id);
const open = id => of(id).filter(entry => !entry.closed);
// Server output: kept for later viewers and sent to the attached ones.
const emit = (id, text) => {
  const data = Buffer.from(text);
  kept.set(id, Buffer.concat([kept.get(id) || Buffer.alloc(0), data]));
  for (const entry of open(id)) entry.socket.send(data);
};
const screen = () => page.evaluate(() => document.querySelector(".terminal-lane:not([hidden]) .xterm-rows")?.textContent || "");
const times = (text, part) => text.split(part).length - 1;
const perf = id => page.evaluate(id => window.__cagePerf?.summary().terminals.find(entry => entry.id === id), id);
// Small steps, so the stand-in answers each keepalive before the next check.
const advance = async ms => {
  for (let left = ms; left > 0; left -= 5_000) {
    await page.clock.fastForward(Math.min(5_000, left));
    await new Promise(resolve => setTimeout(resolve, 30));
  }
};
const show = name => page.getByRole("tab", { name, exact: true }).first().click();
const newTerminal = async () => {
  const before = sockets.length;
  // The home page's button first, then the project row's in the sidebar.
  await page.getByRole("button", { name: /新建终端$/ }).first().click();
  await page.getByRole("menuitem", { name: "普通终端", exact: true }).click();
  await until(() => sockets.length > before, "new terminal did not connect");
  return sockets.at(-1).id;
};
try {
  await page.routeWebSocket("**/api/**", socket => {
    if (!socket.url().includes("/terminals/")) return socket.close();
    const url = new URL(socket.url());
    const entry = { socket, id: url.pathname.split("/").at(-2), after: url.searchParams.get("after"), messages: [], closed: false };
    socket.onMessage(message => {
      const parsed = JSON.parse(message);
      entry.messages.push(parsed);
      if (parsed.type === "ping") socket.send(JSON.stringify({ type: "pong" }));
    });
    socket.onClose(() => { entry.closed = true; });
    sockets.push(entry);
    const history = kept.get(entry.id) || Buffer.alloc(0);
    const after = Number(entry.after);
    const tail = history.subarray(Math.min(after, history.length));
    socket.send(JSON.stringify({ type: "history", offset: after, reset: after > history.length, size: tail.length }));
    if (tail.length) socket.send(tail);
    if (ended.has(entry.id)) {
      socket.send(JSON.stringify({ type: "exit" }));
      entry.closed = true;
      socket.close();
    }
  });
  await page.reload();
  await page.getByRole("button", { name: "启动 Super Cage 环境", exact: true }).click();
  const first = await newTerminal();
  emit(first, "LINE-A\r\n$ ");
  await until(async () => (await screen()).includes("LINE-A"), "first terminal output was not shown");
  const second = await newTerminal();
  emit(second, "TWO-A\r\n$ ");
  await until(async () => (await screen()).includes("TWO-A"), "second terminal output was not shown");

  // Hidden for one minute: still attached. Past two minutes: suspended.
  await advance(60_000);
  assert.equal(open(first).length, 1, "a terminal hidden for one minute keeps its stream");
  await advance(65_000);
  await until(() => open(first).length === 0, "a terminal hidden for two minutes kept its stream");
  assert.equal(of(first).length, 1, "a suspended terminal must not reconnect");
  assert.equal((await perf(first)).suspended, 1);
  emit(first, "LINE-B\r\n$ ");

  // Wake-ups and network changes leave a suspended terminal alone, and the
  // visible terminal is never suspended (five minutes shown in all).
  await page.evaluate(() => {
    window.dispatchEvent(new Event("online"));
    document.dispatchEvent(new Event("visibilitychange"));
  });
  await advance(180_000);
  assert.equal(of(first).length, 1, "a suspended terminal reconnected while hidden");
  assert.equal(of(second).length, 1, "the visible terminal reconnected");
  assert.equal(open(second).length, 1, "the visible terminal lost its stream");
  assert.equal((await perf(second)).suspended, 0, "the visible terminal was suspended");

  // Shown again: one stream after the received bytes; the output produced
  // while suspended appears once and nothing repeats.
  await page.evaluate(() => {
    window.terminalNotes = [];
    new MutationObserver(() => {
      const note = document.querySelector(".terminal-lane:not([hidden]) .terminal-note");
      if (note) window.terminalNotes.push(note.textContent);
    }).observe(document.body, { childList: true, subtree: true, characterData: true });
  });
  await show("终端 1");
  await until(() => of(first).length === 2, "a shown terminal did not resume");
  assert.equal(of(first)[1].after, String(Buffer.byteLength("LINE-A\r\n$ ")), "resume must start after the received bytes");
  await until(async () => (await screen()).includes("LINE-B"), "output produced while suspended was not shown");
  emit(first, "LINE-C\r\n$ ");
  await until(async () => (await screen()).includes("LINE-C"), "live output after the resume was not shown");
  const resumed = await screen();
  for (const line of ["LINE-A", "LINE-B", "LINE-C"]) assert.equal(times(resumed, line), 1, line + " must appear once");
  assert.deepEqual(await page.evaluate(() => window.terminalNotes), [], "a quick resume must not flash a notice");
  assert.equal((await perf(first)).resumed, 1);

  // A session that ends while its view is suspended still shows its last
  // output and the end when shown, even after the list marks it closed.
  await show("终端 2");
  await advance(125_000);
  await until(() => open(first).length === 0, "the first terminal was not suspended again");
  emit(first, "BYE\r\n");
  ended.add(first);
  state.terminals.find(item => item.id === first).closed = true;
  state.status.terminals_revision = "closed-" + first;
  await advance(10_000);
  await show("终端 1");
  await until(() => of(first).length === 3, "a suspended terminal that ended did not fetch its last output");
  await until(async () => (await screen()).includes("BYE"), "the last output was not shown");
  await page.getByText("终端会话已结束。可点击项目右侧的终端图标新建终端。", { exact: true }).waitFor();
  assert.equal(times(await screen(), "BYE"), 1);
  assert.equal(times(await screen(), "LINE-C"), 1);
  await advance(30_000);
  assert.equal(of(first).length, 3, "an ended session must not reconnect");

  // After a reload the second terminal is restored hidden. Its xterm cannot
  // take its size while hidden, so it opens its stream only when shown and
  // draws the kept history at its own size.
  const before = of(second).length;
  await page.reload();
  await page.getByRole("tab", { name: "终端 2", exact: true }).first().waitFor();
  await new Promise(resolve => setTimeout(resolve, 500));
  await advance(125_000);
  assert.equal(of(second).length, before, "a terminal restored hidden must not connect before it is shown");
  emit(second, "TWO-B\r\n$ ");
  await show("终端 2");
  await until(() => of(second).length === before + 1, "the restored terminal did not connect when shown");
  assert.equal(of(second).at(-1).after, "0");
  await until(async () => (await screen()).includes("TWO-B"), "the restored terminal did not show its output");
  assert.equal(times(await screen(), "TWO-A"), 1);
  assert.equal(times(await screen(), "TWO-B"), 1);
  assert.deepEqual(errors, []);
  console.log("PASS: hidden terminals suspend after two minutes and resume after their offset without gaps or repeats; the visible terminal stays attached; an ended session shows its tail; a terminal restored hidden connects when shown");
} finally {
  await browser.close();
}
