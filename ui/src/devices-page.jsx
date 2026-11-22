import React, { useCallback, useEffect, useRef, useState } from "react";
import { Laptop, LoaderCircle, Smartphone } from "lucide-react";
import { api } from "./api";
import { getClientContext, isRemoteClient } from "./client-context.mjs";
import { AddDeviceDialog, JoinDialog } from "./devices-dialogs.jsx";
import { openComputer } from "./devices-open.mjs";
import { deviceArt } from "./devices-art.mjs";
import {
  atLimit, computerMeta, deviceMeta, deviceName, forgetConfirmation, keepAwakeText, limitOf, limitText, removeConfirmation, summaryText,
} from "./devices-state.mjs";
import "./devices.css";

// Settings → 设备: the phones and computers that can connect to this computer, and the computers this one has
// joined (design §6). Loaded lazily with its section: nothing here runs, loads or asks the host until it opens,
// and nothing ticks while it is open (the list is read on open, after each action and when the window comes
// back; a code on show is followed by held reads, see devices-dialogs.jsx).
export default function DevicesPage({ relay, savedRelay, busy: working, onRelayChange, onConfirm }) {
  const remote = isRemoteClient();
  // A tenant workspace is a machine of a server, not a computer to join from or to add a device to (a code is made
  // only in this computer's own window, devices.go): what was paired stays listed and can be removed.
  const portal = getClientContext().tenant_workspace === true;
  const canJoin = !portal;
  const [state, setState] = useState(null);
  const [joined, setJoined] = useState(null);
  const [error, setError] = useState("");
  const [dialog, setDialog] = useState("");
  const [busy, setBusy] = useState("");
  const [rowErrors, setRowErrors] = useState({});
  const [custom, setCustom] = useState(Boolean(relay));
  const [token, setToken] = useState("");
  const revision = useRef(0);

  const refresh = useCallback(async () => {
    const mine = ++revision.current;
    const [host, computers] = await Promise.all([
      remote ? null : api("/devices").catch((reason) => ({ failure: reason })),
      canJoin ? api("/client/devices").catch(() => null) : null,
    ]);
    // A newer read, or a change made meanwhile, wins over this answer.
    if (mine !== revision.current) return;
    if (host?.failure) setError(host.failure.status === 404 ? "这台电脑上的 Super Cage 还不支持设备互通，请先更新。" : host.failure.message);
    else if (host) { setState(host); setError(""); }
    setJoined(Array.isArray(computers?.computers) ? computers : null);
  }, [remote, canJoin]);
  useEffect(() => {
    void refresh();
    // Back from another window (the other device was added there, or the host changed): read again, not on a timer.
    const back = () => { if (!document.hidden) void refresh(); };
    document.addEventListener("visibilitychange", back);
    window.addEventListener("focus", back);
    return () => {
      document.removeEventListener("visibilitychange", back);
      window.removeEventListener("focus", back);
    };
  }, [refresh]);

  const fail = (id, reason) => setRowErrors((previous) => ({ ...previous, [id]: reason.message }));
  async function remove(device) {
    if (!(await onConfirm(removeConfirmation(device)))) return;
    setBusy(device.id);
    setRowErrors({});
    try {
      // Already removed (404) is as good as removed.
      await api("/devices/" + encodeURIComponent(device.id), undefined, "DELETE", {}, { empty: true }).catch((reason) => { if (reason.status !== 404) throw reason; });
      revision.current++;
      setState((previous) => ({ ...previous, devices: previous.devices.filter((item) => item.id !== device.id) }));
      void refresh();
    } catch (reason) { fail(device.id, reason); }
    finally { setBusy(""); }
  }
  async function forget(computer) {
    if (!(await onConfirm(forgetConfirmation(computer)))) return;
    setBusy(computer.id);
    setRowErrors({});
    try {
      await api("/client/devices/" + encodeURIComponent(computer.id), undefined, "DELETE", {}, { empty: true }).catch((reason) => { if (reason.status !== 404) throw reason; });
      revision.current++;
      setJoined((previous) => ({ ...previous, computers: previous.computers.filter((item) => item.id !== computer.id) }));
      void refresh();
    } catch (reason) { fail(computer.id, reason); }
    finally { setBusy(""); }
  }
  // The list of joined computers cannot be read: the gateway moves its file aside (never written over), and the join
  // dialog opens to join again.
  async function clearJoined() {
    setBusy("joined");
    setError("");
    try {
      await api("/client/devices", undefined, "DELETE");
      revision.current++;
      setJoined({ computers: [] });
      setDialog("join");
      void refresh();
    } catch (reason) { setError(reason.message); }
    finally { setBusy(""); }
  }
  async function open(computer) {
    setBusy(computer.id);
    setRowErrors({});
    try { await openComputer(computer.id, computer.name); }
    catch (reason) { fail(computer.id, reason); setBusy(""); }
  }

  const devices = state?.devices || [];
  const full = state ? atLimit(state) : false;
  const unsavedRelay = relay !== savedRelay;
  const cannotAdd = busy !== "" || full || unsavedRelay || (custom && !relay);
  const empty = state && !devices.length;
  const addButton = (
    <button type="button" className="clay" disabled={cannotAdd} onClick={() => setDialog("add")}>添加设备</button>
  );
  const closeDialog = () => { setDialog(""); void refresh(); };
  const computers = joined?.computers || [];

  return (
    <section className="remote-link-settings devices-page" aria-label="设备">
      {/* As the section it replaces, the page follows the settings page: nothing here can be started while the environment is being changed. The dialogs sit outside, so they are never disabled halfway. */}
      <fieldset className="settings-fields" disabled={working}>
      <div className="section-title">
        <h3>设备</h3>
        <p>手机和其他电脑可以连接这台电脑上的工作台。</p>
      </div>
      {error && <p className="error-message" role="alert">{error}</p>}
      {state?.problem && <p className="error-message" role="alert">{state.problem}</p>}
      {!state && !error && !remote && <p className="devices-status" role="status"><LoaderCircle size={15} className="spin" aria-hidden="true" />正在读取设备…</p>}
      {remote && <p className="field-help">请先切换到这台电脑的本机环境，再添加或管理手机和其他电脑。</p>}
      {portal && <p className="field-help">这是门户里的环境，不能在这里添加设备。手机请在 Super Cage 应用里用门户账号登录。</p>}
      {(state || joined) && (
        <div className="devices-actions">
          {state && !empty && !portal && addButton}
          {joined && <button type="button" disabled={busy !== ""} onClick={() => setDialog("join")}>加入其他电脑</button>}
        </div>
      )}
      {state && (
        <section className="devices-group" aria-label="可以连接这台电脑的设备">
          {empty ? (
            <div className="empty-state devices-empty">
              <span className="drawing drawing-laptop" aria-hidden="true" dangerouslySetInnerHTML={{ __html: deviceArt.laptop }} />
              <h3>还没有设备</h3>
              <p>手机或另一台电脑配对后，可以在这里看到它，并随时移除。</p>
              {!portal && <div className="empty-action">{addButton}</div>}
            </div>
          ) : (
            <>
              <div className="devices-group-head">
                <h4>可以连接这台电脑的设备</h4>
                <span className="devices-meta">{summaryText(devices)}</span>
              </div>
              <ul className="devices-list">
                {devices.map((device) => (
                  <li className="devices-row" key={device.id} aria-label={deviceName(device)}>
                    <span className="devices-row-icon" aria-hidden="true">{device.kind === "computer" ? <Laptop size={16} /> : <Smartphone size={16} />}</span>
                    <div className="devices-row-text">
                      <strong>{deviceName(device)}</strong>
                      <span className="devices-row-meta">{deviceMeta(device)}</span>
                      {rowErrors[device.id] && <span className="devices-row-error" role="alert">{rowErrors[device.id]}</span>}
                    </div>
                    {device.connected && <span className="devices-live" role="img" aria-label="已连接" title="已连接" />}
                    <button type="button" className="quiet-button" disabled={busy !== ""} aria-label={"移除 " + deviceName(device)} onClick={() => remove(device)}>
                      {busy === device.id ? <LoaderCircle size={14} className="spin" aria-hidden="true" /> : null}移除
                    </button>
                  </li>
                ))}
              </ul>
              {full && <p className="devices-limit" role="status">{limitText(limitOf(state))}</p>}
              {state.keep_awake?.active && <p className="field-help devices-note">{keepAwakeText}</p>}
              {state.keep_awake?.error && <p className="devices-note" role="alert">主机防休眠未生效：{state.keep_awake.error}</p>}
            </>
          )}
        </section>
      )}
      {joined?.problem && (
        <p className="devices-note devices-problem" role="alert">
          {joined.problem}
          <button type="button" className="compact" disabled={busy !== ""} onClick={clearJoined}>
            {busy === "joined" ? <LoaderCircle size={14} className="spin" aria-hidden="true" /> : null}清除并重新加入
          </button>
        </p>
      )}
      {computers.length > 0 && (
        <section className="devices-group" aria-label="我加入的电脑">
          <div className="devices-group-head"><h4>我加入的电脑</h4></div>
          <ul className="devices-list">
            {computers.map((computer) => (
              <li className="devices-row" key={computer.id} aria-label={computer.name}>
                <span className="devices-row-icon" aria-hidden="true"><Laptop size={16} /></span>
                <div className="devices-row-text">
                  <strong>{computer.name || "未命名的电脑"}</strong>
                  <span className="devices-row-meta">{computerMeta(computer)}</span>
                  {rowErrors[computer.id] && <span className="devices-row-error" role="alert">{rowErrors[computer.id]}</span>}
                </div>
                {computer.state === "open" && <span className="devices-live" role="img" aria-label="已打开" title="已打开" />}
                <button type="button" className="compact" disabled={busy !== ""} onClick={() => open(computer)}>
                  {busy === computer.id ? <LoaderCircle size={14} className="spin" aria-hidden="true" /> : null}打开
                </button>
                <button type="button" className="quiet-button" disabled={busy !== ""} aria-label={"移除 " + (computer.name || "这台电脑")} onClick={() => forget(computer)}>移除</button>
              </li>
            ))}
          </ul>
        </section>
      )}
      {!remote && (
        <section className="remote-relay-settings" aria-label="中继设置">
          <div className="remote-relay-row">
            <div>
              <h4>中继服务</h4>
              <p className="field-help">通过中继连接手机与电脑，可使用官方或自建服务。</p>
            </div>
            <div className="remote-relay-options" role="group" aria-label="中继服务">
              <button type="button" aria-pressed={!custom} onClick={() => { setCustom(false); setToken(""); onRelayChange(""); }}>Cage 官方中继</button>
              <button type="button" aria-pressed={custom} onClick={() => setCustom(true)}>自建中继</button>
            </div>
          </div>
          {custom && (
            <>
              <label>
                自建中继地址
                <input value={relay} placeholder="https://relay.example.com" onChange={(event) => { onRelayChange(event.target.value); setToken(""); }} />
              </label>
              <label>
                管理令牌（可选）
                <input type="password" autoComplete="off" value={token} onChange={(event) => setToken(event.target.value)} />
              </label>
            </>
          )}
          {unsavedRelay && <small>保存设置后，可使用新中继添加设备。</small>}
        </section>
      )}
      </fieldset>
      {dialog === "add" && <AddDeviceDialog savedRelay={savedRelay} customRelay={custom ? savedRelay : ""} admin={custom ? token : ""} onClose={closeDialog} />}
      {dialog === "join" && <JoinDialog onClose={closeDialog} onJoined={() => void refresh()} />}
    </section>
  );
}
