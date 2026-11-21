// What the native shells do for the phone UI, done by the page when it runs in a phone browser at "/"
// (the workbench serves it there to phones, see ui/src/entry-layout.mjs). Everything here is inert in
// the native apps, which load the same bundle with "CageMobile/1" in the user agent.
import { useEffect, useRef, useState } from "react";

export const nativeShell = String(globalThis.navigator?.userAgent || "").includes("CageMobile/1");
// Only the phone document (mobile.html, marked by its cage-entry meta) served at "/" to a browser.
export const webPhone = !nativeShell && globalThis.location?.pathname === "/" &&
  typeof globalThis.document?.querySelector === "function" && document.querySelector('meta[name="cage-entry"][content="phone"]') !== null;
// Inside the portal's frame (tenant workspaces): the portal owns the window, the keyboard and the notch.
export const portalFrame = webPhone && window.top !== window.self;
// The swipe from the left edge that opens the side panel. In a phone browser that edge belongs to the
// browser's own Back; only a home-screen app on iOS (navigator.standalone) has no Back gesture of its own.
export const edgeSwipe = !webPhone || globalThis.navigator?.standalone === true;

const ROUTE = "cage-phone-route";

// The phone side of ui/src/entry-layout.mjs (same cookie, flag and size test; see the note at its end).
const layoutCookie = (value, maxAge) => {
  document.cookie = `cage-layout=${value}; Path=/; SameSite=Lax${maxAge === undefined ? "" : "; Max-Age=" + maxAge}${location.protocol === "https:" ? "; Secure" : ""}`;
};
// The hand-over arrived: forget an automatic choice the device no longer warrants, so the next load
// decides afresh (no bounce now).
function settlePhoneEntry() {
  try { sessionStorage.removeItem("cage-entry-guard"); } catch { /* storage may be unavailable */ }
  const auto = document.cookie.split(";").some((part) => part.trim() === "cage-layout=phone-auto");
  const desktopSite = innerWidth >= 980 || innerWidth > Math.max(screen.width, screen.height) + 1;
  const phoneSized = matchMedia("(pointer: coarse)").matches && !matchMedia("(hover: hover)").matches &&
    !desktopSite && (innerWidth < 740 || innerHeight < 500);
  if (auto && !phoneSized) layoutCookie("", 0);
}
// A native desktop window (its shell injects the bridge before any page script) never runs the phone UI.
// Its web view shares cookies across every localhost port, so a choice made on another port could ask
// for it; this window takes the desktop workbench for its session instead, before drawing (once per tab,
// so a server that ignored the cookie could not make it loop).
const nativeDesktopWindow = webPhone && Boolean(window.cageNativeBridge || window.webkit?.messageHandlers?.cage);
if (nativeDesktopWindow) {
  try {
    if (!sessionStorage.getItem("cage-native-desktop")) {
      sessionStorage.setItem("cage-native-desktop", "1");
      document.documentElement.style.visibility = "hidden";
      layoutCookie("desktop");
      location.reload();
    }
  } catch { /* storage may be unavailable: stay, without the browser parts */ }
}
// 电脑版: remembered on this device for this origin; the server then serves the desktop workbench.
export function chooseDesktop() {
  layoutCookie("desktop", 31536000);
  try { sessionStorage.removeItem("cage-entry-guard"); } catch { /* storage may be unavailable */ }
  location.reload();
}
const load = Math.random().toString(36).slice(2, 8);
let overlays = 0;
let portalOrigin = "";
let tenant = false;
const tenantListeners = new Set();

if (webPhone && !nativeDesktopWindow) {
  document.documentElement.classList.add("web-phone");
  if (portalFrame) document.documentElement.classList.add("portal-frame");
  settlePhoneEntry();
  // Return to the last screen of this tab after the portal re-enters the workspace (sign-in expiry,
  // a reconnect). Overlay entries of an earlier page life are dropped; a remote display is not resumed.
  let state = history.state;
  if (!state?.cageCLI) {
    try {
      const saved = JSON.parse(sessionStorage.getItem(ROUTE) || "null");
      if (saved && typeof saved === "object") state = { ...state, cageCLI: saved.displayID ? { ...saved, tool: "code", displayID: "" } : saved };
    } catch { /* a bad entry is ignored */ }
  }
  if (state !== history.state || state?.cageOverlay) history.replaceState({ ...state, cageOverlay: undefined }, "");
  trackViewport();
  void readContext();
}

