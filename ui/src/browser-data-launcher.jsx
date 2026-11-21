import React, { Component, Suspense, lazy, useEffect, useRef, useState } from "react";
import { reportDiagnostic } from "./diagnostics.mjs";

// Main-bundle wiring for browser data management; everything else loads with
// browser-data.jsx on first use. Views: clear, site, popover, reset, and
// privacy (the settings section, optionally at "sites", "history" or "kept"),
// which the settings page shows (browser-settings.jsx). The lazy chunks reach
// this module through document events only, so it stays in the main chunk:
// "cage-browser-data" opens a view, and "cage-browser-data-take" hands a
// privacy section the view asked for before it was mounted.
const load = () => lazy(() => import("./browser-data.jsx"));
let BrowserDataApp = load(), pending = { section: "", at: 0 };
export const openBrowserData = detail => document.dispatchEvent(new CustomEvent("cage-browser-data", { detail }));

// A chunk that fails to load (an update replaced it, the network dropped) or a
// render error stays inside browser data instead of reaching the app's error
// screen: it is reported, the caller falls back to what older clients show,
// and the next use loads the chunk again.
class Guard extends Component {
  state = { failed: false };
  static getDerivedStateFromError() { return { failed: true }; }
  componentDidCatch(error) { reportDiagnostic("browser-data", error); this.props.onFail(); }
  render() { return this.state.failed ? null : this.props.children; }
}

export function BrowserDataHost(props) {
  const [request, setRequest] = useState(null);
  const latest = useRef(props);
  latest.current = props;
  useEffect(() => {
    const open = ({ detail = {} }) => {
      if (detail.view !== "privacy") return setRequest({ ...detail, key: Date.now() });
      // A mounted privacy section follows the event; one mounted next (the
      // settings page opening, or remounting as its tab comes to the front)
      // takes the section within a moment.
      pending = { section: detail.section || "", at: Date.now() };
      latest.current.onSettings?.("privacy");
    };
    const take = ({ detail }) => { detail.view = Date.now() - pending.at < 3000 ? pending.section : ""; pending = { section: "", at: 0 }; };
    document.addEventListener("cage-browser-data", open);
    document.addEventListener("cage-browser-data-take", take);
    return () => { document.removeEventListener("cage-browser-data", open); document.removeEventListener("cage-browser-data-take", take); };
  }, []);
  const fail = () => {
    BrowserDataApp = load();
    setRequest(null);
    if (request.fallback) request.fallback();
    else if (request.view === "clear") props.onInternal?.("clear");
  };
  return request && <Guard key={request.key} onFail={fail}><Suspense fallback={null}><BrowserDataApp key={request.key} {...props} request={request} onClose={() => setRequest(null)} /></Suspense></Guard>;
}
