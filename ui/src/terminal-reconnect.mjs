// Terminal streams pass through proxies, relays and tunnels that drop a
// WebSocket after a period of silence, or lose it without any close. The
// server sends a heartbeat every 20 seconds and answers pings. The view pings
// on its own, notices a stream that went silent, and resumes a dropped stream
// after the last byte it received instead of replaying the whole history.

export const terminalKeepaliveMessage = JSON.stringify({ type: "ping" });
// Keepalive intervals for a visible page and a hidden one, which the browser
// may throttle to about one timer per minute anyway.
export const terminalKeepaliveMs = 25_000;
export const terminalHiddenKeepaliveMs = 60_000;
// How often the view checks the stream.
export const terminalWatchdogMs = 5_000;
// No frame of any kind for this long: ask the server whether the path still works.
export const terminalSilenceMs = 45_000;
// A ping sent to confirm a silent stream must be answered within this time.
export const terminalProbeMs = 5_000;
// A reconnect that completes within this time is not shown at all.
export const terminalNoticeDelayMs = 1_500;
// A connection attempt that has not opened by then is abandoned and retried.
// Waking a frozen workspace can take most of it.
export const terminalConnectTimeoutMs = 20_000;

// A terminal hidden this long gives its stream up until it is shown again,
// then resumes after the last byte it received. Streams carry heartbeats, so
// a hidden terminal would otherwise keep the network, the guest and the
// tenant's idle clock busy for nothing.
export const terminalSuspendMs = 120_000;

// A stream that stayed open this long was dropped by the network, not refused.
const stableMs = 10_000;
// After this many quick failures in a row the view stops and offers the button.
export const terminalReconnectLimit = 6;

// failures counts consecutive attempts that closed soon after connecting
// (or never connected). The returned delay is 0 when the view should stop.
export function terminalReconnectPlan(failures, openedFor) {
  const next = openedFor >= stableMs ? 1 : failures + 1;
  if (next > terminalReconnectLimit) return { failures: next, delay: 0 };
  return { failures: next, delay: Math.min(500 * 2 ** (next - 1), 15_000) };
}

// terminalStreamPath resumes after the last byte the view received, or asks
// for the kept history when it has received nothing yet.
//
// viewer, when given, identifies this view across reconnects (8-64 of
// A-Z a-z 0-9 _ -): the server then answers terminal queries from one
// viewer only, sizes the terminal to the one who typed last, and sends a
// {"type":"viewer"} frame first. Without it the stream is as before.
export function terminalStreamPath(id, offset, viewer) {
  const after = Number.isSafeInteger(offset) && offset >= 0 ? offset : 0;
  const path = `/terminals/${encodeURIComponent(id)}/stream?after=${after}`;
  return typeof viewer === "string" && /^[A-Za-z0-9_-]{8,64}$/.test(viewer) ? `${path}&viewer=${viewer}` : path;
}

// terminalHistoryPlan reads a history marker. The bytes that follow it are
// replayed output; reset means the view must clear its screen first because
// they do not continue what it shows. A server without offsets replays its
// whole history in one frame, so that view always resets and cannot resume.
export function terminalHistoryPlan(offset, marker) {
  const at = marker?.offset;
  if (!Number.isSafeInteger(at) || at < 0) return { resumable: false, reset: true, offset: null, size: null };
  const size = Number.isSafeInteger(marker.size) && marker.size > 0 ? marker.size : 0;
  return { resumable: true, reset: marker.reset === true || offset !== at, offset: at, size };
}

// terminalLiveness tells the watchdog what to do with an open stream:
// "ping" sends a keepalive, "probe" sends one that must be answered,
// "dead" drops the stream and "idle" waits. A silent stream is first probed,
// because a throttled or frozen page can run its timers before it delivers
// the messages that arrived meanwhile. Only a server known to send heartbeats
// is ever judged silent.
export function terminalLiveness({ now, lastReceived, lastPing, probe = 0, visible, heartbeats }) {
  if (heartbeats && probe && lastReceived < probe) return now - probe >= terminalProbeMs ? "dead" : "idle";
  if (heartbeats && now - lastReceived >= terminalSilenceMs) return "probe";
  if (now - lastPing >= (visible ? terminalKeepaliveMs : terminalHiddenKeepaliveMs)) return "ping";
  return "idle";
}
