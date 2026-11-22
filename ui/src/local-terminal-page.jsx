import React from "react";
import LocalTerminalSettings from "./local-terminal-settings.jsx";
import LocalEntryPage from "./local-entry.jsx";

// Settings → 本机终端: this computer's own terminals, in two parts.
// "在本机终端中打开" belongs to the desktop app (its client routes);
// "从你自己的终端连进来" to a window on the environment's own computer (the
// local SSH entry). Either may be absent. Loaded lazily with the section,
// with both parts' code.
export default function LocalTerminalPage({ opener, entry, onDirty, onConfirm, environmentReady }) {
  return (
    <div className="local-terminal-page">
      <div className="section-title">
        <h3>本机终端</h3>
        <p>在这台电脑上用你自己的终端使用环境中的终端：在本机终端中打开，或从任何终端连进来。终端始终在环境中运行。</p>
      </div>
      {opener && <LocalTerminalSettings onDirty={onDirty} />}
      {entry && <LocalEntryPage onConfirm={onConfirm} environmentReady={environmentReady} />}
    </div>
  );
}
