// Pasting into Herdr terminals (herdr-core: Super Cage views each one with
// hc attach). The screen gets what hc attach really prints (hc 0.1.22): its
// colour queries, then every mouse mode reset and focus reports on; the hc
// picker (kind herdr) draws on the alternate screen with no mouse mode.
// In every platform's keys: ⌘V or Ctrl+Shift+V, Ctrl+V in an agent terminal,
// Edit › Paste (the desktop menus, window.cageEditAction) and the browser's
// own paste (its context menu, or ⌘V without the desktop bridge) all deliver
// the text, bracketed and whole past 4 KiB; a right click is the page's own
// (its context menu) unless the program asked for the mouse. Needs a Vite
// dev server (CAGE_UI_URL).
import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

const HC_ATTACH = "\x1b]10;?\x1b\\\x1b]11;?\x1b\\\x1b]12;?\x1b\\\x18\x1b[!p\x1b[=0;1u\x1b[?1004h\x1b[?2026l" +
  "\x1b[?9l\x1b[?1000l\x1b[?1001l\x1b[?1002l\x1b[?1003l\x1b[?1004h\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l\x1b[?2004l";
const HC_PICKER = "\x1b[?1049h\x1b[?25l\x1b[H\x1b[2J  recent  1/1\r\n› + new  a shell here\r\n" +
  "  ○ t1   cage-0123456789abcdef  sh  proj  running\r\n ↑↓ move   enter choose   esc quit";
const AFTER_PICK = "\x1b[?25h\x1b[?1049l" + HC_ATTACH;
const big = "PASTE-BIG " + Array.from({ length: 80 }, (_, i) => String(i).padStart(3, "0") + " " + "x".repeat(76)).join("\n");
assert.ok(big.length > 4096);
const bracketed = (text) => "\x1b[200~" + text.replace(/\r?\n/g, "\r") + "\x1b[201~";
const isMouse = (data) => data.startsWith("\x1b[<") || data.startsWith("\x1b[M");

