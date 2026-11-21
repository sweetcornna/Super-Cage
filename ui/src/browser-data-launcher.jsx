import React, { Suspense, lazy, useEffect, useRef, useState } from "react";

// Main-bundle wiring for browser data management; everything else loads with
// browser-data.jsx on first use. Views: clear, site, popover, reset, and
// privacy (the settings section, optionally at "sites", "history" or "kept").
const BrowserDataApp = lazy(() => import("./browser-data.jsx"));
export const BrowserPrivacy = lazy(() => import("./browser-data.jsx").then(module => ({ default: module.BrowserPrivacySection })));
let pending = "", sections = 0;
export const takePendingPrivacyView = () => { const view = pending; pending = ""; return view; };
// A mounted privacy section follows the event itself; a new one reads pending.
export const privacySectionMounted = () => { sections++; return () => { sections--; }; };
export const openBrowserData = detail => document.dispatchEvent(new CustomEvent("cage-browser-data", { detail }));

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
  return request && <Suspense fallback={null}><BrowserDataApp key={request.key} {...props} request={request} onClose={() => setRequest(null)} /></Suspense>;
}
