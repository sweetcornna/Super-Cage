import assert from "node:assert/strict";
import test from "node:test";
import {
  browserEngine, codexLaunchRefused, createTerminalRenderer, readTerminalRendererChoice, terminalLastLine, terminalRendererKey, wantsWebgl, webglLetterSpacing,
} from "./terminal-renderer.mjs";

const storage = (value) => ({ getItem: (key) => key === terminalRendererKey ? value : null });

test("the setting defaults to auto and ignores unknown values", () => {
  assert.equal(readTerminalRendererChoice(storage(null)), "auto");
  assert.equal(readTerminalRendererChoice(storage("dom")), "dom");
  assert.equal(readTerminalRendererChoice(storage("webgl")), "webgl");
  assert.equal(readTerminalRendererChoice(storage("canvas")), "auto");
  assert.equal(readTerminalRendererChoice({ getItem() { throw new Error("denied"); } }), "auto");
});

test("Blink engines are told apart from WebKit web views", () => {
  assert.equal(browserEngine({ userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0" }), "chromium");
  assert.equal(browserEngine({ userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko)" }), "webkit");
  assert.equal(browserEngine({ userAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15" }), "webkit");
  assert.equal(browserEngine({ userAgent: "x", userAgentData: { brands: [{ brand: "Chromium" }] } }), "chromium");
  assert.equal(browserEngine({ userAgent: "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0" }), "gecko");
});

test("auto uses WebGL only on Blink with WebGL2; the setting overrides", () => {
  assert.equal(wantsWebgl({ choice: "auto", engine: "chromium", webgl2: true }), true);
  assert.equal(wantsWebgl({ choice: "auto", engine: "webkit", webgl2: true }), false);
  assert.equal(wantsWebgl({ choice: "webgl", engine: "webkit", webgl2: true }), true);
  assert.equal(wantsWebgl({ choice: "webgl", engine: "chromium", webgl2: false }), false);
  assert.equal(wantsWebgl({ choice: "dom", engine: "chromium", webgl2: true }), false);
});

function fakeTerminal() {
  return { loaded: [], options: {}, loadAddon(addon) { this.loaded.push(addon); addon.activate?.(this); } };
}
function fakeAddon(log = []) {
  return class {
    constructor() { this.disposed = false; log.push(this); }
    onContextLoss(fn) { this.lose = fn; }
    dispose() { this.disposed = true; }
  };
}
const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

test("a shown terminal gets WebGL; a lost context returns it to the DOM renderer for good", async () => {
  const made = [];
  const changes = [];
  const renderer = createTerminalRenderer(fakeTerminal(), { load: async () => fakeAddon(made), enabled: () => true, live: new Set(), onChange: (kind) => changes.push(kind) });
  renderer.show();
  await settle();
  assert.equal(renderer.kind, "webgl");
  made[0].lose();
  assert.equal(renderer.kind, "dom");
  assert.equal(made[0].disposed, true);
  renderer.show();
  await settle();
  assert.equal(made.length, 1, "no second attempt after a context loss");
  assert.deepEqual(changes, ["webgl", "dom"]);
});

test("WebGL cells stay within half a device pixel of the DOM renderer's", () => {
  // The WebGL renderer uses floor(advance × ratio) device pixels plus the spacing.
  for (const ratio of [1, 1.25, 1.5, 2]) {
    for (const advance of [6.6, 7.2, 7.5, 7.8, 7.875, 8.4, 9.01]) {
      const device = advance * ratio;
      const cell = Math.floor(device) + webglLetterSpacing(advance, ratio);
      assert.ok(Math.abs(cell - device) <= 0.5 + 1e-9, `${advance} px at ${ratio}x: ${cell} device px for ${device}`);
    }
  }
  assert.equal(webglLetterSpacing(7.875, 1), 1);
  assert.equal(webglLetterSpacing(7.2, 1), 0);
  assert.equal(webglLetterSpacing(8, 2), 0);
  assert.equal(webglLetterSpacing(undefined, 2), 0);
  assert.equal(webglLetterSpacing(7.8, NaN), 0);
});

test("letter spacing follows the renderer, the font and the pixel ratio", async () => {
  const made = [];
  const terminal = fakeTerminal();
  let advance = 7.875, ratio = 1, ratioChanged = null, unwatched = 0;
  const renderer = createTerminalRenderer(terminal, {
    load: async () => fakeAddon(made), enabled: () => true, live: new Set(),
    measure: () => advance, pixelRatio: () => ratio,
    watchRatio: (callback) => { ratioChanged = callback; return () => { unwatched++; }; },
  });
  assert.equal(terminal.options.letterSpacing, undefined, "the DOM renderer keeps xterm's default");
  renderer.show();
  await settle();
  assert.equal(terminal.options.letterSpacing, 1, "7.875 px at 1x: an 8 px cell instead of 7");
  advance = 7.2;
  renderer.align();
  assert.equal(terminal.options.letterSpacing, 0, "a font change measures again");
  ratio = 1.5;
  ratioChanged();
  assert.equal(terminal.options.letterSpacing, 1, "7.2 px at 1.5x: an 11 px cell instead of 10");
  made[0].lose();
  assert.equal(terminal.options.letterSpacing, 0, "the DOM renderer gets no spacing back");
  assert.equal(unwatched, 1, "the ratio watch ends with WebGL");
});

test("a renderer that cannot start stays on the DOM renderer", async () => {
  const renderer = createTerminalRenderer(fakeTerminal(), { load: async () => { throw new Error("no webgl2"); }, enabled: () => true, live: new Set() });
  renderer.show();
  await settle();
  assert.equal(renderer.kind, "dom");
  assert.equal(renderer.failed, true);
});

test("only the most recently shown terminals keep a context", async () => {
  const live = new Set();
  const made = [];
  const renderers = Array.from({ length: 3 }, () => createTerminalRenderer(fakeTerminal(), { load: async () => fakeAddon(made), enabled: () => true, live, budget: 2 }));
  for (const renderer of renderers) { renderer.show(); await settle(); renderer.hide(); }
  assert.deepEqual(renderers.map((renderer) => renderer.kind), ["dom", "webgl", "webgl"]);
  renderers[1].show();
  renderers[0].show();
  await settle();
  assert.deepEqual(renderers.map((renderer) => renderer.kind), ["webgl", "webgl", "dom"], "the least recently shown gives its context back");
  for (const renderer of renderers) renderer.dispose();
  assert.equal(live.size, 0);
});

test("the setting or a missing WebGL2 keeps the DOM renderer, and a disposed view loads nothing", async () => {
  let loads = 0;
  const off = createTerminalRenderer(fakeTerminal(), { load: async () => { loads++; return fakeAddon(); }, enabled: () => false, live: new Set() });
  off.show();
  await settle();
  assert.equal(off.kind, "dom");
  const gone = createTerminalRenderer(fakeTerminal(), { load: async () => { loads++; return fakeAddon(); }, enabled: () => true, live: new Set() });
  gone.show();
  gone.dispose();
  await settle();
  assert.equal(gone.kind, "dom");
  assert.equal(loads, 1);
});

test("the last line a terminal drew is the refusal's reason", () => {
  const lines = ["", "$ codex", "会话账号已变化，请重新读取会话列表", "  ", ""];
  const buffer = { length: lines.length, getLine: (index) => ({ translateToString: () => lines[index] }) };
  assert.equal(terminalLastLine(buffer), "会话账号已变化，请重新读取会话列表", "the last line with text");
  assert.equal(terminalLastLine({ length: 0, getLine: () => undefined }), "");
  assert.equal(terminalLastLine(undefined), "");
});

test("a refused Codex launch is not an ordinary close", () => {
  const ended = (fields) => ({ cli_session_provider: "codex", closed: true, exit_code: 75, ...fields });
  assert.equal(codexLaunchRefused(ended()), true);
  assert.equal(codexLaunchRefused(ended({ exit_code: 0 })), false, "Codex quit on its own");
  assert.equal(codexLaunchRefused(ended({ exit_code: undefined })), false, "an older server sends no status");
  assert.equal(codexLaunchRefused(ended({ closed: false })), false, "only an ended terminal");
  assert.equal(codexLaunchRefused(ended({ cli_session_provider: "" })), false, "a Claude or plain terminal ending with 75 is its own business");
});