for (const platform of ["MacIntel", "Win32", "Linux x86_64"]) {
  const inputs = new Map();
  const sockets = new Map();
  let clipboard = { text: "" };
  const { page, browser, state, errors } = await createWorkbench({
    mockWebSockets: false,
    beforeNavigate: (page) => page.addInitScript((platform) => {
      Object.defineProperty(navigator, "platform", { value: platform });
    }, platform),
    nativeRequest: (request) => {
      if (request.action === "clipboardReadContent") return clipboard;
    },
  });
  const typed = (id) => inputs.get(id) || [];
  const pastes = (id) => typed(id).filter((data) => data.includes("PASTE-"));
  const mouse = (id) => typed(id).filter(isMouse);
  const wait = async (check, message) => {
    const deadline = Date.now() + 8000;
    while (!check() && Date.now() < deadline) await new Promise((resolve) => setTimeout(resolve, 20));
    assert.ok(check(), message);
  };
  const lane = () => page.locator(".terminal-lane:not([hidden])");
  const focus = () => lane().locator(".xterm-helper-textarea").focus();
  const primaryPaste = () => page.keyboard.press(platform === "MacIntel" ? "Meta+v" : "Control+Shift+V");
  // A right click on the screen; true when the page's own menu would open.
  const rightClick = async () => {
    const box = await lane().locator(".xterm-screen").boundingBox();
    await page.evaluate(() => {
      window.__contextMenus = [];
      document.addEventListener("contextmenu", (event) => setTimeout(() => window.__contextMenus.push(!event.defaultPrevented)), { capture: true, once: true });
    });
    await page.mouse.click(box.x + box.width / 2, box.y + 12, { button: "right" });
    await page.waitForFunction(() => window.__contextMenus.length === 1);
    return page.evaluate(() => window.__contextMenus[0]);
  };
  // The browser's paste (its context menu's 粘贴): a paste event where the
  // terminal takes text.
  const browserPaste = (text) => page.evaluate((text) => {
    const data = new DataTransfer();
    data.setData("text/plain", text);
    document.activeElement.dispatchEvent(new ClipboardEvent("paste", { clipboardData: data, bubbles: true, cancelable: true }));
  }, text);
  const open = async (item, id) => {
    const first = page.getByRole("button", { name: "新建终端", exact: true });
    await (await first.count() ? first.first() : page.getByRole("button", { name: /^在 .* 新建终端$/ }).first()).click();
    await page.getByRole("menuitem", { name: item }).first().click();
    await wait(() => sockets.has(id), item + " never connected");
  };
  try {
    await page.route("**/api/herdr**", (route) => route.fulfill({ json: {
      enabled: true, installed: true, supports_managed: true, server_running: true, version: "0.1.22",
      takeover_terminals: true, terminals_count: 2, vm_running: true, ready: true,
    } }));
    await page.route("**/api/input/paste/start", (route) => route.fulfill({ json: { available: true } }));
    await page.route("**/api/terminals", (route) => {
      if (route.request().method() !== "POST") return route.fallback();
      const { kind } = route.request().postDataJSON();
      const id = { shell: "herdr-shell", claude: "herdr-claude", herdr: "herdr-picker" }[kind];
      const terminal = { id, kind, title: kind === "herdr" ? "Herdr" : kind === "claude" ? "Claude 1" : "终端 1",
        workspace: state.settings.workspace, herdr_backed: true };
      state.terminals = [...state.terminals.filter((item) => item.id !== id), terminal];
      return route.fulfill({ json: terminal });
    });
    await page.routeWebSocket("**/api/**", (socket) => {
      const id = socket.url().match(/\/terminals\/([^/]+)\/stream/)?.[1];
      if (!id) return socket.close();
      sockets.set(id, socket);
      let picked = false;
      socket.onMessage((message) => {
        const value = JSON.parse(String(message));
        if (value.type !== "input") return;
        inputs.set(id, [...typed(id), value.data]);
        if (id === "herdr-picker" && !picked && value.data === "\r") {
          picked = true;
          socket.send(Buffer.from(AFTER_PICK + "\x1b[?2004hpicked$ "));
        }
      });
      socket.send(Buffer.from(id === "herdr-picker" ? HC_PICKER : HC_ATTACH + "\x1b[?2004h" + id + "$ "));
    });
    await page.reload();
    await page.getByRole("button", { name: "启动 Super Cage 环境", exact: true }).click();

    // A Herdr shell: the primary paste key, a paste past 4 KiB, Edit › Paste.
    await open(/^普通终端$/, "herdr-shell");
    await page.waitForFunction(() => document.querySelector(".terminal-lane:not([hidden]) .xterm-rows")?.textContent.includes("herdr-shell$"));
    await focus();
    clipboard = { text: "PASTE-1 héllo 中文 $HOME \u0060x\u0060" };
    await primaryPaste();
    await wait(() => pastes("herdr-shell").length === 1, "the paste key delivered nothing");
    assert.equal(pastes("herdr-shell")[0], bracketed(clipboard.text));
    clipboard = { text: big };
    await primaryPaste();
    await wait(() => pastes("herdr-shell").length === 2, "a paste past 4 KiB delivered nothing");
    assert.equal(pastes("herdr-shell")[1], bracketed(big), "a paste past 4 KiB changed on the way");
    clipboard = { text: "PASTE-EDIT menu" };
    assert.equal(await page.evaluate(() => window.cageEditAction("paste")), true);
    await wait(() => pastes("herdr-shell").length === 3, "Edit › Paste delivered nothing");
    assert.equal(pastes("herdr-shell")[2], bracketed("PASTE-EDIT menu"));

    // No mouse mode (hc attach turned them all off): the right click is the
    // page's, so its menu's 粘贴 pastes.
    assert.equal(await page.locator(".terminal-lane:not([hidden]) .xterm.enable-mouse-events").count(), 0, "hc attach left mouse capture on");
    assert.equal(await rightClick(), true, "the terminal took a right click no program asked for");
    assert.deepEqual(mouse("herdr-shell"), [], "a right click reached the program");
    await browserPaste("PASTE-CONTEXT 粘贴");
    await wait(() => pastes("herdr-shell").length === 4, "the browser's paste delivered nothing");
    assert.equal(pastes("herdr-shell")[3], bracketed("PASTE-CONTEXT 粘贴"));

    // A program that asks for the mouse gets the right click, and gives it
    // back when it is done.
    sockets.get("herdr-shell").send(Buffer.from("\x1b[?1000h\x1b[?1006h"));
    await page.locator(".terminal-lane:not([hidden]) .xterm.enable-mouse-events").waitFor();
    await rightClick();
    await wait(() => mouse("herdr-shell").some((data) => data.startsWith("\x1b[<2;")), "the program asked for the mouse and got no right click");
    sockets.get("herdr-shell").send(Buffer.from("\x1b[?1000l\x1b[?1006l"));
    await page.locator(".terminal-lane:not([hidden]) .xterm.enable-mouse-events").waitFor({ state: "detached" });
    const taken = mouse("herdr-shell").length;
    assert.equal(await rightClick(), true, "the right click stayed the program's after it let go");
    assert.equal(mouse("herdr-shell").length, taken);

    // An agent terminal in Herdr: Ctrl+V reads the computer's clipboard.
    await open(/^Claude 终端$/, "herdr-claude");
    await page.waitForFunction(() => document.querySelector(".terminal-lane:not([hidden]) .xterm-rows")?.textContent.includes("herdr-claude$"));
    await focus();
    clipboard = { text: "PASTE-CTRLV agent" };
    await page.keyboard.press("Control+v");
    await wait(() => pastes("herdr-claude").length === 1, "Ctrl+V in an agent terminal delivered nothing");
    assert.equal(pastes("herdr-claude")[0], bracketed("PASTE-CTRLV agent"));
    clipboard = { text: big };
    await primaryPaste();
    await wait(() => pastes("herdr-claude").length === 2, "the paste key in an agent terminal delivered nothing");
    assert.equal(pastes("herdr-claude")[1], bracketed(big));

    // The picker (Herdr 终端): no mouse mode while it shows, none left once
    // it attaches; then pastes go to the chosen terminal.
    await open(/Herdr 终端/, "herdr-picker");
    await page.waitForFunction(() => document.querySelector(".terminal-lane:not([hidden]) .xterm-rows")?.textContent.includes("enter choose"));
    await focus();
    assert.equal(await page.locator(".terminal-lane:not([hidden]) .xterm.enable-mouse-events").count(), 0, "the picker took the mouse");
    await page.keyboard.press("Enter");
    await page.waitForFunction(() => document.querySelector(".terminal-lane:not([hidden]) .xterm-rows")?.textContent.includes("picked$"));
    assert.equal(await page.locator(".terminal-lane:not([hidden]) .xterm.enable-mouse-events").count(), 0, "mouse capture stayed on after the picker attached");
    assert.equal(await rightClick(), true, "the terminal took a right click after the picker attached");
    assert.deepEqual(mouse("herdr-picker"), []);
    clipboard = { text: "PASTE-PICKED after attach" };
    await primaryPaste();
    await wait(() => pastes("herdr-picker").length === 1, "pasting after the picker attached delivered nothing");
    assert.equal(pastes("herdr-picker")[0], bracketed("PASTE-PICKED after attach"));
    assert.deepEqual(errors, []);
    console.log(platform + ": Herdr terminals paste by key, past 4 KiB, Edit › Paste and the page's menu; right clicks stay the page's without a mouse mode; the picker leaves none on");
  } catch (error) {
    console.error({ platform, inputs: Object.fromEntries([...inputs].map(([id, list]) => [id, list.map((data) => JSON.stringify(data).slice(0, 120))])), errors });
    throw error;
  } finally { await browser.close(); }
}
