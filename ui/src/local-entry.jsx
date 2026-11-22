import React, { useCallback, useEffect, useRef, useState } from "react";
import { Check, Copy, LoaderCircle, Pencil } from "lucide-react";
import { api, writeClipboard } from "./api";
import { EmptyState } from "./drawings";
import {
  commandRows,
  endKeptConfirm,
  entryStateLine,
  includeDialog,
  includeRow,
  keptRows,
  loadError,
  offConfirm,
  relativeTime,
  revokeConfirm,
  shortFingerprint,
  sortedDevices,
  unfilteredNote,
  unreachedNote,
  validDeviceName,
} from "./local-entry-state.mjs";
import "./local-entry.css";

const ENTRY_PART = "从你自己的终端或 Herdr 连进来";

// Settings → 本机终端, its second part, for the environment this workbench
// runs (design 6.2): turn the local SSH entry on or off, copy the commands,
// list and revoke this environment's devices. Loaded lazily with the
// section (local-terminal-page.jsx). It never polls:
// it reads the state when it opens, after each action, when the window comes
// back into view, and when the environment's readiness changes.
export default function LocalEntryPage({ onConfirm, environmentReady }) {
  const [view, setView] = useState(null);
  const [failure, setFailure] = useState(null);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [copied, setCopied] = useState("");
  const copiedTimer = useRef();
  const load = useCallback(async () => {
    try {
      setView(await api("/entry"));
      setFailure(null);
    } catch (e) {
      setFailure(loadError(e));
    }
  }, []);
  useEffect(() => {
    load();
  }, [load, environmentReady]);
  useEffect(() => {
    const refresh = () => {
      if (document.visibilityState === "visible") load();
    };
    window.addEventListener("focus", refresh);
    document.addEventListener("visibilitychange", refresh);
    return () => {
      window.removeEventListener("focus", refresh);
      document.removeEventListener("visibilitychange", refresh);
      clearTimeout(copiedTimer.current);
    };
  }, [load]);

  const run = async (name, action) => {
    setBusy(name);
    setError("");
    setNotice("");
    try {
      const next = await action();
      if (next) setView(next);
    } catch (e) {
      setError(e.message);
    } finally {
      setBusy("");
    }
  };

  // The Include line is shown, word for word, before anything is written.
  const askInclude = async (withSecondary) => {
    let preview;
    try {
      preview = await api("/entry/include");
    } catch {
      return { write: false, preview: null };
    }
    if (preview.present || preview.error) return { write: false, preview };
    const dialog = includeDialog(preview);
    const answer = await onConfirm({
      title: dialog.title,
      body: (
        <>
          {dialog.intro}
          <code className="local-entry-include-line">{dialog.line}</code>
          {dialog.block && (
            <>
              {dialog.aliasText}
              <pre className="local-entry-include-block">{dialog.block}</pre>
            </>
          )}
          {dialog.warning && <strong className="local-entry-include-warning">{dialog.warning}</strong>}
          {dialog.after}
        </>
      ),
      secondary: withSecondary ? dialog.actions.secondary : undefined,
      confirm: withSecondary ? dialog.actions.confirm : "写入",
    });
    if (!answer) return null;
    return { write: answer === true, preview };
  };

  const writeInclude = async (preview) => {
    try {
      await api("/entry/include", { token: preview.token });
    } catch (e) {
      setError(e.status === 409 ? e.message : "无法写入 ~/.ssh/config：" + e.message.replace(/^无法写入 ~\/\.ssh\/config：/, ""));
    }
    return api("/entry");
  };

  const turnOn = async () => {
    const choice = await askInclude(true);
    if (!choice) return;
    await run("on", async () => {
      const next = await api("/entry/on", {});
      return choice.write ? writeInclude(choice.preview) : next;
    });
  };
  const addInclude = async () => {
    const choice = await askInclude(false);
    if (!choice?.write) return;
    await run("include", () => writeInclude(choice.preview));
  };
  const turnOff = async () => {
    if (!(await onConfirm(offConfirm))) return;
    await run("off", () => api("/entry/off", {}));
  };
  const revoke = async (device) => {
    if (!(await onConfirm(revokeConfirm(device)))) return;
    await run("revoke:" + device.id, async () => {
      const next = await api("/entry/devices/" + encodeURIComponent(device.id) + "/revoke", {});
      setNotice(unreachedNote(next?.unreached));
      return next;
    });
  };
  const endKept = async (row) => {
    if (!(await onConfirm(endKeptConfirm(row)))) return;
    await run("end:" + row.id, async () => {
      await api("/terminals/" + encodeURIComponent(row.id), undefined, "DELETE");
      return api("/entry");
    });
  };
  const rename = (device, name) =>
    run("rename:" + device.id, () => api("/entry/devices/" + encodeURIComponent(device.id) + "/rename", { name }));
  const copy = async (row) => {
    try {
      await writeClipboard(row.command);
      setCopied(row.id);
      clearTimeout(copiedTimer.current);
      copiedTimer.current = setTimeout(() => setCopied(""), 1500);
    } catch {
      setError("复制失败：请手动选中命令。");
    }
  };

  const line = entryStateLine(view, { locked: failure?.locked });
  const part = (children) => (
    <section className="settings-group local-terminal-part local-entry" aria-label={ENTRY_PART}>
      <h3>{ENTRY_PART}</h3>
      <p className="field-help">在任何终端里用 ssh，或在 Herdr 里，连接到这个环境。环境看不到你的电脑。</p>
      {children}
    </section>
  );
  if (failure && !failure.locked) {
    return part(<p className="local-entry-error" role="alert">{failure.text}</p>);
  }
  if (!view || failure?.locked) {
    return part(
      <p className="local-entry-state" data-tone={line.tone} role="status">
        <span className="local-entry-dot" aria-hidden="true" />
        {line.text}
      </p>
    );
  }
  if (view.state === "unsupported") {
    return part(
      <EmptyState drawing="terminal" title="这个环境暂不支持本机终端">
        本机终端目前只支持 Linux 环境。
      </EmptyState>
    );
  }
  if (!view.enabled) {
    return part(
      <>
        {error && <p className="local-entry-error" role="alert">{error}</p>}
        <EmptyState
          drawing="terminal"
          title="在你自己的终端里使用这个环境"
          action={
            <button type="button" className="clay" disabled={Boolean(busy)} onClick={turnOn}>
              {busy === "on" && <LoaderCircle size={16} className="spin" aria-hidden="true" />}
              开启本机终端
            </button>
          }
        >
          开启后，这台电脑上的任何终端都能用 ssh 连进这个环境。写入 ~/.ssh/config 之前会先问你。
        </EmptyState>
      </>
    );
  }

  const rows = commandRows(view);
  const include = includeRow(view);
  const devices = sortedDevices(view.devices);
  const kept = keptRows(view);
  return part(
    <>
      <p className="local-entry-state" data-tone={line.tone} role="status">
        <span className="local-entry-dot" aria-hidden="true" />
        {line.text}
        {view.revoked && (
          <button type="button" className="primary" disabled={Boolean(busy)} onClick={turnOn}>
            重新开启
          </button>
        )}
      </p>
      {error && <p className="local-entry-error" role="alert">{error}</p>}
      {notice && <p className="local-entry-notice" role="status">{notice}</p>}
      <ul className="local-entry-commands" aria-label="连接命令">
        {rows.map((row) => (
          <li key={row.id} data-row={row.id}>
            <div>
              <code>{row.command}</code>
              <span>
                {row.status && <span className="local-terminal-chip local-entry-status" data-connected={row.connected || undefined}>{row.status}</span>}
                {row.caption}
              </span>
            </div>
            <button
              type="button"
              className="quiet-button local-entry-copy"
              aria-label={copied === row.id ? "已复制" : "复制 " + row.command}
              title={copied === row.id ? "已复制" : "复制"}
              onClick={() => copy(row)}
            >
              {copied === row.id ? <Check size={16} /> : <Copy size={16} />}
            </button>
          </li>
        ))}
      </ul>
      <span className="sr-only" aria-live="polite">{copied ? "已复制" : ""}</span>
      <p className="field-help local-entry-unfiltered">{unfilteredNote()}</p>
      {include && (
        <p className="local-entry-include" data-state={include.state}>
          {include.text}
          {include.state === "absent" && (
            <button type="button" className="quiet-button" disabled={Boolean(busy)} onClick={addInclude}>
              写入 ~/.ssh/config
            </button>
          )}
        </p>
      )}
      <section className="local-entry-group local-entry-devices" aria-label="设备">
        <h4 className="local-terminal-subtitle">设备</h4>
        <p className="field-help">每台电脑一把钥匙。撤销后，那台电脑的连接会立即断开。</p>
        <ul>
          {devices.map((device) => (
            <DeviceRow key={device.id} device={device} busy={busy} onRename={rename} onRevoke={revoke} />
          ))}
        </ul>
      </section>
      {kept.rows.length > 0 && (
        <section className="local-entry-group local-entry-kept" aria-label={kept.title}>
          <h4 className="local-terminal-subtitle">{kept.title}</h4>
          <p className="field-help">{kept.help}</p>
          <ul>
            {kept.rows.map((row) => (
              <li key={row.id} className="local-entry-device">
                <div className="local-entry-device-main">
                  <div className="local-entry-device-title"><strong title={row.title}>{row.title}</strong></div>
                  <span className="local-entry-device-meta"><span>{row.detail}</span></span>
                </div>
                <button type="button" className="quiet-button local-entry-revoke" disabled={Boolean(busy)} onClick={() => endKept(row)}>
                  结束
                </button>
              </li>
            ))}
          </ul>
        </section>
      )}
      <div className="local-entry-footer">
        <button type="button" disabled={Boolean(busy)} onClick={turnOff}>
          {busy === "off" && <LoaderCircle size={16} className="spin" aria-hidden="true" />}
          关闭本机终端
        </button>
      </div>
    </>
  );
}

