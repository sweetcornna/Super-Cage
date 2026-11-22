// The desktop window's "在本机终端中打开": loaded only in the desktop app
// (main.jsx imports it on demand), so none of it is in the first chunk.
import { api, writeClipboard } from "./api";
import { getClientContext } from "./client-context.mjs";
import { MACHINE_APPS, SETTINGS_PATH, appName, appsRequest, automationNotice, copyCommand, herdrSetUp, launchFailure, menuItem, normalizeSettings, openRequest } from "./local-terminal-state.mjs";

const acknowledged = app => "cage-local-terminal-automation-" + app;

// localTerminalOpener reads this computer's choice and returns the menu
// item and its action. changed is called when what the item shows changes:
// the settings were saved, an open started or ended.
export async function localTerminalOpener(changed) {
  let settings = normalizeSettings((await api(SETTINGS_PATH)).settings);
  let apps = null, pending = false;
  window.addEventListener("cage-local-terminal-settings", () => {
    api(SETTINGS_PATH).then(reply => { settings = normalizeSettings(reply.settings); changed(); }).catch(() => {});
  });
  return {
    item(target, environment) {
      const item = menuItem({ settings, apps: apps || undefined, target, context: getClientContext(), environment });
      return pending && item.enabled ? { ...item, enabled: false, reason: "正在打开…" } : item;
    },
    // notify(text, action) shows the outcome; action is "复制命令" when the
    // application could not open the window.
    async open(target, { confirm, notify }) {
      if (pending) return;
      try {
        // Herdr's place in the list depends on the environment shown.
        if (!apps || MACHINE_APPS.includes(settings.app)) {
          const listing = appsRequest(getClientContext());
          apps = (await api(listing.path, undefined, undefined, listing.headers)).apps;
        }
      } catch (error) { notify(error.message); return; }
      const app = apps.find(item => item.id === settings.app);
      // Before the request: it waits until the owner answers macOS.
      const notice = automationNotice(app, localStorage.getItem(acknowledged(settings.app)) === "1");
      if (notice) {
        if (!(await confirm(notice))) return;
        localStorage.setItem(acknowledged(settings.app), "1");
      }
      const request = openRequest(target, getClientContext());
      pending = true;
      changed();
      try {
        await api(request.path, request.body, "POST", request.headers);
      } catch (error) {
        // Only an application that failed to open gets the advice to pick
        // another; a lock, the bound or an open in progress say so.
        if (error.status === 412) {
          // Herdr: the environment is not a machine there (any more).
          let command = "";
          try { command = (await api("/entry"))?.herdr?.command || ""; } catch { /* the dialog says where to turn it on */ }
          const dialog = herdrSetUp(command, String(error.message || "").replace(/^Herdr 没能打开：/, ""));
          if (await confirm(dialog) && dialog.code) await writeClipboard(dialog.code);
        } else if (error.status === 502) {
          const reason = String(error.message || "").replace(/^无法在 .+? 中打开：/, "");
          notify(launchFailure(appName(settings.app, apps), reason), { label: "复制命令", run: () => writeClipboard(copyCommand(target)) });
        } else notify(error.message);
      } finally {
        pending = false;
        changed();
      }
    },
  };
}
