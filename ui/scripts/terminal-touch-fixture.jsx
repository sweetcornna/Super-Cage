import React from "react";
import { createRoot } from "react-dom/client";
import { AppearanceProvider } from "../src/appearance";
import { TerminalView } from "../src/views";
import TerminalScreen from "../src/mobile-cli/terminal-screen";
import "../src/desktop-styles.js";
import "../src/mobile-cli/super.css";

// One terminal view for terminal-touch-smoke.mjs: the desktop workbench's or
// the phone's, over the stand-in streams the smoke serves.
const root = createRoot(document.getElementById("terminal"));
window.showTestTerminal = ({ phone = false, id = "touch-test", visible = true } = {}) =>
  root.render(phone
    ? <TerminalScreen key={id} id={id} title="终端" kind="shell" visible={visible} onBack={() => {}} onDenied={() => {}} />
    : <AppearanceProvider>
      <TerminalView key={id} session={{ id }} visible={visible} onStatus={() => {}} />
    </AppearanceProvider>);
