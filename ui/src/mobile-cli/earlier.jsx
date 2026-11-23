import React, { useEffect, useState } from "react";
import { ChevronLeft, ChevronRight, History, LoaderCircle } from "lucide-react";
import { Sheet } from "./sheet";
import { MessageBody } from "./transcript";
import { request, sessionPath } from "./transport.mjs";
import { clearedWhen, earlierName, earlierPath, earlierTitle } from "../earlier-parts.mjs";
import "./earlier.css";

// 清除前的对话: what /clear left behind in this session, newest first; each
// opens read only and can become an ordinary session again.
export function EarlierSheet({ session, onClose, onSeparated }) {
  const base = sessionPath("").replace(/\/$/, "");
  const [parts, setParts] = useState(null);
  const [open, setOpen] = useState(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [done, setDone] = useState("");
  useEffect(() => {
    const controller = new AbortController();
    request(earlierPath(base, session.id), { signal: controller.signal })
      .then((answer) => setParts(answer.parts || []))
      .catch((failure) => { if (!controller.signal.aborted) setError(failure.message); });
    return () => controller.abort();
  }, [base, session.id]);
  const show = async (part) => {
    setError(""); setOpen({ part, view: null });
    try { setOpen({ part, view: await request(earlierPath(base, session.id, part.cli_id)) }); }
    catch (failure) { setError(failure.message); setOpen(null); }
  };
  const separate = async () => {
    if (busy || !open) return;
    setBusy(true); setError("");
    try {
      const result = await request(earlierPath(base, session.id, open.part.cli_id, "separate"), { body: {} });
      setParts((list) => (list || []).filter((part) => part.cli_id !== open.part.cli_id));
      setOpen(null); setDone("“" + earlierTitle(open.part) + "”已恢复为单独会话。");
      onSeparated?.(result);
    } catch (failure) { setError(failure.message); }
    finally { setBusy(false); }
  };
  return <Sheet title="清除前的对话" onClose={onClose} className="earlier-sheet" full={Boolean(open)}>
    {error && <p className="earlier-error" role="alert">{error}</p>}
    {done && <p className="note" role="status">{done}</p>}
    {open ? <section className="earlier-part" aria-label={"清除前的对话：" + earlierTitle(open.part)}>
      <button type="button" className="earlier-back" onClick={() => setOpen(null)}><ChevronLeft size={16} aria-hidden="true" />全部清除前的对话</button>
      <p className="earlier-meta">清除于 {clearedWhen(open.part.cleared_at)} · 只读</p>
      {!open.view ? <p className="note"><LoaderCircle size={15} className="spin" aria-hidden="true" />正在读取…</p>
        : <ol className="earlier-turns">{(open.view.items || []).map((turn) => <li key={turn.id}>
          <p className="earlier-prompt">{turn.prompt}</p>
          {turn.text && <MessageBody text={turn.text} />}
        </li>)}</ol>}
      <button type="button" className="action secondary" disabled={busy || !open.view} onClick={separate}>{busy ? "正在恢复…" : "恢复为单独会话"}</button>
    </section>
      : parts === null ? !error && <p className="note"><LoaderCircle size={15} className="spin" aria-hidden="true" />正在读取…</p>
      : parts.length === 0 ? <p className="note">这段会话里没有可显示的清除前对话。</p>
      : <ul className="earlier-list">{parts.map((part) => <li key={part.cli_id}>
        <button type="button" aria-label={earlierName(part)} onClick={() => show(part)}>
          <History size={16} aria-hidden="true" />
          <span><strong>{earlierTitle(part)}</strong><small>清除于 {clearedWhen(part.cleared_at)}{part.turns > 0 ? " · " + part.turns + " 轮" : ""}</small></span>
          <ChevronRight size={15} aria-hidden="true" />
        </button>
      </li>)}</ul>}
  </Sheet>;
}
