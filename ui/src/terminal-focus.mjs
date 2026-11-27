const terminalID = /^[0-9a-f]{16}$/;
const conversationID = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i;

export function readTerminalFocus(key) {
  try {
    const value = JSON.parse(localStorage.getItem(key));
    if (value?.pane && conversationID.test(value.pane)) return { pane: value.pane };
    if (value?.terminal && terminalID.test(value.terminal)) {
      return { terminal: value.terminal, conversation: conversationID.test(value.conversation) ? value.conversation : "" };
    }
  } catch { /* A window can still reopen sessions from the list. */ }
  return null;
}

export function writeTerminalFocus(key, value) {
  try {
    if (value) localStorage.setItem(key, JSON.stringify(value));
    else localStorage.removeItem(key);
  } catch { /* A window can still use the current tab. */ }
}

export function terminalFocusFor(tab) {
  if (tab?.kind !== "terminal" || tab.pending || !terminalID.test(tab.sessionId) || tab.terminalKind?.endsWith("-login")) return null;
  return { terminal: tab.sessionId, conversation: conversationID.test(tab.cli_session_id) ? tab.cli_session_id : "" };
}

export function terminalFocusAction(focus, terminals) {
  if (!focus?.terminal) return {};
  const exact = terminals.find((item) => item.id === focus.terminal && (!focus.conversation || item.cli_session_id === focus.conversation));
  if (exact) return { terminal: exact };
  if (!focus.conversation) return { missing: true };
  const bound = terminals.find((item) => item.cli_session_id === focus.conversation && (!item.closed || item.herdr_live));
  return bound ? { terminal: bound } : { recover: focus.conversation };
}
