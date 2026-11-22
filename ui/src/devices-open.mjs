import { api } from "./api";
import { deviceIdOf, rememberDevice } from "./devices-state.mjs";

// 打开: the gateway opens its saved link to the joined computer and answers with a connection id; the window keeps
// it and goes to the phone page on this origin (design §3.5), where mobile-cli/transport.mjs sends it with every
// request. Only a path under /mobile.html on this origin is followed.
export async function openComputer(id, name) {
  const reply = await api("/client/devices/" + encodeURIComponent(id) + "/open", {}, "POST");
  const connection = String(reply?.connection || "");
  if (!deviceIdOf(connection)) throw new Error("那台电脑没有返回可用的连接，请稍后重试。");
  rememberDevice(connection, name);
  const url = typeof reply.url === "string" && /^\/mobile\.html(?:[?#]|$)/.test(reply.url) ? reply.url : "/mobile.html";
  location.assign(url);
}
