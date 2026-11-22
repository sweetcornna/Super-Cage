import React, { useCallback, useState } from "react";
import LocalTerminalSettings from "./local-terminal-settings.jsx";
import LocalEntryPage from "./local-entry.jsx";
import "./local-terminal-page.css";

// Settings → 本机终端: this computer's own terminals, in two parts.
// "用哪个终端应用打开" belongs to the desktop app (its client routes);
// "从你自己的终端或 Herdr 连进来" to a window on the environment's own
// computer (the local SSH entry). Either may be absent. Loaded lazily with
// the section, with both parts' code. With the entry on, its part comes
// first. Herdr has one place: its set-up command in the entry's part until
// it added the environment, then its row among the applications.
export default function LocalTerminalPage({ apps, entry, onDirty, onConfirm, environmentReady }) {
  const [entryView, setEntryView] = useState(null);
  const [herdrListed, setHerdrListed] = useState(false);
  const listed = useCallback((found) => setHerdrListed(found.some((app) => app.installed && app.machine)), []);
  const appsPart = apps && <LocalTerminalSettings key="apps" onDirty={onDirty} herdrState={entryView?.herdr?.state || ""} onApps={listed} />;
  const entryPart = entry && (
    <LocalEntryPage key="entry" onConfirm={onConfirm} environmentReady={environmentReady} herdrElsewhere={Boolean(apps) && herdrListed} onView={setEntryView} />
  );
  return (
    <div className="local-terminal-page">
      <div className="section-title">
        <h3>本机终端</h3>
        <p>在这台电脑上用你自己的终端使用环境中的终端：用选好的终端应用打开，或从任何终端连进来。终端始终在环境中运行。</p>
      </div>
      {entryView?.enabled ? [entryPart, appsPart] : [appsPart, entryPart]}
    </div>
  );
}
