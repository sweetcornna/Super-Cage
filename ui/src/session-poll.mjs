// Pacing of a held session request (GET /api/cli/sessions and
// GET /api/cli/sessions/{id}), as plain values so it can be tested without a
// clock. The server contract is in internal/workbench/mobile_cli_http.go:
//   - an answer carries "revision"; the next request sends after=<revision>
//     and wait=<s>, and the server holds it until something changes (200) or
//     the time is up (204);
//   - a client leaves at least 1 s between the starts of two requests;
//   - no "revision" (an older server), or 400/404 on the held form, means
//     today's plain polling;
//   - a 204 that arrives more than 1 s before its wait elapsed was not held:
//     the client waits its usual interval before the next request.
// Errors back off so that a dead server is not hammered.

export const HOLD_SECONDS = 25;
export const MIN_SPACING_MS = 1000;
export const LIST_INTERVAL_MS = 5000;
export const ERROR_BACKOFF_MS = [1000, 2000, 5000, 10000, 20000, 30000];

// mode: "probe" (no answer yet), "held" (the server sends revisions) or
// "plain" (an older server: poll at the plain interval, no hold).
export function initialPoll({ held = true } = {}) {
  return { mode: held ? "probe" : "plain", revision: "", failures: 0 };
}

// The query a request carries now: after and wait only once the server has
// shown that it knows revisions.
export function pollQuery(poll, wait = HOLD_SECONDS) {
  if (poll.mode !== "held" || !poll.revision) return { after: "", wait: 0 };
  return { after: poll.revision, wait };
}

// A request gets its wait plus this before the client gives up on it: a
// request that never answers (a half-open connection after the computer
// slept, a stalled tunnel) would otherwise hold the loop for good, since a
// loop never starts a second request while one is in flight. The server
// answers a held request by its wait (internal/workbench/session_hold.go).
export const REQUEST_SLACK_MS = 10000;
export const requestDeadlineMs = (wait) => (wait > 0 ? wait : HOLD_SECONDS) * 1000 + REQUEST_SLACK_MS;

// The signal of one request: aborted with parent, or once ms have passed
// (expired() tells the two apart). Call done() when the request has ended.
export function requestDeadline(parent, ms, { setTimer = setTimeout, clearTimer = clearTimeout } = {}) {
  const controller = new AbortController();
  let expired = false;
  const follow = () => controller.abort(parent.reason);
  if (parent?.aborted) follow();
  else parent?.addEventListener("abort", follow, { once: true });
  const timer = setTimer(() => {
    expired = true;
    controller.abort(new Error("请求超时"));
  }, ms);
  return {
    signal: controller.signal,
    expired: () => expired,
    done() { clearTimer(timer); parent?.removeEventListener("abort", follow); },
  };
}

export const backoffDelay = (failures) => ERROR_BACKOFF_MS[Math.min(Math.max(failures, 1), ERROR_BACKOFF_MS.length) - 1];

// The next state and the delay before the next request after one outcome:
//   { status: 200, revision }  a full answer (revision "" or absent: an older server)
//   { status: 204 }            unchanged
//   { status: 400 | 404 }      the server or a link filter does not know the held form
//   { error: true }            anything else (network, 5xx, 409, 423...)
// timing: { started, ended, wait (s, of that request), interval (plain ms) }.
export function afterOutcome(poll, outcome, { started, ended, wait = 0, interval = LIST_INTERVAL_MS }) {
  const spaced = Math.max(0, started + MIN_SPACING_MS - ended);
  const held = wait > 0;
  if (outcome.status === 200) {
    const revision = typeof outcome.revision === "string" || typeof outcome.revision === "number" ? String(outcome.revision) : "";
    if (!revision || poll.mode === "plain") return { poll: { mode: "plain", revision: "", failures: 0 }, delay: interval };
    return { poll: { mode: "held", revision, failures: 0 }, delay: spaced };
  }
  if (outcome.status === 204) {
    const next = { ...poll, failures: 0 };
    // An early 204 to a held request: the server did not hold it.
    if (held && ended - started < wait * 1000 - MIN_SPACING_MS) return { poll: next, delay: interval };
    return { poll: next, delay: held ? spaced : interval };
  }
  if ((outcome.status === 400 || outcome.status === 404) && (held || poll.mode === "held")) {
    return { poll: { mode: "plain", revision: "", failures: 0 }, delay: spaced };
  }
  const failures = poll.failures + 1;
  return { poll: { ...poll, failures }, delay: Math.max(spaced, backoffDelay(failures)) };
}