// The visual viewport is the visible area above the keyboard. Its height is published by main.jsx
// (--mobile-height); its offset is published here, so the fixed app sits exactly on it on iOS, where
// the keyboard pans the page instead of resizing it.
function trackViewport() {
  const viewport = window.visualViewport;
  if (!viewport) return;
  const root = document.documentElement.style;
  const update = () => {
    if (Math.abs(viewport.scale - 1) > 0.01) return;
    root.setProperty("--mobile-top", Math.max(0, viewport.offsetTop) + "px");
    if (viewport.offsetTop > 0 && document.scrollingElement?.scrollTop) document.scrollingElement.scrollTop = 0;
  };
  viewport.addEventListener("resize", update);
  viewport.addEventListener("scroll", update);
  update();
  // The keyboard took part of the screen (here, or the portal shrank this frame to what is left): keep the
  // field being typed in on screen, as a page that scrolls would.
  let pending = 0;
  const reveal = () => {
    cancelAnimationFrame(pending);
    pending = requestAnimationFrame(() => {
      const field = document.activeElement;
      if (field?.matches?.("input, textarea, [contenteditable=true]")) field.scrollIntoView({ block: "nearest" });
    });
  };
  viewport.addEventListener("resize", reveal);
  window.addEventListener("resize", reveal);
}

async function readContext() {
  try {
    const response = await fetch("/api/client/context", { credentials: "same-origin", cache: "no-store" });
    const context = response.ok ? await response.json() : null;
    if (!context?.tenant_workspace) return;
    tenant = true;
    // The portal decides on its own whether its bar steps back for this workspace.
    if (portalFrame && /^https?:\/\/[^/]+$/.test(context.portal_origin || "")) portalOrigin = context.portal_origin;
    for (const listener of tenantListeners) listener(true);
  } catch { /* the phone UI works without it */ }
}

// Messages to the portal that frames this workspace; ignored elsewhere.
export function toPortal(message) {
  if (!portalOrigin) return false;
  window.parent.postMessage(message, portalOrigin);
  return true;
}

// "返回门户": the portal goes back to the VM list past this workspace's history entries, which it counts
// itself, leaving nothing behind to press Back through.
export function backToPortal() {
  toPortal({ type: "cage:portal-home" });
}

// Opens an http(s) link outside the app: the portal's sandbox has no pop-ups, so the portal offers it to
// the person with the address shown.
export function openExternal(url) {
  if (portalFrame && toPortal({ type: "cage:open-link", url })) return;
  window.open(url, "_blank", "noopener");
}

// Sign-in expired (HTTP 401): the portal checks its own session and re-enters the workspace, which
// then returns to the remembered screen.
// A request was refused for an expired workspace sign-in (transport.mjs): the portal signs in again.
if (webPhone) window.addEventListener("cage-sign-in-expired", () => toPortal({ type: "cage:workspace-unauthenticated" }));

// A tenant workspace (a VM of a Super Cage server): no paired computer for the remote browser.
export function useWebTenant() {
  const [value, setValue] = useState(tenant);
  useEffect(() => {
    if (!webPhone) return undefined;
    tenantListeners.add(setValue);
    setValue(tenant);
    return () => tenantListeners.delete(setValue);
  }, []);
  return value;
}

// Two panes (the side panel docked beside the page) when the window is wide enough.
export function useDocked() {
  const query = "(min-width: 720px)";
  const [docked, setDocked] = useState(() => webPhone && matchMedia(query).matches);
  useEffect(() => {
    if (!webPhone) return undefined;
    const list = matchMedia(query);
    const change = () => setDocked(list.matches);
    list.addEventListener("change", change);
    return () => list.removeEventListener("change", change);
  }, []);
  return docked;
}

// main.jsx's route writer: an overlay's history entry becomes the new screen instead of staying under
// it, and the route is remembered for this tab.
export function writeRoute(route, push) {
  const overlay = history.state?.cageOverlay;
  const next = { ...history.state, cageCLI: route, cageOverlay: undefined };
  history[push && !overlay ? "pushState" : "replaceState"](next, "");
  if (webPhone) {
    try { sessionStorage.setItem(ROUTE, JSON.stringify(route)); } catch { /* storage may be unavailable */ }
  }
}

// The system Back button and swipe-back close an open drawer, sheet or dialog before leaving the screen.
// Each overlay owns one history entry while it is open; closing it from the UI removes that entry.
const openOverlays = new Set();
let pendingBacks = 0;
export function useBackDismiss(open, close) {
  const closeRef = useRef(close);
  closeRef.current = close;
  useEffect(() => {
    if (!open || !webPhone || !closeRef.current) return undefined;
    const id = load + ":" + ++overlays;
    openOverlays.add(id);
    history.pushState({ ...history.state, cageOverlay: id }, "");
    const pop = () => { if (history.state?.cageOverlay !== id) closeRef.current?.(); };
    window.addEventListener("popstate", pop);
    return () => {
      window.removeEventListener("popstate", pop);
      openOverlays.delete(id);
      if (history.state?.cageOverlay === id) { pendingBacks++; history.back(); }
    };
  }, [open]);
}

// main.jsx's popstate handler asks this first: a Back that only closed an overlay (or the entry an
// overlay removed when it closed) is not a change of screen.
export function overlayPop() {
  if (pendingBacks) { pendingBacks--; return true; }
  return openOverlays.size > 0;
}
