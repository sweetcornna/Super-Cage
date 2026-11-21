// Which of the two web documents this device uses at "/": the desktop workbench or the phone UI.
// The server picks one first (internal/webentry: the cookie below, else the browser's claim to be a
// phone). Pages only refine that with what the server cannot see, at load and never on resize, so a
// rotating phone or an unfolding foldable keeps its app and its place.
//   cage-layout=phone | desktop  the visible switch in either UI (one year, this origin)
//   cage-layout=phone-auto       the desktop page found a touch-only device too small for it (a session
//                                cookie; the phone page drops it again on a device that is not)
const NAME = "cage-layout";
const GUARD = "cage-entry-guard";

export function layoutChoice() {
  for (const part of document.cookie.split(";")) {
    const [key, value] = part.trim().split("=");
    if (key === NAME) return value || "";
  }
  return "";
}

function writeCookie(value, maxAge) {
  const secure = location.protocol === "https:" ? "; Secure" : "";
  document.cookie = `${NAME}=${value}; Path=/; SameSite=Lax${maxAge === undefined ? "" : "; Max-Age=" + maxAge}${secure}`;
}

// A phone-sized touch device: no hover, a coarse pointer, and a window narrower than a tablet or
// shorter than one (a phone on its side). A desktop browser resized narrow has a fine pointer and
// is never moved; a phone asking for the desktop site has a 980 px wide window and is left alone.
export function phoneSized() {
  return matchMedia("(pointer: coarse)").matches && !matchMedia("(hover: hover)").matches &&
    (innerWidth < 740 || innerHeight < 500);
}

// The visible switch: remembered on this device for this origin, then the server serves the choice.
export function chooseLayout(value) {
  writeCookie(value, 31536000);
  try { sessionStorage.removeItem(GUARD); } catch { /* storage may be unavailable */ }
  location.reload();
}

// Desktop page: hand a phone-sized device without a choice over to the phone UI, once per tab, so a
// server that is not ready for it (no session yet, a locked account) cannot make it loop. Returns true
// when the page is leaving.
export function guardDesktopEntry() {
  if (layoutChoice() || !phoneSized()) return false;
  try {
    if (sessionStorage.getItem(GUARD)) return false;
    sessionStorage.setItem(GUARD, "1");
  } catch { return false; }
  writeCookie("phone-auto");
  location.reload();
  return true;
}

// Desktop page on a phone-sized touch device (a phone that chose the desktop, or a server that was not
// ready for the phone UI): a small switch back. Hidden by CSS everywhere else.
export function mountPhoneSwitch() {
  if (!matchMedia("(pointer: coarse)").matches) return;
  const button = document.createElement("button");
  button.type = "button";
  button.className = "layout-switch";
  button.textContent = "手机版";
  button.setAttribute("aria-label", "切换到手机版");
  button.addEventListener("click", () => chooseLayout("phone"));
  document.body.append(button);
}

// Desktop page inside the portal's frame on a phone: tell the portal, which hides its bars for the phone UI,
// that this workspace now shows the desktop workbench (the message carries nothing else).
export function announceDesktopInFrame() {
  if (window.top === window.self || !phoneSized()) return;
  try { window.parent.postMessage({ type: "cage:desktop-ui" }, "*"); } catch { /* no portal */ }
}

// The phone page's side of this (dropping phone-auto on a device that no longer warrants it, and its 电脑版
// switch) lives in ui/src/mobile-cli/web-shell.mjs: a module shared by both pages becomes a chunk of its
// own, one more request for each page, and the phone page has no use for the rest of this one.
