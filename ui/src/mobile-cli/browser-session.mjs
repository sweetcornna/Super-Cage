import { browserRequest, desktopRequest, browserFrameRequest, desktopFrameRequest } from "./transport.mjs";

const clamp = (value, low, high) => Math.max(low, Math.min(high, value));
export function viewportSize(element) {
  return {
    width: clamp(Math.round(element.clientWidth), 240, 1440),
    height: clamp(Math.round(element.clientHeight), 160, 1600),
  };
}

export function framePoint(rect, frame, x, y) {
  const width = frame.content_width || frame.width;
  const height = frame.content_height || frame.height;
  const scale = Math.min(rect.width / width, rect.height / height);
  if (!(scale > 0)) return null;
  const left = rect.left + (rect.width - width * scale) / 2;
  const top = rect.top + (rect.height - height * scale) / 2;
  const px = Math.floor((x - left) / scale);
  const py = Math.floor((y - top) / scale);
  return px >= 0 && px < width && py >= 0 && py < height ? {x: px, y: py, scale} : null;
}

const frameTagPattern = /^[a-f0-9]{32}$/;
// How long one negotiated read may be held by the host. The host never holds
// longer than 15 s; the link answers a request after 40 s at the latest.
const holdMs = 12000;

// The frame metadata of a binary reply is unpadded base64url JSON.
export function frameMeta(header) {
  if (typeof header !== "string" || !/^[A-Za-z0-9_-]+$/.test(header)) throw new Error("主机返回的画面无效。");
  const binary = atob(header.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((header.length + 3) % 4));
  const bytes = Uint8Array.from(binary, c => c.charCodeAt(0));
  let meta;
  try { meta = JSON.parse(new TextDecoder("utf-8", {fatal: true}).decode(bytes)); }
  catch { throw new Error("主机返回的画面无效。"); }
  if (!meta || typeof meta !== "object" || Array.isArray(meta)) throw new Error("主机返回的画面无效。");
  return meta;
}

