// A display session is one RFB stream to a guest display (the browser or
// Claude Desktop) that outlives the pane showing it. The pane unmounts on
// every switch to a terminal, the home page or another app; with a server that
// speaks cage-display.v1 the stream is kept, hidden, for the next visit:
//
//   hide   {"type":"visibility","visible":false}, no more update requests
//   show   {"type":"visibility","visible":true}, one full update, and the
//          last painted frame stays until the guest has restored its window
//
// The server closes a stream that stayed hidden for 60 s (code 4001). Any
// close while hidden ends the session without reconnecting: the next visit
// connects afresh. A server that did not select the protocol gets today's
// behaviour: hiding the pane closes its stream.
//
// This module holds no DOM or noVNC code, so it runs under node --test. The
// opener (remote-session.js) creates the RFB client and reports its events
// through session.handle().

export const displayControlProtocol = "cage-display.v1";
// "binary" first: a server that only knows the older subprotocol still picks it.
export const displayProtocols = ["binary", displayControlProtocol];
export const displayHeldCloseCode = 4001;
// The client's own bound on a hidden stream, for a server or proxy that never
// sends the close: a little longer than the server's 60 s.
export const displayHoldMs = 65_000;
// A pane hidden this long asks for its size again on return. The guest lowers
// its refresh rate after three idle seconds; a size request restores it.
export const displayRestoreAfterMs = 3_000;
// The guest restores its window in the background. Until the framebuffer shows
// more than an empty desktop the last painted frame stays, at most this long.
export const displayFrameGateMs = 1_000;

export function createDisplaySessions({ open, now = () => Date.now(), setTimer = setTimeout, clearTimer = clearTimeout, createHost = () => null } = {}) {
  const sessions = new Map();

  function create(key, options) {
    let holdTimer = null, failed = false;
    const session = {
      key, options, state: "connecting", client: null, info: null, perf: null, sizeSync: null,
      host: createHost(), view: null, hidden: false, hiddenAt: 0, gateUntil: 0, fitRatio: undefined,
      // Replaced by the opener: places the canvas again after the ratio changed.
      refit() {},
      get holdable() {
        return session.state === "connected" && session.client?.cageProtocol === displayControlProtocol;
      },
      // True while a resumed stream should keep its last frame on screen.
      holdsFrame() { return session.gateUntil > now(); },
      releaseFrame() { session.gateUntil = 0; },
      handle(event) {
        if (session.state === "closed") return;
        if (event === "connect") {
          session.state = "connected";
          if (session.view) session.view.connected(session, { resumed: false, hiddenFor: 0 });
          // The pane went away while connecting: there is nothing to hold.
          else dispose();
          return;
        }
        if (event === "securityfailure") { failed = true; return; }
        // "disconnect" (the stream closed) or "failure" (it never opened).
        finish();
        const view = session.view;
        session.view = null;
        // A held stream is not reconnected; its pane connects when shown.
        view?.lost(session, { failed: failed || event === "failure" });
      },
      attach(box, view) {
        if (session.state === "closed") return () => {};
        session.view = view;
        if (session.host && box && session.host.parentNode !== box) box.appendChild(session.host);
        if (session.state === "connected") {
          if (session.hidden) view.connected(session, { resumed: true, hiddenFor: show() });
          else view.connected(session, { resumed: false, hiddenFor: 0 });
        }
        return () => detach(view);
      },
      dispose,
    };

    function detach(view) {
      if (session.view !== view) return;
      session.view = null;
      session.host?.remove?.();
      if (session.holdable) hide();
      else dispose();
    }

    function hide() {
      if (session.hidden) return;
      session.hidden = true;
      session.hiddenAt = now();
      session.gateUntil = 0;
      session.client.cageSendControl({ type: "visibility", visible: false });
      session.client.cageSetUpdatesPaused(true);
      session.perf?.hidden();
      holdTimer = setTimer(dispose, displayHoldMs);
    }

    function show() {
      const hiddenFor = Math.max(0, now() - session.hiddenAt);
      session.hidden = false;
      clearTimer(holdTimer);
      holdTimer = null;
      session.client.cageSendControl({ type: "visibility", visible: true });
      session.client.cageSetUpdatesPaused(false);
      session.gateUntil = now() + displayFrameGateMs;
      session.perf?.shown();
      return hiddenFor;
    }

    function finish() {
      session.state = "closed";
      clearTimer(holdTimer);
      holdTimer = null;
      session.sizeSync?.dispose();
      if (sessions.get(key) === session) sessions.delete(key);
      session.host?.remove?.();
    }

    function dispose() {
      if (session.state === "closed") return;
      finish();
      session.view = null;
      session.client?.disconnect();
    }

    sessions.set(key, session);
    try { open(session); } catch { session.handle("failure"); }
    return session;
  }

  return {
    // The live session for key, or a new one. A closed session is replaced.
    acquire(key, options) {
      const current = sessions.get(key);
      return current && current.state !== "closed" ? current : create(key, options);
    },
    get: (key) => sessions.get(key),
    // Ends a session whose pane is gone for good (Claude Desktop stopped).
    release(key) { const current = sessions.get(key); if (current && !current.view) current.dispose(); },
    get size() { return sessions.size; },
  };
}

// Whether a downscaled framebuffer is a single colour within tolerance: the
// empty desktop a guest shows for a moment while it restores a window.
export function uniformPixels(data, tolerance = 2) {
  for (let i = 4; i < data.length; i += 4) {
    if (Math.abs(data[i] - data[0]) > tolerance || Math.abs(data[i + 1] - data[1]) > tolerance ||
        Math.abs(data[i + 2] - data[2]) > tolerance) return false;
  }
  return true;
}
