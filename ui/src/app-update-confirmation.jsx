import React, { useEffect, useState } from "react";
import { ArrowDownToLine, LoaderCircle } from "lucide-react";
import { api } from "./api";
import { Modal } from "./modal";
import { appUpdateConfirmation } from "./app-update-texts.mjs";

// The confirmation shown after the click and before anything stops: what
// will be interrupted, what must finish first, which terminals will close.
export default function AppUpdateConfirmation({ version, blocked, onCancel, onContinue }) {
  const [interruptions, setInterruptions] = useState(null);
  const [error, setError] = useState(null);
  const [loading, setLoading] = useState(true);
  useEffect(() => {
    const controller = new AbortController();
    api("/app-update/interruptions", undefined, undefined, {}, { signal: controller.signal })
      .then(setInterruptions)
      .catch(failure => { if (!controller.signal.aborted) setError(failure); })
      .finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, []);
  const view = appUpdateConfirmation({ version, interruptions, error, blocked });
  return (
    <Modal
      title={view.title}
      onClose={onCancel}
      footer={
        <>
          <span className="save-note">{loading ? "正在确认会被中断的任务…" : view.waitMessage || "不点击继续不会升级"}</span>
          <button onClick={onCancel}>{view.cancelLabel}</button>
          <button className="primary" disabled={loading || !view.canContinue} onClick={() => onContinue(view.confirmed)}>
            {loading ? <LoaderCircle size={15} className="spin" /> : <ArrowDownToLine size={15} />}
            {view.continueLabel}
          </button>
        </>
      }
    >
      <div className="modal-body app-update-confirmation" aria-busy={loading}>
        {!loading && <p>{view.body}</p>}
        {!loading && view.items.length > 0 && (
          <ul className="app-update-interruptions" aria-label="会被中断的任务">
            {view.items.map(item => <li key={item.id} data-unknown={item.unknown || undefined}>{item.label}</li>)}
          </ul>
        )}
        {!loading && view.mustWait.length > 0 && <p className="app-update-wait-heading">需要先完成：</p>}
        {!loading && view.mustWait.length > 0 && (
          <ul className="app-update-interruptions" aria-label="需要先完成的操作" data-kind="wait">
            {view.mustWait.map(label => <li key={label}>{label}</li>)}
          </ul>
        )}
        {!loading && view.note && <p className="footnote">{view.note}</p>}
      </div>
    </Modal>
  );
}
