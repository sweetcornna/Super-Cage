// The desktop window's "在本机终端中打开": loaded only in the desktop app
// (main.jsx imports it on demand), so none of it is in the first chunk.
import { api, writeClipboard } from "./api";
import { getClientContext } from "./client-context.mjs";
import { APP_PATH, SETTINGS_PATH, automationNotice, copyCommand, launchFailure, menuItem, openRequest } from "./local-terminal-state.mjs";

const acknowledged = app => "cage-local-terminal-automation-" + app;

// localTerminalOpener reads whether this computer can open a terminal in its
// own terminal application and returns the menu item and its action. changed
// is called when what the item shows changes: an open started or ended.
export async function localTerminalOpener(changed) {
  const supported = (await api(SETTINGS_PATH)).supported !== false;
  let pending = false;
  return {
    item(target, environment) {
      const item = menuItem({ supported, target, context: getClientContext(), environment });
      return pending && item.enabled ? { ...item, enabled: false, reason: "正在打开…" } : item;
    },
    // notify(text, action) shows the outcome; action is "复制命令" when the
    // application could not open the window.
    async open(target, { confirm, notify }) {
      if (pending) return;
      // The application is found at each open, so that one installed since
      // (Herdr or Ghostty) is the one used, with its notice if needed.
      let app;
      try {
        app = (await api(APP_PATH)).app;
      } catch (error) { notify(error.message); return; }
      if (!app) { notify("这台电脑上找不到 Herdr、Ghostty 或系统终端"); return; }
      // Before the request: it waits until the owner answers macOS.
      const notice = automationNotice(app, localStorage.getItem(acknowledged(app.id)) === "1");
      if (notice) {
        if (!(await confirm(notice))) return;
        localStorage.setItem(acknowledged(app.id), "1");
      }
      const request = openRequest(target, getClientContext());
      pending = true;
      changed();
      try {
        await api(request.path, request.body, "POST", request.headers);
      } catch (error) {
        // Only an application that failed to open gets 复制命令; a lock, the
        // bound or an open in progress say so.
        if (error.status === 502) {
          const reason = String(error.message || "").replace(/^无法在 .+? 中打开：/, "");
          notify(launchFailure(app.name, reason), { label: "复制命令", run: () => writeClipboard(copyCommand(target)) });
        } else notify(error.message);
      } finally {
        pending = false;
        changed();
      }
    },
  };
}
