// Opens the RFB clients behind display sessions (see display-session.mjs) and
// fits their canvas to the pane. Browser-only: it imports the patched noVNC.
import RFB from "../.generated/novnc/core/rfb.js";
import { api } from "./api";
import { runtimeSocketURL } from "./client-context.mjs";
import { guestKeys } from "./guest-keys.mjs";
import { installLocalCursorFallback } from "./remote-cursor.mjs";
import { canvasFit } from "./remote-display.mjs";
import { cagePerf } from "./display-perf.mjs";
import { createDisplaySessions, displayProtocols, uniformPixels } from "./display-session.mjs";

export function displayKey(desktop) {
  return runtimeSocketURL(`${desktop ? "/claude-desktop" : ""}/vnc`);
}

function createHost() {
  const host = document.createElement("div");
  host.className = "remote-host";
  return host;
}

// Places the canvas at its intended size in the top-left corner of the pane:
// one screen pixel per framebuffer pixel when the guest follows the screen,
// reduced uniformly only while an older, larger frame is still shown.
function installCanvasFit(client, session) {
  const display = client._display;
  const canvas = client._canvas;
  canvas.style.margin = "0";
  client._screen.style.overflow = "hidden";
  display.autoscale = (boxWidth, boxHeight) => {
    // A detached pane measures nothing; keep the last placement.
    if (!boxWidth || !boxHeight) return;
    // Before the first frame the empty canvas covers the pane, so pointer
    // input lands on it at once; there is nothing on it to stretch.
    if (!display.width || !display.height) {
      canvas.style.width = "100%";
      canvas.style.height = "100%";
      return;
    }
    const fit = canvasFit({
      frameWidth: display.width, frameHeight: display.height, boxWidth, boxHeight,
      ratio: session.fitRatio, pixelRatio: window.devicePixelRatio || 1,
    });
    if (!fit) return;
    display._rescale(fit.scale);
    canvas.style.imageRendering = fit.pixelated ? "pixelated" : "";
  };
  session.refit = () => {
    const box = client._screen.getBoundingClientRect();
    display.autoscale(box.width, box.height);
  };
}

// Counts painted frames and keeps the last painted frame on screen while a
// resumed stream still shows the guest's empty desktop. Also times the paint
// path: message handling (parsing and decoding) and the drawing of decoded
// rectangles, see window.__cagePerf.
function installFrameHooks(client, session, perf) {
  const display = client._display;
  const flip = display.flip.bind(display);
  const handlers = client._sock._eventHandlers;
  const handle = handlers.message;
  handlers.message = () => {
    const started = performance.now();
    try { handle(); } finally { perf.handled(performance.now() - started); }
  };
  const scan = display._scanRenderQ.bind(display);
  display._scanRenderQ = () => {
    const started = performance.now();
    try { scan(); } finally { perf.drew(performance.now() - started); }
  };
  let probe;
  const empty = () => {
    const source = display._backbuffer;
    if (!source.width || !source.height) return true;
    probe ??= document.createElement("canvas");
    probe.width = 32;
    probe.height = 20;
    const context = probe.getContext("2d", { willReadFrequently: true });
    context.imageSmoothingQuality = "high";
    context.drawImage(source, 0, 0, 32, 20);
    return uniformPixels(context.getImageData(0, 0, 32, 20).data);
  };
  display.flip = (fromQueue) => {
    // A flip behind queued image decodes runs later, from the queue.
    if (display._renderQ.length !== 0 && !fromQueue) return flip(fromQueue);
    // A hidden pane paints nothing; the damage is drawn when it is shown.
    if (session.hidden) return;
    if (session.holdsFrame()) {
      if (empty()) { perf.held(); return; }
      session.releaseFrame();
    }
    flip(fromQueue);
    perf.painted();
  };
}

// Streams whose server answered the display protocol with none. A browser
// refuses a WebSocket whose server selects no offered subprotocol, so such a
// server is asked again without one, as before, for the life of the page.
const legacyServers = new Set();

function openRemote(session) {
  const { desktop, guestPlatform } = session.options;
  const prefix = desktop ? "/claude-desktop" : "";
  // Windows and macOS guests stream through the native relay, which keeps
  // the previous protocol on every server version.
  const native = /^(windows|darwin|macos)/.test(guestPlatform || "");
  api(`${prefix}/vnc-credentials`).then(({ password, display_scale, dynamic_scale, pixel_wheel }) => {
    if (session.state === "closed") return;
    const perf = cagePerf().display(desktop ? "desktop" : "browser", session.key);
    session.perf = perf;
    // Such a guest renders at the density of the screen showing it.
    session.info = { density: display_scale, followScreen: dynamic_scale === true, keys: null };
    const start = (protocols) => {
      let socketOpened = false;
      const client = new RFB(session.host, session.key, {
        wsProtocols: protocols,
        credentials: { password }, cageBrowserDisplay: !desktop, cageCursorFallback: desktop && guestPlatform === "windows",
        // The Linux browser scrolls its page by the distance of a trackpad
        // gesture. A menu or a second pane declines it and gets wheel steps.
        cageWheelSink: !desktop && pixel_wheel === true
          ? wheel => api("/browser/wheel", wheel).then(reply => reply?.handled === true)
          : undefined,
      });
      session.client = client;
      let keys = null;
      if (!native) {
        // A Linux guest takes key events as sent; see guest-keys.mjs. The
        // other systems arrive here as "macos" and "windows-11".
        keys = guestKeys(client.sendKey.bind(client));
        client.sendKey = keys.sendKey;
      }
      session.info.keys = keys;
      installLocalCursorFallback(client._cursor, !desktop && ["darwin", "macos"].includes(guestPlatform));
      installCanvasFit(client, session);
      client.scaleViewport = true;
      client.resizeSession = false;
      // Bars around a frame of another size take the pane's own colour.
      client.background = "var(--paper)";
      client.qualityLevel = 8;
      client.compressionLevel = 2;
      installFrameHooks(client, session, perf);
      const raw = client._sock._websocket;
      raw?.addEventListener("open", () => { socketOpened = true; });
      raw?.addEventListener("message", (event) => perf.received(event.data?.byteLength || 0));
      client.addEventListener("connect", () => {
        if (session.client !== client) return;
        if (!protocols.length && !native) legacyServers.add(session.key);
        perf.connected(client.cageProtocol);
        session.handle("connect");
      });
      client.addEventListener("disconnect", () => {
        keys?.stop();
        if (session.client !== client) return;
        if (!socketOpened && protocols.length && session.state === "connecting") {
          try { start([]); return; } catch { /* reported as a failure below */ }
        }
        perf.closed(client.cageCloseCode);
        session.handle("disconnect");
      });
      client.addEventListener("securityfailure", () => { if (session.client === client) session.handle("securityfailure"); });
    };
    start(native || legacyServers.has(session.key) ? [] : displayProtocols);
  }).catch(() => {
    // The credentials could not be read, or the client could not be created.
    if (session.client) session.client.disconnect();
    session.handle("failure");
  });
}

export const displaySessions = createDisplaySessions({ open: openRemote, createHost });
