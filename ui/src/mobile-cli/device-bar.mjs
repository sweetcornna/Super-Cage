import { DEVICE_KEY, DEVICE_NAME_KEY, storedConnection } from "./device-link.mjs";
import "./device-bar.css";

// 设备方式 (design §6.5): the slim line above the phone surface of a joined computer, whose it is and the way back.
// Loaded only when the page is that window (web-shell.mjs imports it, the drawer's 返回本机 calls leaveDevice): the
// phone bundle carries none of it. Plain DOM, since nothing in the phone UI needs to know it is there; the room for it
// is reserved by super.css (--device-bar) before this arrives, so the page does not move.

// 返回本机: close the link, forget the connection and go back to this computer's own workbench. A plain request, not
// transport.mjs (whose base is /api/mobile; the gateway keeps /api/client/* local on a device connection), and leaving
// must work whether or not the close arrives: the request is sent with keepalive and the page does not wait for it.
export function leaveDevice() {
  const id = storedConnection().split(":")[1];
  sessionStorage.removeItem(DEVICE_KEY);
  sessionStorage.removeItem(DEVICE_NAME_KEY);
  fetch("/api/client/devices/" + id + "/close", { method: "POST", keepalive: true }).catch(() => {});
  location.assign("/");
}

const bar = document.createElement("div");
bar.className = "device-bar";
bar.setAttribute("role", "region");
bar.setAttribute("aria-label", "设备");
const name = document.createElement("span");
name.className = "device-bar-name";
name.textContent = (sessionStorage.getItem(DEVICE_NAME_KEY) || "另一台电脑") + " · 手机版功能";
const back = document.createElement("button");
back.type = "button";
back.textContent = "返回本机";
back.onclick = leaveDevice;
bar.append(name, back);
document.body.append(bar);
