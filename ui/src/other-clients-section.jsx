import React, { useCallback, useEffect, useRef, useState } from "react";
import { LoaderCircle, MoreHorizontal } from "lucide-react";
import { api } from "./api";
import { Modal } from "./modal";
import { ProviderMark } from "./provider-mark";
import {
  ADDED_LINE, AGENTS_PATH, EMPTY_LINE, LEAD, NOTE, addDialog, agentJob, currentJob, jobEnded, normalizeAgents, removeDialog, requestFor, rowFor, waitLine,
} from "./other-clients-state.mjs";
import "./other-clients.css";

// Settings → 其他客户端 (design 8): the optional agent clients a person can add
// to this environment, one hairline list. Loaded lazily with its section; it
// asks the host once when it opens (with the guest read, so a client installed
// by hand shows up) and once after each of its own jobs ends, never by a
// timer. Progress and failure come from the job the app already polls.
function Mark({ row }) {
  if (row.mark.builtin) return <ProviderMark provider={row.mark.builtin} size={22} decorative />;
  return <i className="provider-mark other-client-monogram" aria-hidden="true">{row.mark.monogram}</i>;
}

function RowMenu({ row, onPick }) {
  const [open, setOpen] = useState(false);
  const button = useRef(null);
  useEffect(() => {
    if (!open) return undefined;
    // Escape closes the menu only, not the settings page behind it (as a dialog does).
    const key = (event) => {
      if (event.key !== "Escape") return;
      event.preventDefault();
      event.stopPropagation();
      setOpen(false);
      button.current?.focus();
    };
    document.addEventListener("keydown", key, true);
    return () => document.removeEventListener("keydown", key, true);
  }, [open]);
  return <span className="other-client-more">
    <button type="button" ref={button} className="quiet-button other-client-icon" aria-label={"更多操作：" + row.name} aria-haspopup="menu" aria-expanded={open}
      onClick={() => setOpen((value) => !value)}>
      <MoreHorizontal size={16} aria-hidden="true" />
    </button>
    {open && <>
      <div className="menu-dismiss" onMouseDown={() => setOpen(false)} />
      <div className="menu other-client-menu" role="menu" aria-label={row.name}>
        {row.menu.map((item) => <button key={item.id} type="button" role="menuitem" className="danger" disabled={item.disabled}
          onClick={() => { setOpen(false); onPick(item.id); }}>{item.label}</button>)}
      </div>
    </>}
  </span>;
}

function AddBody({ dialog }) {
  return <div className="modal-body other-clients-dialog">
    <dl>
      {dialog.rows.map((row) => <div key={row.label} className="other-clients-fact">
        <dt>{row.label}</dt>
        <dd>
          {row.text}
          {row.details && <details>
            <summary>展开查看全部</summary>
            <ul>{row.details.map((group) => <li key={group.label}><span>{group.label}</span>{group.hosts.join("、")}</li>)}</ul>
          </details>}
        </dd>
      </div>)}
    </dl>
  </div>;
}

