// A new terminal's tab appears as soon as it is asked for (TE-2). The
// placeholder carries the request (workspace and kind) and, when the request
// fails, the server's error; the server's record then replaces it in place.
// main.jsx owns the tab list and calls these.

const pendingPrefix = "terminal:pending-";
const pendingTitles = { claude: "Claude", codex: "Codex" };
let sequence = 0;

export const isPendingTerminal = (id) => typeof id === "string" && id.startsWith(pendingPrefix);

// Account logins start from settings, which show their own progress and errors.
export const loginTerminalKind = (kind) => kind === "claude-login" || kind === "codex-login";

export function pendingTerminalTab(workspace, kind) {
  return {
    id: pendingPrefix + ++sequence, kind: "terminal", pending: true, error: "",
    workspace, terminalKind: kind, title: pendingTitles[kind] || "终端",
  };
}

// markPendingTab records a failed request, or clears it for a retry.
export function markPendingTab(list, id, error) {
  return list.map((tab) => (tab.id === id ? { ...tab, error } : tab));
}

// settlePendingTab puts the terminal where its placeholder was and drops any
// copy of it that a poll added meanwhile.
export function settlePendingTab(list, id, tab) {
  const rest = list.filter((item) => item.id !== tab.id &&
    (!tab.cli_session_id || item.cli_session_id !== tab.cli_session_id));
  const at = rest.findIndex((item) => item.id === id);
  return at < 0 ? [...rest, tab] : [...rest.slice(0, at), tab, ...rest.slice(at + 1)];
}

// renamePendingLayout points the layout at the terminal wherever it still
// points at the placeholder; a user who moved on stays where they are.
export function renamePendingLayout(layout, id, next) {
  if (layout.active !== id && layout.other !== id) return layout;
  return {
    ...layout,
    ...(layout.active === id && { active: next }),
    ...(layout.other === id && { other: next }),
  };
}
