import {
  announceDesktopInFrame,
  guardDesktopEntry,
  mountPhoneSwitch,
} from "./entry-layout.mjs";
import "./entry-layout.css";
import "./touch.css";
import React, { lazy, Suspense } from "react";
import { createRoot } from "react-dom/client";
import { installDiagnostics } from "./diagnostics.mjs";
import { DiagnosticBoundary } from "./diagnostics.jsx";
import { ClientRoot } from "./remote-hosts";
import { native } from "./api";
import "./desktop-styles.js";
import { IconHints } from "./icon-hints.jsx";

// A phone-sized touch device without a layout choice goes to the phone UI instead of loading the
// workbench (after a launch ticket in the hash has been redeemed, so the server can serve it).
const leaving = !location.hash && guardDesktopEntry();
// In the portal's frame the desktop page asks for the portal's bar back once it knows it stays.
if (location.hash)
  window.addEventListener(
    "cage-session-ready",
    () => {
      if (!guardDesktopEntry()) announceDesktopInFrame();
    },
    { once: true },
  );
const Workbench = lazy(() =>
  leaving ? new Promise(() => {}) : import("./main.jsx"),
);

installDiagnostics();
// Keep the native menu usable while the workbench is loading or disconnected.
window.cageNativeAction = (command) => {
  if (window.cageWorkbenchAction) return window.cageWorkbenchAction(command);
  if (command === "quit") return native("quitNow");
};
createRoot(document.getElementById("root")).render(
  <DiagnosticBoundary>
    <IconHints focusVisibleOnly={matchMedia("(hover: none)").matches} />
    <ClientRoot>
      <Suspense
        fallback={
          <div className="client-loading" role="status">
            正在打开工作台…
          </div>
        }
      >
        <Workbench />
      </Suspense>
    </ClientRoot>
  </DiagnosticBoundary>,
);

mountPhoneSwitch();
if (!leaving && !location.hash) announceDesktopInFrame();

window.addEventListener("hashchange", () => {
  if (location.hash) location.reload();
});