export default function OtherClientsSection({ status }) {
  const [data, setData] = useState(null);
  const [error, setError] = useState("");
  const [dialog, setDialog] = useState(null);
  const [removeData, setRemoveData] = useState(false);
  const [local, setLocal] = useState(null);
  const [sending, setSending] = useState(false);
  const [added, setAdded] = useState("");
  const raw = status?.job;
  const job = currentJob(raw, local);
  // What each entry was last asked to do in this visit, for 重试.
  const operations = useRef({});
  const running = agentJob(raw);
  if (running?.running) operations.current[running.id] = running.operation;
  const anyJobRunning = Boolean(raw?.running) || Boolean(local && !local.error);

  const load = useCallback((verify, signal) => api(AGENTS_PATH + (verify ? "?verify=1" : ""), undefined, undefined, {}, { signal })
    .then((answer) => { const next = normalizeAgents(answer); setData(next || { entries: [], environment: { running: false } }); setError(""); })
    .catch((reason) => { if (!signal?.aborted) setError(reason.message); }), []);
  useEffect(() => {
    const controller = new AbortController();
    void load(true, controller.signal);
    return () => controller.abort();
  }, [load]);

  // A job of this section ended: read the list again, once.
  const before = useRef(raw);
  useEffect(() => {
    const was = before.current;
    before.current = raw;
    const accepted = Boolean(local && raw?.id && raw.id === local.jobId);
    if (accepted) setLocal(null);
    // An ended job is noticed when it was seen running, or, if it was over before the next poll, when it is the accepted one.
    if (!(jobEnded(was, raw) || (accepted && !raw.running))) return;
    void load(false);
    const ended = agentJob(raw);
    if (ended && ended.operation === "install" && !raw.error && !raw.canceled) setAdded(ended.id);
  }, [raw, local, load]);
  // The poll never showed the accepted job (it was over before the next read): do not hold the row.
  useEffect(() => {
    if (!local || local.error) return undefined;
    const timer = setTimeout(() => { setLocal(null); void load(false); }, 12000);
    return () => clearTimeout(timer);
  }, [local, load]);

  const send = async (action, entry, options = {}) => {
    const [path, body] = requestFor(action, entry.id, options);
    const operation = action === "cancel" ? "" : path.split("/").at(-1);
    setSending(true);
    try {
      const result = await api(path, body);
      if (action !== "cancel") { operations.current[entry.id] = operation; setAdded(""); setLocal(result?.id ? { id: entry.id, operation, jobId: result.id } : null); }
    } catch (reason) {
      if (action !== "cancel") { operations.current[entry.id] = operation; setLocal({ id: entry.id, operation, error: reason.message }); }
      else setError(reason.message);
    } finally { setSending(false); }
  };
  const pick = (entry, action) => {
    if (action === "add") setDialog({ kind: "add", entry, ...addDialog(entry, data.environment) });
    else if (action === "remove") { setRemoveData(false); setDialog({ kind: "remove", entry, ...removeDialog(entry) }); }
    else void send(action, entry, { job: job?.jobId || "" });
  };
  const confirm = async () => {
    const { kind, entry } = dialog;
    setDialog(null);
    await send(kind === "add" ? "add" : "remove", entry, { removeData });
  };

  const environment = data?.environment || { running: false };
  const rows = (data?.entries || []).map((entry) => rowFor(entry, { environment, job, anyJobRunning, added, operations: operations.current }));
  const mineRunning = Boolean(job?.running) || (data?.entries || []).some((entry) => entry.operation);
  const line = data ? waitLine(environment, anyJobRunning, mineRunning) : "";
  return <div className="other-clients-page">
    <div className="section-title">
      <h3>其他客户端</h3>
      <p>{LEAD}</p>
    </div>
    <section className="settings-group other-clients" aria-label="可添加的客户端" aria-busy={!data && !error}>
      {!data && !error && <p className="other-clients-loading" role="status"><LoaderCircle size={15} className="spin" aria-hidden="true" />正在读取…</p>}
      {error && <p className="error-message" role="alert">{error}</p>}
      {data && !rows.length && <p className="field-help">{EMPTY_LINE}</p>}
      {rows.length > 0 && <ul className="other-clients-list">
        {rows.map((row) => <li key={row.id} className={"other-client" + (row.muted ? " muted" : "")} data-state={row.state} data-id={row.id}>
          <span className="other-client-mark"><Mark row={row} /></span>
          <div className="other-client-text">
            <div className="other-client-title">
              <strong>{row.name}</strong>
              {row.parts.map((part, index) => <span key={index}>{part}</span>)}
            </div>
            {row.error
              ? <p className={"other-client-meta " + row.error.tone} role="status">{row.error.line}</p>
              : row.meta && <p className={"other-client-meta" + (row.meta === ADDED_LINE ? " added" : "")} aria-live="polite">{row.meta}</p>}
            {row.bar && <div className="other-client-bar" role="progressbar" aria-label={row.name + "的进度"} aria-valuemin={0} aria-valuemax={100}
              aria-valuenow={row.bar.fraction === null ? undefined : Math.round(row.bar.fraction * 100)}>
              <i style={{ width: Math.max(2, Math.round((row.bar.fraction ?? 0) * 100)) + "%" }} />
            </div>}
          </div>
          <div className="other-client-actions">
            {row.actions.map((action) => <button key={action.id} type="button" className={action.kind === "ghost" ? "quiet-button" : undefined}
              disabled={action.disabled || sending} onClick={() => pick(rowEntry(data, row.id), action.id)}>{action.label}</button>)}
            {row.menu.length > 0 && <RowMenu row={row} onPick={(action) => pick(rowEntry(data, row.id), action)} />}
          </div>
        </li>)}
      </ul>}
      {line && <p className="other-clients-wait" role="status">{line}</p>}
      <p className="footnote other-clients-note">{NOTE}</p>
    </section>
    {dialog && <Modal title={dialog.title} onClose={() => setDialog(null)}
      footer={<>
        <button type="button" onClick={() => setDialog(null)}>取消</button>
        <button type="button" className={dialog.kind === "remove" ? "danger-fill" : "primary"} onClick={confirm}>{dialog.confirm}</button>
      </>}>
      {dialog.kind === "add"
        ? <AddBody dialog={dialog} />
        : <div className="modal-body other-clients-dialog">
          <p>{dialog.body}</p>
          {dialog.checkbox && <label className="check-line other-clients-check">
            <input type="checkbox" checked={removeData} onChange={(event) => setRemoveData(event.target.checked)} />
            <span>{dialog.checkbox}</span>
          </label>}
        </div>}
    </Modal>}
  </div>;
}

const rowEntry = (data, id) => data.entries.find((entry) => entry.id === id);
