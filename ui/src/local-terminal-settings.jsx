import React, { useEffect, useState } from "react";
import { Check, LoaderCircle } from "lucide-react";
import { api } from "./api";
import { APP_PATH, SETTINGS_PATH, SWITCHES, normalizeSettings, openerLine, settingsChanged } from "./local-terminal-state.mjs";
import "./local-terminal-settings.css";

// Settings → 本机终端, its first part: the one line that says which
// application "在本机终端中打开" uses on this computer (there is nothing to
// choose), and the two switches its windows (and, for the clipboard, ssh
// sessions) follow. Loaded lazily with the section: the application is looked
// for only while it is open.
export default function LocalTerminalSettings({ onDirty }) {
  const [saved, setSaved] = useState(null);
  const [draft, setDraft] = useState(null);
  // undefined until looked for; null when neither Ghostty nor Terminal is here.
  const [app, setApp] = useState(undefined);
  const [supported, setSupported] = useState(true);
  const [warning, setWarning] = useState("");
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [done, setDone] = useState(false);

  useEffect(() => {
    const controller = new AbortController();
    const options = { signal: controller.signal };
    Promise.all([api(SETTINGS_PATH, undefined, undefined, {}, options), api(APP_PATH, undefined, undefined, {}, options)])
      .then(([state, found]) => {
        const settings = normalizeSettings(state?.settings);
        setSaved(settings);
        setDraft(settings);
        setSupported(state?.supported !== false);
        setWarning(state?.warning || "");
        setApp(found?.app || null);
      })
      .catch(reason => { if (!controller.signal.aborted) setError(reason.message); });
    return () => controller.abort();
  }, []);

  const dirty = Boolean(saved && draft && settingsChanged(saved, draft));
  const loading = !error && (!saved || app === undefined);
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

  return <section className="settings-group local-terminal-part local-terminal-settings" aria-label="在本机终端中打开" aria-busy={loading}>
    <h3>在本机终端中打开</h3>
    {loading && <p className="local-terminal-loading" role="status"><LoaderCircle size={15} className="spin" aria-hidden="true" />正在查看这台电脑上的终端…</p>}
    {!loading && saved && <p className="field-help local-terminal-opener">{openerLine(app, supported)}</p>}
    {saved && app !== undefined && <>
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
