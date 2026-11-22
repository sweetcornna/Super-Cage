// Which renderer draws a terminal. xterm's WebGL addon draws the whole grid
// on the GPU; the DOM renderer rebuilds rows of spans on the main thread,
// which competes with typing echo and the display canvas while a TUI repaints.
//
// The choice is kept in localStorage under cage-terminal-renderer:
//   "auto"   WebGL on Blink engines (Chrome, Edge, WebView2), measured there;
//            the DOM renderer on WebKit (WKWebView, WebKitGTK) and others,
//            where it was not verified
//   "webgl"  WebGL wherever WebGL2 exists
//   "dom"    the DOM renderer everywhere
// A lost context or a failed start returns that terminal to the DOM renderer.
// Browsers cap live WebGL contexts, so only the terminals shown most recently
// keep one.

export const terminalRendererKey = "cage-terminal-renderer";
export const terminalRendererChoices = ["auto", "webgl", "dom"];
export const webglContextBudget = 4;

export function readTerminalRendererChoice(storage = globalThis.localStorage) {
  try {
    const value = storage?.getItem(terminalRendererKey);
    return terminalRendererChoices.includes(value) ? value : "auto";
  } catch { return "auto"; }
}

// Blink reports itself in client hints; its user agent says Chrome/ or Edg/.
// WKWebView and WebKitGTK say AppleWebKit without either.
export function browserEngine(nav = globalThis.navigator) {
  const brands = nav?.userAgentData?.brands?.map((item) => item.brand) || [];
  if (brands.some((brand) => /Chromium|Google Chrome|Microsoft Edge/.test(brand))) return "chromium";
  const agent = nav?.userAgent || "";
  if (/\b(?:Chrome|Chromium|Edg|HeadlessChrome)\//.test(agent)) return "chromium";
  if (/\bFirefox\//.test(agent)) return "gecko";
  if (/AppleWebKit\//.test(agent)) return "webkit";
  return "other";
}

export function wantsWebgl({ choice = "auto", engine, webgl2 }) {
  if (!webgl2 || choice === "dom") return false;
  return choice === "webgl" || engine === "chromium";
}

let webgl2;
export function webgl2Available(doc = globalThis.document) {
  if (webgl2 !== undefined) return webgl2;
  try {
    const context = doc?.createElement("canvas").getContext("webgl2");
    webgl2 = Boolean(context);
    // Do not hold on to a context of the browser's limited number.
    context?.getExtension("WEBGL_lose_context")?.loseContext();
  } catch { webgl2 = false; }
  return webgl2;
}

const loadWebgl = () => import("@xterm/addon-webgl").then((module) => module.WebglAddon);

// xterm's WebGL renderer floors the glyph advance to whole device pixels: a
// 7.875 px advance becomes a 7 px cell at 1x and 15 device pixels at 2x,
// which packs the text tighter than the DOM renderer and changes the grid.
// Its letterSpacing is added in device pixels, so one pixel of spacing when
// the advance is nearer the next pixel keeps every cell within half a device
// pixel of the DOM renderer's.
export function webglLetterSpacing(charWidth, pixelRatio) {
  const device = charWidth * pixelRatio;
  if (!Number.isFinite(device) || device <= 0) return 0;
  return Math.round(device) - Math.floor(device);
}

// The DOM-measured advance xterm uses for both renderers (FitAddon reads the
// same private services).
const measuredCharWidth = (terminal) => terminal._core?._charSizeService?.width;

// One controller per terminal. show() asks for WebGL, hide() lets the
// context go to a terminal shown later; the DOM renderer draws meanwhile.
function webglWanted() {
  const choice = readTerminalRendererChoice();
  // Probe WebGL2 only when the choice can use it: a probe creates a context.
  if (choice === "dom" || (choice === "auto" && browserEngine() !== "chromium")) return false;
  return wantsWebgl({ choice, engine: browserEngine(), webgl2: webgl2Available() });
}

export function createTerminalRenderer(terminal, {
  load = loadWebgl, enabled = webglWanted,
  live = sharedContexts, budget = webglContextBudget, onChange = () => {},
  measure = () => measuredCharWidth(terminal), pixelRatio = () => globalThis.devicePixelRatio || 1,
  watchRatio = watchPixelRatio,
} = {}) {
  let addon = null, loading = false, failed = false, disposed = false, wanted = false, unwatch = null;
  // Letter spacing only while WebGL draws; the DOM renderer keeps none.
  const align = () => {
    const spacing = addon ? webglLetterSpacing(measure(), pixelRatio()) : 0;
    if ((terminal.options.letterSpacing || 0) !== spacing) terminal.options.letterSpacing = spacing;
  };
  const release = () => {
    if (!addon) return;
    const current = addon;
    addon = null;
    unwatch?.();
    unwatch = null;
    live.delete(controller);
    try { current.dispose(); } catch { /* The DOM renderer is back either way. */ }
    if (!disposed) {
      align();
      onChange("dom");
    }
  };
  const controller = {
    get kind() { return addon ? "webgl" : "dom"; },
    get failed() { return failed; },
    show() {
      wanted = true;
      if (addon) { live.delete(controller); live.add(controller); return; }
      if (loading || failed || disposed || !enabled()) return;
      loading = true;
      Promise.resolve().then(load).then((WebglAddon) => {
        loading = false;
        if (disposed || !wanted || addon) return;
        const next = new WebglAddon();
        next.onContextLoss(() => { failed = true; release(); });
        terminal.loadAddon(next);
        addon = next;
        align();
        unwatch = watchRatio(align);
        live.add(controller);
        // The terminals shown longest ago give their contexts back.
        for (const other of live) {
          if (live.size <= budget) break;
          if (other !== controller) other.evict();
        }
        onChange("webgl");
      }).catch(() => {
        loading = false;
        failed = true;
        release();
        if (!disposed) onChange("dom");
      });
    },
    hide() { wanted = false; },
    // After a font change: the advance and so the spacing may differ.
    align,
    evict() { release(); },
    dispose() { disposed = true; wanted = false; release(); },
  };
  return controller;
}

const sharedContexts = new Set();

// Calls back when the device pixel ratio changes (another screen, zoom).
function watchPixelRatio(callback) {
  if (typeof globalThis.matchMedia !== "function") return () => {};
  let query = null;
  const arm = () => {
    query?.removeEventListener("change", changed);
    query = globalThis.matchMedia(`(resolution: ${globalThis.devicePixelRatio || 1}dppx)`);
    query.addEventListener("change", changed);
  };
  function changed() { arm(); callback(); }
  arm();
  return () => query?.removeEventListener("change", changed);
}

// A Codex launch the guest refused ends its terminal with exit status 75
// (codex_sessions.py LAUNCH_REFUSED, reported as exit_code by the terminal list); the
// pane's note then says it did not open and quotes the last line the launch drew.
export const codexLaunchRefused = (tab) => tab.cli_session_provider === "codex" && tab.closed === true && tab.exit_code === 75;

// The last line a terminal drew, from its buffer (xterm's buffer.active).
export function terminalLastLine(buffer) {
  let text = "";
  for (let index = buffer?.length ?? 0; index-- > 0 && !text;) text = buffer.getLine(index)?.translateToString(true).trim() ?? "";
  return text;
}
