import React, { useEffect, useState } from "react";
import { ChevronLeft, History, LoaderCircle } from "lucide-react";
import { Modal } from "./modal";
import { api } from "./api";
import { clearedWhen, earlierName, earlierPath, earlierTitle } from "./earlier-parts.mjs";
import "./earlier-parts.css";

// 清除前的对话 on the desktop: what /clear left behind in a session's terminal,
// newest first; each part reads only and can become an ordinary session again.
export function EarlierPartsDialog({ session, onClose, onSeparated }) {
  const [parts, setParts] = useState(null);
  const [open, setOpen] = useState(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [done, setDone] = useState("");
  useEffect(() => {
    const controller = new AbortController();
    api(earlierPath("/cli/sessions", session.id), undefined, undefined, {}, { signal: controller.signal })
      .then((answer) => setParts(answer.parts || []))
      .catch((failure) => { if (!controller.signal.aborted) setError(failure.message); });
    return () => controller.abort();
  }, [session.id]);
  const show = async (part) => {
    setError(""); setDone(""); setOpen({ part, view: null });
    try { setOpen({ part, view: await api(earlierPath("/cli/sessions", session.id, part.cli_id)) }); }
    catch (failure) { setError(failure.message); setOpen(null); }
  };
  const separate = async () => {
    if (busy || !open) return;
    setBusy(true); setError("");
    try {
      const result = await api(earlierPath("/cli/sessions", session.id, open.part.cli_id, "separate"), {});
      setParts((list) => (list || []).filter((part) => part.cli_id !== open.part.cli_id));
      setDone("“" + earlierTitle(open.part) + "”已恢复为单独会话，可在会话列表中打开。");
      setOpen(null);
      onSeparated?.(result);
    } catch (failure) { setError(failure.message); }
    finally { setBusy(false); }
  };
  return <Modal title="清除前的对话" onClose={onClose} wide>
    <div className="modal-body earlier-dialog">
      <p className="earlier-intro">“{session.title || "会话"}”在终端里清除上下文前的对话。它们只读保留，仍属于这个会话。</p>
      {error && <p className="earlier-error" role="alert">{error}</p>}
      {done && <p className="earlier-done" role="status">{done}</p>}
      {open ? <section className="earlier-part" aria-label={"清除前的对话：" + earlierTitle(open.part)}>
        <button type="button" className="quiet-button earlier-back" onClick={() => setOpen(null)}><ChevronLeft size={15} aria-hidden="true" />全部清除前的对话</button>
        <p className="earlier-meta">清除于 {clearedWhen(open.part.cleared_at)} · 只读</p>
        {!open.view ? <p className="earlier-meta"><LoaderCircle size={14} className="spin" aria-hidden="true" />正在读取…</p>
          : <ol className="earlier-turns">{(open.view.items || []).map((turn) => <li key={turn.id}>
            <p className="earlier-prompt">{turn.prompt}</p>
            {turn.text && <p className="earlier-reply">{turn.text}</p>}
          </li>)}</ol>}
        <div className="earlier-actions"><button type="button" disabled={busy || !open.view} onClick={separate}>{busy ? "正在恢复…" : "恢复为单独会话"}</button></div>
      </section>
        : parts === null ? !error && <p className="earlier-meta"><LoaderCircle size={14} className="spin" aria-hidden="true" />正在读取…</p>
        : parts.length === 0 ? <p className="earlier-meta">这个会话里没有可显示的清除前对话。</p>
        : <ul className="earlier-list">{parts.map((part) => <li key={part.cli_id}>
          <button type="button" aria-label={earlierName(part)} onClick={() => show(part)}>
            <History size={15} aria-hidden="true" />
            <span><strong>{earlierTitle(part)}</strong><small>清除于 {clearedWhen(part.cleared_at)}{part.turns > 0 ? " · " + part.turns + " 轮" : ""}</small></span>
          </button>
        </li>)}</ul>}
    </div>
  </Modal>;
}
