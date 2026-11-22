// 设备方式: a joined computer's window is the desktop app navigated to /mobile.html with a connection
// "device:<id>:<generation>" kept in sessionStorage (stored by Settings → 设备 → 打开, see ui/src/devices-open.mjs).
// The phone page reads it in transport.mjs (X-Cage-Connection) and in web-shell.mjs (deviceMode). This file is
// part of the phone bundle, so it stays tiny; the rest of device mode is device-bar.mjs, loaded only in that mode.
export const DEVICE_KEY = "cage-cli-local-connection";
export const DEVICE_NAME_KEY = "cage-device-name";

export function storedConnection() {
  try { return sessionStorage.getItem(DEVICE_KEY) || ""; } catch { return ""; }
}
