// The workbench sidebar on a touch tablet held upright (744 to 1100 px): a drawer over the content instead
// of a column beside it, so the panes keep the whole width. It starts closed, a choice made in it closes it,
// and a tap on the dimmed panes closes it without acting on them. Mouse and trackpad windows are untouched.
// The matching styles are in touch.css.
import { useEffect, useState } from "react";
import { readLocal } from "./workspace-state.mjs";

const QUERY = "(pointer: coarse) and (hover: none) and (max-width: 1100px)";
let opened = 0;

export const sidebarDrawer = () => typeof matchMedia === "function" && matchMedia(QUERY).matches;

// collapsed/setCollapsed: the workbench's sidebar state; storageKey: where the docked choice is kept;
// route: values that change when the user opens something (a tab, a page, home).
export function useSidebarDrawer(collapsed, setCollapsed, storageKey, route) {
  const [drawer, setDrawer] = useState(sidebarDrawer);
  useEffect(() => {
    const media = matchMedia(QUERY);
    const change = () => setDrawer(media.matches);
    media.addEventListener("change", change);
    return () => media.removeEventListener("change", change);
  }, []);
  // Turned upright: closed. Turned on its side: the docked choice saved on this device comes back.
  const [mode, setMode] = useState(drawer);
  if (mode !== drawer) {
    setMode(drawer);
    setCollapsed(drawer || readLocal(storageKey, false));
  }
  const key = route.join("\u0000");
  useEffect(() => {
    if (drawer) setCollapsed(true);
  }, [drawer, key, setCollapsed]);
  // While open, the panes behind are inert (no focus, no taps: a tap there reaches the shell around them),
  // and a tap there closes the drawer.
  useEffect(() => {
    const panes = drawer && !collapsed ? document.querySelector(".workbench-main") : null;
    if (!panes) return undefined;
    const token = ++opened;
    panes.inert = true;
    let closing = false;
    const down = (event) => {
      if (!event.target.closest?.(".workbench-shell") || event.target.closest(".workspace-sidebar")) return;
      closing = true;
      setCollapsed(true);
    };
    document.addEventListener("pointerdown", down, true);
    return () => {
      document.removeEventListener("pointerdown", down, true);
      // The tap that closed the drawer ends with a click over the panes: they stay inert until it has
      // landed (on the shell around them), or the tap became a scroll.
      const release = () => {
        clearTimeout(timer);
        document.removeEventListener("click", release, true);
        if (token === opened) panes.inert = false;
      };
      const timer = closing ? setTimeout(release, 600) : 0;
      if (closing) document.addEventListener("click", release, true);
      else release();
    };
  }, [drawer, collapsed, setCollapsed]);
  return drawer;
}