function DeviceRow({ device, busy, onRename, onRevoke }) {
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(device.name);
  const valid = validDeviceName(name);
  const save = async (event) => {
    event.preventDefault();
    if (!valid) return;
    if (name !== device.name) await onRename(device, name);
    setEditing(false);
  };
  return (
    <li className="local-entry-device" data-revoked={device.revoked || undefined}>
      <div className="local-entry-device-main">
        {editing ? (
          <form onSubmit={save} className="local-entry-rename">
            <input
              aria-label="设备名称"
              value={name}
              maxLength={64}
              autoFocus
              onChange={(e) => setName(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Escape") {
                  e.stopPropagation();
                  setName(device.name);
                  setEditing(false);
                }
              }}
            />
            <button type="submit" className="primary" disabled={!valid || Boolean(busy)}>保存</button>
            <button type="button" className="quiet-button" onClick={() => { setName(device.name); setEditing(false); }}>取消</button>
          </form>
        ) : (
          <div className="local-entry-device-title">
            <strong title={device.name}>{device.name}</strong>
            {device.current && <span className="local-entry-pill">这台电脑</span>}
            {device.revoked && <span className="local-entry-pill" data-tone="muted">已撤销</span>}
            {!device.revoked && (
              <button type="button" className="quiet-button local-entry-icon" aria-label={"重命名 " + device.name} title="重命名" onClick={() => setEditing(true)}>
                <Pencil size={15} />
              </button>
            )}
          </div>
        )}
        <span className="local-entry-device-meta">
          <span title={device.fingerprint}>{shortFingerprint(device.fingerprint)}</span>
          <span>{relativeTime(device.last_used)}</span>
        </span>
      </div>
      {!device.revoked && !editing && (
        <button type="button" className="quiet-button local-entry-revoke" disabled={Boolean(busy)} onClick={() => onRevoke(device)}>
          撤销
        </button>
      )}
    </li>
  );
}
