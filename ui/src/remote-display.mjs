// The guest reports its running pixel density. Zoom changes its logical workspace size,
// while the page fits the resulting framebuffer into the available CSS area.
// A guest that follows its viewer is given pixelRatio, the density of the
// screen showing it: it renders one pixel per screen pixel, and zoom changes
// the density instead of the pixel count. The reduced-clarity setting (1.5)
// halves the pixels in each direction, but only on screens of density 2 or
// more, where each guest pixel then covers exactly two by two screen pixels:
// a whole-number enlargement keeps text sharp, a fractional one would blur it.
function frameSize({ width, height }, { desktop = false, zoom = 1, density = 2, pixelRatio } = {}) {
  if (width < (desktop ? 320 : 160) || height < 240) return null;
  const follow = pixelRatio > 0;
  const automatic = desktop && zoom === "auto" && !follow;
  const zoomFactor = Number.isFinite(zoom) && zoom > 0 ? zoom : 1;
  let scale = !follow && desktop && Number.isFinite(density) && density >= 1 && density <= 8
    ? density : density === 1.5 ? 1.5 : 2;
  let ratio = scale / zoomFactor;
  if (follow) {
    const screen = Math.max(1, Math.min(2, pixelRatio));
    ratio = density === 1.5 && screen >= 2 ? screen / 2 : screen;
  }
  // Native Windows uses guest DPI; Linux follows the host screen density.
  // Only native automatic mode limits allocation to twelve million pixels.
  const limit = Math.min(1, 8192 / (width * ratio), 8192 / (height * ratio),
    automatic ? Math.sqrt(12_000_000 / (width * height * ratio * ratio)) : 1);
  // Chromium applies a density in steps of 1/64.
  if (follow) scale = Math.round(Math.max(0.75, Math.min(4, ratio * limit * zoomFactor)) * 64) / 64;
  return {
    width: Math.max(desktop ? 320 : Math.ceil(160 * scale), Math.floor(width * ratio * limit)),
    height: Math.max(desktop ? 240 : Math.ceil(240 * scale), Math.floor(height * ratio * limit)),
    ...(follow ? { scale } : {}),
    ratio: ratio * limit,
  };
}

// displayGeometry is the size the guest display is asked for.
export function displayGeometry(box, options) {
  const size = frameSize(box, options);
  if (!size) return null;
  const { ratio: _ratio, ...request } = size;
  return request;
}

// displayRatio is the number of framebuffer pixels per CSS pixel that the
// requested geometry gives, or undefined for a pane too small to size.
export function displayRatio(box, options) {
  return frameSize(box, options)?.ratio;
}

// canvasFit places a framebuffer of frameWidth x frameHeight in a box of CSS
// pixels. The frame is drawn at its intended size, 1 / ratio CSS pixels per
// framebuffer pixel, which is one screen pixel per framebuffer pixel for a
// guest that follows the screen. A frame that does not fit, the previous size
// while the guest is still resizing, is reduced uniformly: never stretched,
// never distorted, and the rest of the box stays empty. pixelated marks a
// whole-number enlargement, drawn without smoothing so that text stays sharp.
export function canvasFit({ frameWidth, frameHeight, boxWidth, boxHeight, ratio, pixelRatio = 1 }) {
  if (!(frameWidth > 0 && frameHeight > 0 && boxWidth > 0 && boxHeight > 0)) return null;
  const density = pixelRatio > 0 ? pixelRatio : 1;
  const preferred = ratio > 0 ? 1 / ratio : 1 / density;
  // One screen pixel of slack: the box itself is rarely a whole number.
  const fits = frameWidth * preferred * density <= boxWidth * density + 1 &&
    frameHeight * preferred * density <= boxHeight * density + 1;
  const scale = fits ? preferred : Math.min(boxWidth / frameWidth, boxHeight / frameHeight);
  const screen = scale * density;
  const whole = Math.round(screen);
  const integral = whole >= 1 && Math.abs(screen - whole) < 1e-6;
  return {
    scale,
    width: frameWidth * scale,
    height: frameHeight * scale,
    exact: integral && whole === 1,
    pixelated: integral && whole >= 2,
  };
}

// Reports that the window moved to a screen of another density, or that the
// page zoom changed. A media query matches one ratio only, so each change
// registers the next. Returns the function that stops watching.
export function watchPixelRatio(changed, view = window) {
  let query, stopped = false;
  const update = () => {
    if (stopped) return;
    listen();
    changed();
  };
  const listen = () => {
    query = view.matchMedia(`(resolution: ${view.devicePixelRatio}dppx)`);
    // Older WebKit has no events here; the next window resize still adapts.
    query.addEventListener?.("change", update, { once: true });
  };
  listen();
  return () => { stopped = true; query.removeEventListener?.("change", update); };
}

// Serialize guest changes: a slow old request must finish before the newest
// size is applied. The first size goes out at once; after that a size is sent
// once the box has kept it for delay, so a sidebar toggle or a split drag asks
// the guest for one final size instead of every size on the way. Failed
// changes get bounded retries even without another window resize. Disposing
// never starts another request.
export function displaySizeSync(resize, { onError = () => {}, onSuccess = () => {}, delay = 180 } = {}) {
  let desired, applied = "", timer, busy = false, disposed = false, retries = 0;
  const key = (size) => size ? `${size.width}x${size.height}@${size.scale ?? ""}` : "";
  const schedule = (ms) => { clearTimeout(timer); timer = setTimeout(apply, ms); };
  async function apply() {
    timer = null;
    if (disposed || busy || !desired || key(desired) === applied) return;
    const target = desired;
    let retryDelay;
    busy = true;
    try {
      await resize(target);
      if (!disposed) {
        applied = key(target);
        if (key(desired) === applied) { retries = 0; onSuccess(); }
      }
    } catch (error) {
      if (!disposed && key(desired) === key(target)) {
        onError(error);
        if (retries < 3) retryDelay = 500 * 2 ** retries++;
      }
    } finally {
      busy = false;
      if (!disposed) {
        if (key(desired) !== key(target)) schedule(delay);
        else if (retryDelay !== undefined) schedule(retryDelay);
      }
    }
  }
  return {
    update(size) {
      if (disposed || !size || key(size) === key(desired)) return;
      const first = desired === undefined;
      desired = size;
      retries = 0;
      // A newer size replaces a pending one or a retry. A request in flight
      // schedules the newest size when it finishes.
      if (!busy) schedule(first ? 0 : delay);
    },
    retry() { if (!disposed) { applied = ""; retries = 0; schedule(0); } },
    dispose() { disposed = true; clearTimeout(timer); },
  };
}
