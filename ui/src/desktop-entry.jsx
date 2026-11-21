import {
  announceDesktopInFrame,
  guardDesktopEntry,
  mountPhoneSwitch,
} from "./entry-layout.mjs";
import "./entry-layout.css";
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
if (location.hash)
  window.addEventListener("cage-session-ready", () => guardDesktopEntry(), {
    once: true,
  });
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
    <IconHints />
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
if (!leaving) announceDesktopInFrame();

window.addEventListener("hashchange", () => {
  if (location.hash) location.reload();
});
