import React, { useEffect, useState } from "react";
import { Check, LoaderCircle } from "lucide-react";
import { api } from "./api";
import { APP_PATH, INSTALL_HERDR_PATH, SETTINGS_PATH, SWITCHES, normalizeSettings, openerLine, settingsChanged } from "./local-terminal-state.mjs";
import "./local-terminal-settings.css";

// Settings → 本机终端, its first part: the one line that says which
// application "在本机终端中打开" uses on this computer (there is nothing to
// choose), optional Herdr desktop installation, and the two switches its
// windows (and, for the clipboard, ssh sessions) follow. Loaded lazily with
// the section: the application is looked for only while it is open.
export default function LocalTerminalSettings({ onDirty }) {
  const [saved, setSaved] = useState(null);
  const [draft, setDraft] = useState(null);
  // undefined until looked for; null when none of Herdr, Ghostty or Terminal is here.
  const [app, setApp] = useState(undefined);
  const [supported, setSupported] = useState(true);
  const [warning, setWarning] = useState("");
  const [settingsError, setSettingsError] = useState("");
  const [appError, setAppError] = useState("");
  const [readAttempt, setReadAttempt] = useState(0);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [done, setDone] = useState(false);
  const [installingHerdr, setInstallingHerdr] = useState(false);
  const [herdrStatus, setHerdrStatus] = useState("");

  useEffect(() => {
    const controller = new AbortController();
    const options = { signal: controller.signal };
    setSettingsError("");
    setAppError("");
    if (!saved) api(SETTINGS_PATH, undefined, undefined, {}, options)
      .then((state) => {
        if (controller.signal.aborted) return;
        const settings = normalizeSettings(state?.settings);
        setSaved(settings);
        setDraft(settings);
        setSupported(state?.supported !== false);
        setWarning(state?.warning || "");
      })
      .catch(reason => { if (!controller.signal.aborted) setSettingsError(reason.message); });
    if (app === undefined) api(APP_PATH, undefined, undefined, {}, options)
      .then((found) => { if (!controller.signal.aborted) setApp(found?.app || null); })
      .catch(reason => { if (!controller.signal.aborted) setAppError(reason.message); });
    return () => controller.abort();
  }, [readAttempt]);

  const dirty = Boolean(saved && draft && settingsChanged(saved, draft));
  const loading = (!saved && !settingsError) || (app === undefined && !appError);
  useEffect(() => { onDirty?.(dirty); }, [dirty, onDirty]);
  useEffect(() => () => onDirty?.(false), [onDirty]);
  const change = (key, value) => { setDraft(previous => ({ ...previous, [key]: value })); setDone(false); };

  const save = async () => {
    if (!dirty || saving) return;
    setSaving(true);
    setError("");
    try {
      const result = await api(SETTINGS_PATH, draft, "PUT");
      const settings = normalizeSettings(result?.settings);
      setSaved(settings);
      setDraft(settings);
      setDone(true);
    } catch (reason) {
      setError(reason.message);
    } finally {
      setSaving(false);
    }
  };

  const installHerdr = async () => {
    if (installingHerdr) return;
    setInstallingHerdr(true);
    setError("");
    setHerdrStatus("");
    try {
      const result = await api(INSTALL_HERDR_PATH, {}, "POST");
      if (result && "app" in result) setApp(result.app || null);
      setHerdrStatus(result?.app?.id === "herdr"
        ? "Herdr 桌面端已安装，所有本机终端已自动改用 Herdr 打开。"
        : "Herdr 安装完成；如未立即生效，重新打开本页即可切换。");
    } catch (reason) {
      setError(reason.message);
    } finally {
      setInstallingHerdr(false);
    }
  };

  return <section className="settings-group local-terminal-part local-terminal-settings" aria-label="在本机终端中打开" aria-busy={loading}>
    <h3>在本机终端中打开</h3>
    {loading && <p className="local-terminal-loading" role="status"><LoaderCircle size={15} className="spin" aria-hidden="true" />正在查看这台电脑上的终端…</p>}
    {saved && app !== undefined && <p className="field-help local-terminal-opener">{openerLine(app, supported)}</p>}
    {saved && supported && app !== undefined && app?.id !== "herdr" && <div className="local-terminal-herdr-install" style={{ marginTop: 10 }}>
      <button type="button" disabled={installingHerdr || saving} onClick={installHerdr} style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
        {installingHerdr && <LoaderCircle size={15} className="spin" aria-hidden="true" />}
        {installingHerdr ? "正在安装 Herdr 桌面端…" : "安装 Herdr 桌面端（安装后自动改用 Herdr）"}
      </button>
    </div>}
    {herdrStatus && <p className="field-help" role="status">{herdrStatus}</p>}
    {saved && <>
      <h4 className="local-terminal-subtitle">图片与剪贴板</h4>
      {SWITCHES.map(item => <label className="toggle-row" key={item.key}>
        <span>
          <strong>{item.title}</strong>
          <small>{item.help}</small>
        </span>
        <input type="checkbox" role="switch" checked={Boolean(draft?.[item.key])} disabled={saving}
          onChange={event => change(item.key, event.target.checked)} />
      </label>)}
      <p className="footnote">这些选择只保存在这台电脑上，不会同步到环境或其他设备。已经打开的窗口和 ssh 连接在下次打开时使用新的选择。</p>
    </>}
    {(settingsError || appError) && <div>
      {settingsError && <p className="error-message" role="alert">{settingsError}</p>}
      {appError && <p className="error-message" role="alert">{appError}</p>}
      <button type="button" onClick={() => setReadAttempt(value => value + 1)}>重新读取本机终端状态</button>
    </div>}
    {warning && <p className="field-help">{warning}</p>}
    {error && <p className="error-message" role="alert">{error}</p>}
    {(dirty || saving) && <div className="local-terminal-savebar">
      <span className="save-note">有尚未保存的修改</span>
      <button type="button" disabled={saving} onClick={() => { setDraft(saved); setError(""); }}>放弃修改</button>
      <button type="button" className="primary" disabled={saving} onClick={save}>
        {saving ? <LoaderCircle size={15} className="spin" aria-hidden="true" /> : <Check size={15} aria-hidden="true" />}
        保存
      </button>
    </div>}
    {done && !dirty && <p className="local-terminal-saved" role="status">已保存</p>}
  </section>;
}
