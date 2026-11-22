// One session list per window, read with the held request of
// session-poll.mjs, kept as a small store so that only the views that show the
// list re-render when it changes (audit UI-4). An unchanged answer causes no
// state change at all: the snapshot object stays the same.
import { codexCapabilities, SESSION_PROVIDERS, sessionListPath } from "./session-provider.mjs";
import { afterOutcome, initialPoll, LIST_INTERVAL_MS, pollQuery, requestDeadline, requestDeadlineMs } from "./session-poll.mjs";
import { reuseItems, sameValue } from "./poll-state.mjs";

const idle = Object.freeze({ sessions: Object.freeze([]), error: "", loading: false,
  capabilities: codexCapabilities(null), held: false });

// A notice means the guest could not say which sessions run: none is shown
// as running then, as before.
const rows = (answer) => (answer.sessions || []).map((session) =>
  answer.notice ? { ...session, active: false, running: false } : session);

// request(path, {signal}) resolves the answer, or null for 204, and rejects
// with an error carrying .status for an HTTP failure.
export function createSessionList({ request, now = () => Date.now(), setTimer = setTimeout, clearTimer = clearTimeout,
  interval = LIST_INTERVAL_MS, providers = SESSION_PROVIDERS } = {}) {
  let snapshot = idle;
  const listeners = new Set();
  let poll = initialPoll();
  let running = false, controller = null, timer = null, lastStart = -Infinity, generation = 0;

  const publish = (changes) => {
    const next = { ...snapshot, ...changes };
    if (Object.keys(next).every((key) => next[key] === snapshot[key])) return;
    snapshot = Object.freeze(next);
    for (const listener of [...listeners]) listener();
  };
  const schedule = (delay) => {
    clearTimer(timer);
    timer = setTimer(run, Math.max(0, delay));
  };
  const run = async () => {
    timer = null;
    if (!running || controller) return;
    const own = ++generation;
    const { after, wait } = pollQuery(poll);
    const started = now();
    lastStart = started;
    controller = new AbortController();
    const signal = controller.signal;
    const deadline = requestDeadline(signal, requestDeadlineMs(wait), { setTimer, clearTimer });
    let outcome;
    let answer = null;
    try {
      answer = await request(sessionListPath({ after, wait, providers }), { signal: deadline.signal });
      outcome = answer ? { status: 200, revision: answer.revision } : { status: 204 };
    } catch (failure) {
      if (signal.aborted || own !== generation) return;
      // No answer in time: an error like any other, so the loop backs off and asks again.
      const error = deadline.expired() ? new Error("会话列表暂时没有回应，正在重试") : failure;
      outcome = error?.status ? { status: error.status, error: true } : { error: true };
      if (!(outcome.status === 400 || outcome.status === 404) || !wait) {
        publish({ error: error?.message || "会话列表暂未更新", loading: false,
          sessions: snapshot.sessions.some((session) => session.active || session.running)
            ? snapshot.sessions.map((session) => ({ ...session, active: false, running: false })) : snapshot.sessions });
      }
    } finally {
      deadline.done();
      if (own === generation) controller = null;
    }
    if (!running || own !== generation) return;
    if (answer) {
      const capabilities = codexCapabilities(answer);
      publish({
        sessions: reuseItems(snapshot.sessions, rows(answer)),
        error: answer.notice || "",
        loading: false,
        capabilities: sameValue(capabilities, snapshot.capabilities) ? snapshot.capabilities : capabilities,
        held: Boolean(answer.revision),
      });
    } else if (outcome.status === 204 && snapshot.loading) publish({ loading: false });
    const next = afterOutcome(poll, outcome, { started, ended: now(), wait, interval });
    poll = next.poll;
    schedule(next.delay);
  };
  const abort = () => {
    generation++;
    controller?.abort();
    controller = null;
    clearTimer(timer);
    timer = null;
  };
  return {
    getSnapshot: () => snapshot,
    subscribe(listener) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    start() {
      if (running) return;
      running = true;
      schedule(lastStart + 1000 - now());
    },
    stop() {
      if (!running) return;
      running = false;
      abort();
      if (snapshot.loading) publish({ loading: false });
    },
    // Ask again (after an action, or the user's retry): the held request in
    // flight is dropped, so two never run at once. The user's retry goes at
    // once (its button stays disabled until the answer); an action's refresh
    // keeps the 1 s spacing.
    refresh({ user = false } = {}) {
      if (!running) return;
      if (user) publish({ loading: true });
      abort();
      schedule(user ? 0 : lastStart + 1000 - now());
    },
    // The providers follow Codex presence (codexSurfaces). Another set is
    // another list: the held request and its revision are dropped.
    setProviders(value) {
      if (!value || value === providers) return;
      providers = value;
      poll = initialPoll();
      if (!running) return;
      abort();
      schedule(lastStart + 1000 - now());
    },
    get running() { return running; },
    get mode() { return poll.mode; },
  };
}
