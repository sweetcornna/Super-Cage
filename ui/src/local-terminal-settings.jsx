import React, { useEffect, useMemo, useRef, useState } from "react";
import { Check, LoaderCircle } from "lucide-react";
import { api } from "./api";
import { getClientContext } from "./client-context.mjs";
import {
  BUILT_IN, SETTINGS_PATH, SWITCHES, appsRequest, automationNote, chooserOptions, goneText, normalizeSettings, settingsChanged,
} from "./local-terminal-state.mjs";
import "./local-terminal-settings.css";

// Settings → 本机终端, its first part: which application "在本机终端中打开"
// uses on this computer, and the two switches its windows (and, for the
// clipboard, ssh sessions) follow. Loaded lazily with the section:
// detecting the applications happens only while it is open.
// herdrState is Herdr's link to this environment (the entry's part reads
// it): its row says 已连接. onApps hears the applications found.
export default function LocalTerminalSettings({ onSaved, onDirty, herdrState = "", onApps }) {
  const [saved, setSaved] = useState(null);
  const [draft, setDraft] = useState(null);
  const [apps, setApps] = useState(null);
  const [supported, setSupported] = useState(true);
  const [warning, setWarning] = useState("");
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [done, setDone] = useState(false);
  const heard = useRef(onApps);
  heard.current = onApps;

  useEffect(() => {
    const controller = new AbortController();
    const options = { signal: controller.signal };
    const listing = appsRequest(getClientContext());
    Promise.all([api(SETTINGS_PATH, undefined, undefined, {}, options), api(listing.path, undefined, undefined, listing.headers, options)])
      .then(([state, found]) => {
        const settings = normalizeSettings(state?.settings);
        setSaved(settings);
        setDraft(settings);
        setSupported(state?.supported !== false);
        setWarning(state?.warning || "");
        const list = Array.isArray(found?.apps) ? found.apps : [];
        setApps(list);
        heard.current?.(list);
      })
      .catch(reason => { if (!controller.signal.aborted) setError(reason.message); });
    return () => controller.abort();
  }, []);

  // Where no application can be used, the built-in terminal is what opens.
  const shown = supported ? draft?.app : BUILT_IN;
  const options = useMemo(() => chooserOptions(apps, shown, herdrState), [apps, shown, herdrState]);
  const gone = supported ? goneText(saved, apps) : "";
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
      // Herdr can be chosen only for an environment it added: name it.
      const result = await api(SETTINGS_PATH, draft, "PUT", appsRequest(getClientContext()).headers);
      window.dispatchEvent(new Event("cage-local-terminal-settings"));
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

  return <section className="settings-group local-terminal-part local-terminal-settings" aria-label="用哪个终端应用打开" aria-busy={loading}>
    <h3>用哪个终端应用打开</h3>
    <p className="field-help">在终端标签页或项目上选择“在本机终端中打开”时，用这里选的应用。终端仍在环境中运行，关掉窗口也不会结束它。</p>
    {loading && <p className="local-terminal-loading" role="status"><LoaderCircle size={15} className="spin" aria-hidden="true" />正在查找这台电脑上的终端应用…</p>}
    {!loading && !supported && <p className="field-help">这台电脑的系统暂不支持本机终端应用，终端会在 Super Cage 的标签页中打开。</p>}
    {!loading && gone && draft?.app === saved?.app && <p className="local-terminal-gone" role="status">{gone}</p>}
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
          {option.chip && <span className="local-terminal-chip" data-connected={option.connected || undefined}>{option.chip}</span>}
        </label>)}
      </div>
      {supported && automationNote(apps) && <p className="field-help local-terminal-automation">{automationNote(apps)}</p>}
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
