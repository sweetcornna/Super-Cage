import React, { useEffect, useMemo, useState } from "react";
import { Check, LoaderCircle } from "lucide-react";
import { api } from "./api";
import {
  APPS_PATH, BUILT_IN, SETTINGS_PATH, SWITCHES, chooserOptions, missingChoice, normalizeSettings, settingsChanged,
} from "./local-terminal-state.mjs";
import "./local-terminal-settings.css";

// Settings → 本机终端应用 (this computer). Loaded lazily: detecting the
// applications happens only while this section is open.
export default function LocalTerminalSettings({ onSaved, onDirty }) {
  const [saved, setSaved] = useState(null);
  const [draft, setDraft] = useState(null);
  const [apps, setApps] = useState(null);
  const [supported, setSupported] = useState(true);
  const [warning, setWarning] = useState("");
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [done, setDone] = useState(false);

  useEffect(() => {
    const controller = new AbortController();
    const options = { signal: controller.signal };
    Promise.all([api(SETTINGS_PATH, undefined, undefined, {}, options), api(APPS_PATH, undefined, undefined, {}, options)])
      .then(([state, found]) => {
        const settings = normalizeSettings(state?.settings);
        setSaved(settings);
        setDraft(settings);
        setSupported(state?.supported !== false);
        setWarning(state?.warning || "");
        setApps(Array.isArray(found?.apps) ? found.apps : []);
      })
      .catch(reason => { if (!controller.signal.aborted) setError(reason.message); });
    return () => controller.abort();
  }, []);

  // Where no application can be used, the built-in terminal is what opens.
  const shown = supported ? draft?.app : BUILT_IN;
  const options = useMemo(() => chooserOptions(apps, shown), [apps, shown]);
  const gone = supported ? missingChoice(saved, apps) : "";
  const dirty = Boolean(saved && draft && settingsChanged(saved, draft));
  const loading = !error && (!saved || !apps);
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
      onSaved?.(settings);
    } catch (reason) {
      setError(reason.message);
    } finally {
      setSaving(false);
    }
  };

  return <section className="local-terminal-settings" aria-label="本机终端应用" aria-busy={loading}>
    <div className="section-title">
      <h3>本机终端应用</h3>
      <p>选择在这台电脑上用哪个应用打开 Super Cage 的终端。终端仍在环境中运行，关掉窗口也不会结束它。</p>
    </div>
    {loading && <p className="local-terminal-loading" role="status"><LoaderCircle size={15} className="spin" aria-hidden="true" />正在查找这台电脑上的终端应用…</p>}
    {!loading && !supported && <p className="field-help">这台电脑的系统暂不支持本机终端应用，终端会在 Super Cage 的标签页中打开。</p>}
    {!loading && gone && draft?.app === saved?.app && <p className="local-terminal-gone" role="status">之前选择的 {gone} 在这台电脑上找不到了。请另选一个应用，或使用内置终端。</p>}
    {saved && apps && <>
      <div className="choice-list local-terminal-apps" role="radiogroup" aria-label="本机终端应用">
        {options.map(option => <label key={option.id || "built-in"}
          className={"choice local-terminal-app" + (option.checked ? " checked" : "") + (option.selectable ? "" : " unavailable") + (option.group === "built-in" ? " built-in" : "")}>
          <input type="radio" name="local-terminal-app" value={option.id} checked={option.checked} disabled={!option.selectable || saving}
            onChange={() => change("app", option.id)} />
          <span className="local-terminal-app-text">
            <strong>{option.name}</strong>
            {option.note && <small>{option.note}</small>}
          </span>
          {option.chip && <span className="local-terminal-chip">{option.chip}</span>}
        </label>)}
      </div>
      <h4 className="local-terminal-subtitle">在本机终端中</h4>
      {SWITCHES.map(item => <label className="toggle-row" key={item.key}>
        <span>
          <strong>{item.title}</strong>
          <small>{item.help}</small>
        </span>
        <input type="checkbox" role="switch" checked={Boolean(draft?.[item.key])} disabled={saving}
          onChange={event => change(item.key, event.target.checked)} />
      </label>)}
      <p className="footnote">这些选择只保存在这台电脑上，不会同步到环境或其他设备。已经打开的本机终端窗口在下次打开时使用新的选择。</p>
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