function jpegBlob(base64) {
  const binary = atob(base64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return new Blob([bytes], {type: "image/jpeg"});
}

// Only frame reads repeat. One input or resize is followed by a fresh frame
// before the next input is accepted. A failed write is never replayed.
//
// A host that announces it (the X-Cage-Frame-Stream header on a frame reply)
// also answers "next frame after this tag" reads: the JPEG as the body, held
// until the picture changes. Everything else, and any refusal of such a read
// by an older host or link, keeps the original polling below.
export function connectDisplay(stage, update, id, start, tool, initialURL) {
  const desktop = tool === "desktop";
  const request = desktop ? desktopRequest : browserRequest;
  const frameRequest = desktop ? desktopFrameRequest : browserFrameRequest;
  const hidden = () => typeof document !== "undefined" && document.hidden === true;
  const toolName = desktop ? "Claude Desktop" : "远程浏览器";
  let waitingSince = 0, homeNotice = "";
  const base = "/" + id;
  let alive = true, visible = !hidden(), opened = false, opening = true, writing = false;
  let frame = null, read, timer, paused = false, mayExist = false;
  let generation = 0, actionFrame = false, quietReads = 0, writeType = "";
  let pixelRatioSupported = false;
  // "unknown" until a frame reply says whether the host answers negotiated reads;
  // "off" after one was refused. Held reads that return at once are not holds.
  let stream = "unknown", streamProven = false, holdBroken = false, quickReturns = 0;
  // Requests are numbered as they start. A frame that finishes after a newer
  // one was applied is dropped, as is one the host captured earlier than the
  // last one shown.
  let issued = 0, applied = 0, lastSeq = 0, lastJsonImage = "";
  // Object URLs of the frames in use, oldest first. The newest is on screen and
  // the one before may still be while the page swaps pictures; nothing older is needed.
  const urls = [];
  const retain = url => {
    if (urls.includes(url)) return;
    urls.push(url);
    while (urls.length > 2) URL.revokeObjectURL(urls.shift());
  };
  const revokeAll = () => { while (urls.length) URL.revokeObjectURL(urls.shift()); };
  const displaySize = () => ({...viewportSize(stage),
    ...(pixelRatioSupported ? {pixel_ratio:clamp(window.devicePixelRatio || 1, 1, 8)} : {})});
  const needsResize = size => frame && (frame.width !== size.width || frame.height !== size.height ||
    (pixelRatioSupported && frame.requested_pixel_ratio !== size.pixel_ratio));
  let queuedScroll = null, scrollTimer;
  // Only untransmitted displacement lives in this queue. A scroll is taken
  // out before sending; neither an interrupted write nor an old document replays it.
  function cancelScroll() {
    clearTimeout(scrollTimer); scrollTimer = null; queuedScroll = null;
  }
  let awaitingResizeFrame = false, pendingAddress = null;
  const lifetime = new AbortController();
  function discardAddress() {
    awaitingResizeFrame = false;
    pendingAddress?.resolve(false);
    pendingAddress = null;
  }
  const closeRemote = () => {
    if (mayExist) void request(base + "/close", {body: {}, keepalive: true}).catch(() => {});
  };
  const publish = (state) => { if (alive && visible) update(homeNotice && state.frame ? {...state, homeNotice} : state); };
  const cancelRead = () => { clearTimeout(timer); read?.abort(); read = null; };
  function failure(error, operation) {
    discardAddress(); cancelScroll();
    frame = null; revokeAll();
    paused = true;
    if (!desktop && operation === "frame" && error.status === 503) opened = mayExist = false;
    const reopen = !opened || [401, 404, 410, 423].includes(error.status) || (error.status === 403 && operation !== "action");
    const message = error.status === 404 && !opened ? "这台电脑尚不支持" + toolName + "，请更新电脑端 Super Cage。" :
      error.message;
    publish({error: message, uncertain: operation === "action" && !error.status, reopen});
  }
  const sameSurface = (first, second) => first && second && first.document === second.document &&
    first.width === second.width && first.height === second.height &&
    first.content_width === second.content_width && first.content_height === second.content_height;
  function finishFrame(changed, fromAction = false) {
    awaitingResizeFrame = false;
    if (pendingAddress) {
      const {url, resolve} = pendingAddress;
      pendingAddress = null;
      void send({type:"navigate", url}, frame).then(resolve);
      return;
    }
    if (changed || fromAction) publish({frame, busy:false});
    const size = displaySize();
    if (needsResize(size)) {
      cancelScroll();
      timer = setTimeout(resize, 0);
      return;
    }
    if (queuedScroll && !sameSurface(queuedScroll.expected, frame)) cancelScroll();
    if (queuedScroll) { flushScroll(); return; }
    quietReads = changed || fromAction ? 0 : Math.min(5, quietReads + 1);
    // The first follow-up catches delayed paint after an input. Stable pages
    // use conditional reads and progressively return to the idle cadence. A
    // host that holds reads is the pacing itself: ask again at once.
    const holding = stream === "on" && !holdBroken && frameTagPattern.test(frame?.frame_tag || "");
    const delay = holding ? 0 : desktop ? 450 : [90, 180, 380, 800, 1500, 2500][quietReads];
    timer = setTimeout(readFrame, delay);
  }
  // Returns whether the frame differs from the one shown, or null when the
  // request ended or the frame came too late to be shown.
  async function decodeFrame(next, valid, ticket, blob) {
    if (next?.id !== id || !(blob ? blob.size > 0 : typeof next.image === "string" && next.image) ||
        typeof next.document !== "string" || !Number.isInteger(next.width) || next.width <= 0 ||
        !Number.isInteger(next.height) || next.height <= 0) throw new Error("主机返回的画面无效。");
    if ((next.content_width !== undefined || next.content_height !== undefined) &&
        (!Number.isFinite(next.content_width) || next.content_width <= 0 ||
         !Number.isFinite(next.content_height) || next.content_height <= 0)) throw new Error("主机返回的画面尺寸无效。");
    const tagged = frameTagPattern.test(next.frame_tag || "");
    const samePicture = !!frame && (tagged ? frame.frame_tag === next.frame_tag : !blob && next.image === lastJsonImage);
    const changed = !samePicture || !sameSurface(frame, next) || frame.focus !== next.focus ||
      frame.url !== next.url || frame.title !== next.title || frame.password !== next.password || frame.window !== next.window || frame.shared !== next.shared ||
      frame.login_supported !== next.login_supported || frame.content_width !== next.content_width || frame.content_height !== next.content_height;
    let src = frame?.image;
    if (!samePicture) {
      // A blob URL keeps the JPEG as bytes: no base64 string in the page, in
      // React state or in a data: URL the engine would parse again.
      src = URL.createObjectURL(blob || jpegBlob(next.image));
      try {
        const image = new Image();
        image.src = src;
        await image.decode();
      } catch (error) {
        URL.revokeObjectURL(src);
        throw error;
      }
    }
    const late = ticket < applied || (Number.isSafeInteger(next.frame_seq) && next.frame_seq < lastSeq);
    if (!valid() || late) {
      if (!samePicture) URL.revokeObjectURL(src);
      return valid() ? undefined : null;
    }
    applied = Math.max(applied, ticket);
    if (Number.isSafeInteger(next.frame_seq)) lastSeq = next.frame_seq;
    if (!samePicture) { retain(src); lastJsonImage = blob ? "" : next.image; }
    if (next.url && next.url !== "about:blank") homeNotice = "";
    if (!desktop && next.action_frame === true) actionFrame = true;
    pixelRatioSupported = desktop && next.pixel_ratio_supported === true;
    waitingSince = 0;
    frame = {...next, image:src};
    return changed;
  }
  // A frame that arrived out of order was dropped; the next read goes on from
  // the newer one that is already shown.
  const droppedLate = changed => changed === undefined;
  async function readFrame() {
    if (!alive || !visible || !opened || paused || writing || read || hidden()) return;
    const controller = new AbortController();
    read = controller;
    const valid = () => read === controller && !controller.signal.aborted && alive && visible && !paused;
    const ticket = ++issued;
    const known = frame;
    const negotiated = stream === "on";
    const tag = (negotiated || actionFrame) && frameTagPattern.test(known?.frame_tag || "") ? known.frame_tag : "";
    const holds = negotiated && tag !== "" && !holdBroken;
    const query = negotiated ? "?" + (tag ? "after=" + tag + (holds ? "&wait=" + holdMs : "") + "&" : "") + "fmt=jpeg" :
      tag ? "?after=" + tag : "";
    const startedAt = Date.now();
    try {
      const reply = await frameRequest(base + "/frame" + query, {signal:controller.signal});
      if (!valid()) return;
      if (reply.stream && stream === "unknown") stream = "on";
      const next = reply.blob ? frameMeta(reply.meta) : reply.json;
      let changed = false;
      if (next?.unchanged === true) {
        if (!tag || frame !== known || next.id !== id || next.frame_tag !== tag) throw new Error("远程画面的版本不匹配，请重新读取。");
        // A host that answers a held read at once is not holding it: go back
        // to timed reads rather than asking in a tight loop.
        if (holds) quickReturns = Date.now() - startedAt < 250 ? quickReturns + 1 : 0;
        if (quickReturns >= 2) holdBroken = true;
      } else {
        changed = await decodeFrame(next, valid, ticket, reply.blob);
        if (changed === null) return;
        if (droppedLate(changed)) changed = false;
      }
      if (negotiated) streamProven = true;
      read = null;
      finishFrame(changed);
    } catch (error) {
      if (valid()) {
        if (negotiated && !streamProven && [400, 404, 415].includes(error.status)) {
          // An older host, or an older link in front of it, refused the
          // negotiated read. This session goes on with the original polling.
          stream = "off";
          timer = setTimeout(readFrame, 0);
        } else if (error.status === 425 && (!waitingSince || Date.now() - waitingSince < 90000)) {
          waitingSince ||= Date.now();
          frame = null;
          publish({waiting:true});
          timer = setTimeout(readFrame, 1000);
        } else {
          failure(error, "frame");
        }
      }
    } finally {
      if (read === controller) read = null;
    }
  }
  function canSend(expected, focus) {
    return alive && visible && !writing && !paused && frame && expected &&
      sameSurface(frame, expected) &&
      (!focus || focus === frame.focus);
  }
  async function send(payload, expected, operation = "action") {
    if (!canSend(expected, payload.focus)) return false;
    if (["navigate","back","forward","reload"].includes(payload.type)) homeNotice = "";
    if (payload.type !== "scroll") cancelScroll();
    cancelRead();
    const ticket = ++issued;
    const current = generation;
    const valid = () => alive && visible && !paused && current === generation;
    writing = true; writeType = payload.type;
    if (operation === "resize") awaitingResizeFrame = true;
    publish({frame, busy:true});
    let feedback = false;
    try {
      const body = {...payload, document:expected.document};
      if (actionFrame) body.return_frame = true;
      const result = await request(base + "/action", {body, signal:lifetime.signal});
      if (!valid()) return false;
      if (actionFrame && result?.image) {
        feedback = (await decodeFrame(result, valid, ticket)) !== null;
      } else {
        if (result?.id !== id) throw new Error("主机未确认这次操作，请重新读取画面。");
        frame = null;
      }
      return valid();
    } catch (error) {
      if (valid()) failure(error, operation);
      return false;
    } finally {
      writing = false; writeType = "";
      if (alive && visible && !paused) {
        if (feedback && valid()) finishFrame(true, true);
        else void readFrame();
      }
    }
  }
  function flushScroll() {
    clearTimeout(scrollTimer); scrollTimer = null;
    if (!queuedScroll || writing || !frame || paused || !alive || !visible) return;
    const next = queuedScroll;
    if (!sameSurface(next.expected, frame)) { cancelScroll(); return; }
    const dx = clamp(next.dx, -3200, 3200), dy = clamp(next.dy, -3200, 3200);
    next.dx -= dx; next.dy -= dy;
    if (!next.dx && !next.dy) queuedScroll = null;
    void send({type:"scroll", x:next.x, y:next.y, dx, dy}, frame);
  }
  function scroll(expected, {x,y,dx,dy}) {
    if (desktop || !alive || !visible || paused || !opened || (!dx && !dy) ||
        (writing && writeType !== "scroll") || (frame && !sameSurface(expected, frame))) return;
    if (!queuedScroll || queuedScroll.x !== x || queuedScroll.y !== y || !sameSurface(queuedScroll.expected, expected)) {
      queuedScroll = {expected, x, y, dx:0, dy:0};
    }
    queuedScroll.dx += dx; queuedScroll.dy += dy;
    if (!queuedScroll.dx && !queuedScroll.dy) { cancelScroll(); return; }
    // Coalesce moves within one display tick. Ongoing writes drain the still
    // unsent movement once their next authenticated frame is available.
    if (!scrollTimer) scrollTimer = setTimeout(() => {scrollTimer = null; flushScroll();}, 16);
  }
  function resize() {
    if (!frame || !alive || !visible || writing || paused) return;
    const size = displaySize();
    if (needsResize(size)) {
      void send({type: "resize", ...size}, frame, "resize");
    }
  }
  async function open() {
    try {
      const body = {id, ...viewportSize(stage)};
      if (tool === "browser" && initialURL) body.url = initialURL;
      if (tool === "claude") body.url = "https://claude.ai/";
      const result = await request("/open", {body});
      if (result?.id !== id) throw new Error("主机未确认这次画面连接。");
      if (tool === "browser" && typeof result.home_notice === "string") homeNotice = result.home_notice;
      actionFrame = !desktop && result.action_frame === true;
      opened = true; mayExist = true;
      void readFrame();
    } catch (error) {
      mayExist = !error.status || error.status >= 500;
      if (alive) failure(error, "open");
    } finally {
      opening = false;
      if (!alive) void closeRemote();
    }
  }
  const observer = new ResizeObserver(() => {
    // Rotation/keyboard animations can report several intermediate sizes.
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(resize, 160);
  });
  let resizeTimer;
  observer.observe(stage);
  if (start) {
    void open();
  } else {
    // History navigation or a page reload resumes the known session by read.
    // A new host browser always requires another explicit open gesture.
    opened = true; opening = false; mayExist = true;
    void readFrame();
  }
  // The page tells the same thing; either way a hidden page stops reading.
  const onVisibility = () => display.visibility(!hidden());
  if (typeof document !== "undefined") document.addEventListener("visibilitychange", onVisibility);
  const display = {
    send, canSend, scroll, cancelScroll,
    async navigate(url) {
      if (!alive || !visible || !opened || paused) throw new Error("请先恢复远程画面，再前往网址。");
      if (awaitingResizeFrame && !pendingAddress) {
        return new Promise(resolve => {pendingAddress = {url, resolve};});
      }
      if (writing || !frame || pendingAddress) throw new Error("上一项操作尚未完成，请稍后再次前往。");
      return send({type: "navigate", url}, frame);
    },
    offline() {
      if (!alive) return;
      generation++; cancelRead(); clearTimeout(resizeTimer);
      failure(new Error("网络已断开，恢复后请重新读取画面。"), writing && writeType !== "resize" ? "action" : "frame");
    },
    refresh() {
      if (!opened || !alive || !visible) return;
      paused = false; waitingSince = 0;
      publish({});
      void readFrame();
    },
    visibility(active) {
      if (active === visible) return;
      visible = active;
      if (!visible) {
        // A hidden page holds no read open and starts none.
        generation++; cancelScroll(); discardAddress();
        cancelRead(); clearTimeout(resizeTimer); frame = null; revokeAll();
        if (alive) update({});
      } else if (alive) {
        if (!opened && !opening) publish({error: "远程画面未连接。", reopen: true});
        else if (paused) publish({error: "请重新读取远程画面。", reopen: !opened});
        else void readFrame();
      }
    },
    close() {
      if (!alive) return;
      alive = false; generation++; cancelScroll();
      discardAddress();
      cancelRead(); clearTimeout(resizeTimer); observer.disconnect();
      if (typeof document !== "undefined") document.removeEventListener("visibilitychange", onVisibility);
      frame = null; revokeAll();
      lifetime.abort();
      // If opening is still in flight, its finally sends close after the
      // server has processed open. An early close could miss the new session.
      if (!opening) void closeRemote();
    },
  };
  return display;
}
