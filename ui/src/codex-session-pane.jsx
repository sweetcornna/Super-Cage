import React, { useEffect, useMemo, useRef, useState } from "react";
import { Archive, LoaderCircle, RefreshCw, Square, TerminalSquare } from "lucide-react";
import { api } from "./api";
import { useSessionSnapshot } from "./cli-sessions";
import { EmptyState } from "./drawings";
import { ProviderMark } from "./provider-mark";
import { afterOutcome, initialPoll, pollQuery, requestDeadline, requestDeadlineMs } from "./session-poll.mjs";
import { canContinueInTerminal, canStopFromPane, codexSessionAccountLabel, codexStateWord, CONTINUE_HINTS, PHONE_OWNER_NOTICES, READONLY_REASONS,
  sessionOwner, sessionViewPath } from "./session-provider.mjs";
import { MessageBody, ToolActivity, TranscriptContext } from "./mobile-cli/transcript.jsx";
import "./codex-session-pane.css";

const turnNotes = { failed: "本轮失败", interrupted: "本轮已中断", stopped: "本轮已停止" };
const isActive = (session) => Boolean(session?.active) || session?.turns?.at?.(-1)?.status === "running";

// Reads GET /api/cli/sessions/{id}: held (wait=25) when the server's list
// carries revisions, otherwise every 1 s while a turn runs and 4 s when idle
// (design 6.4). nonce restarts the read at once ("重新检查").
function useSessionView(id, held, nonce) {
  const [view, setView] = useState(null);
  const [error, setError] = useState("");
  useEffect(() => {
    setView(null);
    setError("");
    const controller = new AbortController();
    let poll = initialPoll({ held }), latest = null, timer = null;
    const run = async () => {
      const started = Date.now();
      const wait = latest ? pollQuery(poll).wait : 0;
      const deadline = requestDeadline(controller.signal, requestDeadlineMs(wait));
      let outcome;
      try {
        const next = await api(sessionViewPath(id, latest ? { after: latest.revision, epoch: latest.generation, wait } : {}).replace(/^\/api/, ""),
          undefined, undefined, {}, { signal: deadline.signal, empty: true });
        if (next) {
          latest = next;
          setView(next);
          outcome = { status: 200, revision: next.revision };
        } else outcome = { status: 204 };
        setError("");
      } catch (failure) {
        if (controller.signal.aborted) return;
        outcome = failure.status ? { status: failure.status } : { error: true };
        if (deadline.expired()) setError("会话暂时没有回应，正在重试");
        else if (!(wait && (failure.status === 400 || failure.status === 404))) setError(failure.message);
      } finally {
        deadline.done();
      }
      if (controller.signal.aborted) return;
      const step = afterOutcome(poll, outcome, { started, ended: Date.now(), wait, interval: isActive(latest) ? 1000 : 4000 });
      poll = step.poll;
      timer = setTimeout(run, step.delay);
    };
    void run();
    return () => { controller.abort(); clearTimeout(timer); };
  }, [id, held, nonce]);
  return { view, setView, error };
}

