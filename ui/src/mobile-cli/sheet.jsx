import React, { useEffect, useRef } from "react";
import { X } from "lucide-react";
import { useBackDismiss } from "./web-shell.mjs";

// A bottom sheet on phones, a centered card on wide screens.
export function Sheet({ open = true, title, onClose, children, actions, className = "", full = false }) {
  const panel = useRef(null);
  useBackDismiss(open, onClose);
  useEffect(() => {
    const dialog = panel.current;
    if (!dialog) return;
    if (open && !dialog.open) { dialog.showModal(); dialog.querySelector(".sheet-content")?.focus({ preventScroll: true }); }
    if (!open && dialog.open) dialog.close();
  }, [open]);
  return <dialog ref={panel} className={"sheet" + (full ? " full" : "") + (className ? " " + className : "")} aria-label={title}
    onCancel={(event) => { event.preventDefault(); onClose?.(); }}
    onClick={(event) => { if (event.target === panel.current) onClose?.(); }}>
    <div className="sheet-body">
      <header className="sheet-head"><h2>{title}</h2>{onClose && <button type="button" className="icon-button" aria-label="关闭" onClick={onClose}><X size={19} /></button>}</header>
      <div className="sheet-content" tabIndex={-1}>{children}</div>
      {actions && <footer className="sheet-actions">{actions}</footer>}
    </div>
  </dialog>;
}

export function Confirm({ title, children, confirm, danger, busy, onConfirm, onCancel }) {
  return <Sheet title={title} onClose={busy ? undefined : onCancel} className="confirm"
    actions={<>
      <button type="button" className="action secondary" disabled={busy} onClick={onCancel}>取消</button>
      <button type="button" className={"action " + (danger ? "danger" : "primary")} disabled={busy} onClick={onConfirm}>{confirm}</button>
    </>}>
    <div className="confirm-text">{children}</div>
  </Sheet>;
}
