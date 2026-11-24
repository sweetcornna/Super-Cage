// Account-login terminals (Claude, Codex and other agents) run outside any
// project: the server gives them no workspace. The sidebar lists terminals
// under their project, and the tab strip shows only browser tabs while the
// authorization page is open, so without these helpers the login terminal
// vanishes the moment its page opens and the code copied there has nowhere
// to go.
export const LOGIN_TERMINAL_KINDS = new Set(["claude-login", "codex-login", "agent-login"]);

// The login terminals no project in the sidebar lists.
export function orphanLoginTerminals(tabs, projects = [], projectGroups = []) {
  const folders = new Set();
  for (const path of projects || []) {
    const group = (projectGroups || []).find((item) => item.workspace === path);
    for (const folder of group?.folders || [path]) folders.add(folder);
  }
  return (tabs || []).filter((tab) =>
    tab?.kind === "terminal" &&
    LOGIN_TERMINAL_KINDS.has(tab.terminalKind) &&
    !folders.has(tab.workspace));
}

// The open login terminal an authorization page was opened from, or null
// once that terminal has ended or been closed.
export function loginReturnTerminal(returns, browserTabId, tabs) {
  const id = browserTabId ? returns?.[browserTabId] : "";
  if (!id) return null;
  return (tabs || []).find((tab) => tab.id === id && tab.kind === "terminal" && !tab.closed) || null;
}
