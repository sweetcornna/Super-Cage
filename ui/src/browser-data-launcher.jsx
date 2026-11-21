import React, { Component, Suspense, lazy, useEffect, useRef, useState } from "react";
import { reportDiagnostic } from "./diagnostics.mjs";
import "./browser-data-launcher.css";

// Main-bundle wiring for browser data management; everything else loads with
// browser-data.jsx on first use. Views: clear, site, popover, reset, and
// privacy (the settings section, optionally at "sites", "history" or "kept").
const loadApp = () => lazy(() => import("./browser-data.jsx"));
const loadPrivacy = () => lazy(() => import("./browser-data.jsx").then(module => ({ default: module.BrowserPrivacySection })));
let BrowserDataApp = loadApp(), Privacy = loadPrivacy();
let pending = "", sections = 0;
export const takePendingPrivacyView = () => { const view = pending; pending = ""; return view; };
// A mounted privacy section follows the event itself; a new one reads pending.
export const privacySectionMounted = () => { sections++; return () => { sections--; }; };
export const openBrowserData = detail => document.dispatchEvent(new CustomEvent("cage-browser-data", { detail }));

// A chunk that fails to load (an update replaced it, the network dropped) or a
// render error stays inside browser data instead of reaching the app's error
// screen: it is reported, the caller falls back to what older clients show,
// and the next use loads the chunk again.
class Guard extends Component {
  state = { failed: false };
  static getDerivedStateFromError() { return { failed: true }; }
  componentDidCatch(error) { reportDiagnostic("browser-data", error); this.props.onFail(); }
  render() { return this.state.failed ? this.props.fallback || null : this.props.children; }
}

export function BrowserPrivacy(props) {
  return <Guard onFail={() => { Privacy = loadPrivacy(); }} fallback={<div className="browser-settings-links">
    <button onClick={() => props.onInternal("permissions")}>网站权限</button>
    <button onClick={() => props.onInternal("clear")}>清除普通会话浏览数据</button>
  </div>}><Suspense fallback={null}><Privacy {...props} /></Suspense></Guard>;
}

export function BrowserDataHost(props) {
  const [request, setRequest] = useState(null);
  const latest = useRef(props);
  latest.current = props;
  useEffect(() => {
    const open = ({ detail = {} }) => {
      if (detail.view !== "privacy") return setRequest({ ...detail, key: Date.now() });
      pending = sections ? "" : detail.section || "";
      latest.current.onSettings?.("privacy");
    };
    document.addEventListener("cage-browser-data", open);
    return () => document.removeEventListener("cage-browser-data", open);
  }, []);
  const fail = () => {
    BrowserDataApp = loadApp();
    setRequest(null);
    if (request.fallback) request.fallback();
    else if (request.view === "clear") props.onInternal?.("clear");
  };
  return request && <Guard key={request.key} onFail={fail}><Suspense fallback={null}><BrowserDataApp key={request.key} {...props} request={request} onClose={() => setRequest(null)} /></Suspense></Guard>;
}