// The read-only pane of a Codex session that another device or process
// writes now (owner phone or external): the reason, the transcript, stop where
// allowed, and "在终端中继续" once nobody writes.
export function CodexSessionPane({ id, store, held, projectOf, accounts, titleId, sidebarButton, busy, onContinue, onArchive, onOpenWeb }) {
  const row = useSessionSnapshot(store, (list) => list.sessions.find((session) => session.id === id) || null);
  const [nonce, setNonce] = useState(0);
  const { view, setView, error: readError } = useSessionView(id, held, nonce);
  const [stop, setStop] = useState({ busy: false, error: "" });
  const [autoOpen, setAutoOpen] = useState(false);
  const [opening, setOpening] = useState({ busy: false, error: "" });
  const session = view || row;
  const owner = sessionOwner(session || { provider: "codex" });
  const last = session?.turns?.at?.(-1);
  const transcript = useRef(null);
  const context = useMemo(() => ({ sessionID: id, openWeb: onOpenWeb }), [id, onOpenWeb]);
  const continueHere = async () => {
    if (!session || opening.busy) return;
    setOpening({ busy: true, error: "" });
    const message = await onContinue(session);
    setOpening({ busy: false, error: message || "" });
  };
  // "完成后自动打开": once the phone's turn is over, continue in a terminal.
  const autoOpened = useRef(false);
  useEffect(() => {
    if (autoOpen && view && canContinueInTerminal(view) && !autoOpened.current) {
      autoOpened.current = true;
      void continueHere();
    }
  }, [autoOpen, view]);
  useEffect(() => { autoOpened.current = false; setStop({ busy: false, error: "" }); setOpening({ busy: false, error: "" }); }, [id]);
  // Follow the newest message while the reader is at the bottom.
  const follow = useRef(true);
  useEffect(() => {
    const box = transcript.current;
    if (box && follow.current) box.scrollTop = box.scrollHeight;
  }, [session?.turns?.length, last?.text?.length]);
  const stopTurn = async () => {
    if (!last || stop.busy) return;
    setStop({ busy: true, error: "" });
    try {
      setView(await api("/cli/sessions/" + encodeURIComponent(id) + "/stop", { request_id: last.id }));
      setStop({ busy: false, error: "" });
    } catch (failure) {
      setStop({ busy: false, error: failure.message });
    }
  };
  const title = session?.title || "新会话";
  const account = session ? codexSessionAccountLabel(session, accounts) : "";
  const reason = owner === "phone" || owner === "external" ? READONLY_REASONS[owner] : "";
  const notice = session?.connection_notice && session.connection_notice !== reason && !PHONE_OWNER_NOTICES.has(session.connection_notice)
    ? session.connection_notice : "";
  const hint = CONTINUE_HINTS[owner] || CONTINUE_HINTS.phone;
  // The server reuses its own terminal when this server's Codex holds the thread.
  const canContinue = Boolean(session) && (canContinueInTerminal(session) || owner === "desktop");
  return (
    <main className="management-page codex-session-page" aria-labelledby={titleId}>
      <header className="chrome management-header codex-session-header">
        {sidebarButton}
        <ProviderMark provider="codex" className="codex-session-mark" />
        <h1 id={titleId} tabIndex={-1} title={title}>{title}</h1>
        {account && <span className="chip codex-session-account" title="这段会话所用的 Codex 账号">{account}</span>}
        <span className="codex-session-state">{session ? codexStateWord(session) : ""}</span>
        <span className="codex-session-project" title={session?.workspace}>{session?.workspace ? projectOf(session.workspace) : ""}</span>
        <button className="icon-button codex-session-archive" aria-label="归档会话" title="归档会话" disabled={busy || !session}
          onClick={(event) => onArchive(session, event.currentTarget)}><Archive size={16} /></button>
      </header>
      <div className="management-content codex-session-content">
        <div className="codex-session-column">
          {(reason || notice) && (
            <section className={`codex-session-notice owner-${owner || "none"}`} aria-label="会话状态">
              <div className="codex-session-notice-text">
                {owner === "phone" && isActive(session) && <i className="live-dot" aria-hidden="true" />}
                <p role="status">{reason || notice}</p>
              </div>
              {reason && notice && <p className="codex-session-notice-detail">{notice}</p>}
              <div className="codex-session-notice-actions">
                {owner === "external" && (
                  <button onClick={() => setNonce((value) => value + 1)}><RefreshCw size={14} aria-hidden="true" />重新检查</button>
                )}
                {session && canStopFromPane(session) && (
                  <button onClick={stopTurn} disabled={stop.busy} aria-busy={stop.busy || undefined}>
                    {stop.busy ? <LoaderCircle size={14} className="spin" aria-hidden="true" /> : <Square size={13} aria-hidden="true" />}
                    {stop.busy ? "正在停止…" : "停止本轮"}
                  </button>
                )}
                {owner === "phone" && (
                  <label className="check-line codex-session-auto">
                    <input type="checkbox" checked={autoOpen} onChange={(event) => setAutoOpen(event.target.checked)} />
                    <span>完成后自动打开</span>
                  </label>
                )}
                <button className="primary" onClick={continueHere} disabled={!canContinue || opening.busy || busy}
                  title={canContinue ? undefined : hint} aria-describedby={canContinue ? undefined : "codex-continue-hint"}>
                  <TerminalSquare size={15} aria-hidden="true" />在终端中继续
                </button>
              </div>
              {!canContinue && <p id="codex-continue-hint" className="codex-session-hint">{hint}</p>}
              {stop.error && <p className="codex-session-error" role="alert">{stop.error}</p>}
              {opening.error && <p className="codex-session-error" role="alert">{opening.error}</p>}
            </section>
          )}
          {!reason && !notice && (
            <div className="codex-session-continue">
              <span>{owner === "desktop" ? "这段会话正在电脑的 Codex 终端中打开。" : "现在没有设备在写这段会话，可以在终端中继续。"}</span>
              <button className="primary" onClick={continueHere} disabled={!canContinue || opening.busy || busy}>
                <TerminalSquare size={15} aria-hidden="true" />在终端中继续
              </button>
              {opening.error && <p className="codex-session-error" role="alert">{opening.error}</p>}
            </div>
          )}
          {readError && <p className="codex-session-error" role="alert">{readError}</p>}
          <section className="codex-transcript" ref={transcript} aria-label="会话消息"
            onScroll={(event) => { const box = event.currentTarget; follow.current = box.scrollHeight - box.scrollTop - box.clientHeight < 64; }}>
            {!view && !readError && <p className="codex-session-loading" role="status"><LoaderCircle size={14} className="spin" aria-hidden="true" />正在读取会话…</p>}
            {view && !view.turns?.length && (
              <EmptyState drawing="chat" title="还没有对话">在终端中开始与 Codex 对话。</EmptyState>
            )}
            <TranscriptContext.Provider value={context}>
              {view?.turns?.map((turn) => (
                <React.Fragment key={turn.id}>
                  {turn.prompt && <article className="codex-message user" aria-label="你的消息"><MessageBody text={turn.prompt} /></article>}
                  <article className="codex-message" aria-label="回复">
                    <ToolActivity tools={turn.tools} />
                    {turn.text && <MessageBody text={turn.text} />}
                    {turn.status === "running" && !turn.text && <p className="codex-message-working" role="status"><i className="live-dot" aria-hidden="true" />正在运行…</p>}
                    {turn.error && <p className="codex-session-error">{turn.error}</p>}
                    {turnNotes[turn.status] && <p className="codex-message-note">{turnNotes[turn.status]}</p>}
                  </article>
                </React.Fragment>
              ))}
            </TranscriptContext.Provider>
          </section>
        </div>
      </div>
    </main>
  );
}
