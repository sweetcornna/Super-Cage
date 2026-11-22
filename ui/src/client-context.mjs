let current = Object.freeze({ mode: "none", connection_id: "" });

export function setClientContext(value) {
  current = Object.freeze({ ...value });
}

export const getClientContext = () => current;
export const isCloudWorkbench = () => current.mode === "remote" || current.tenant_workspace === true;
export const isRemoteClient = () => current.mode === "remote";
// 本机终端's entry writes this computer's ~/.ssh: the window on the
// environment's own computer manages it, and the desktop app for the cloud
// workspace it is connected to when the server offers that (cloud_entry,
// only in a build with the cloud entry).
export const localEntryAvailable = (context = current) =>
  (context.mode !== "remote" && context.tenant_workspace !== true) || (context.desktop_gateway === true && context.cloud_entry === true);

export function assertConnection(connectionID) {
  if (current.connection_id !== connectionID) {
    const error = new Error("工作环境已切换，请在当前环境重新操作。");
    error.code = "connection_changed";
    throw error;
  }
}

export function connectionHeaders(path, context = current) {
  if (path === "/session" || (path.startsWith("/client/") && !isProjectSharesPath(path))) return {};
  return { "X-Cage-Connection": context.connection_id || "disconnected" };
}

export const isProjectSharesPath = (path) => path === "/client/project-shares" || path.startsWith("/client/project-shares/");

export function runtimeURL(path, context = current) {
  const url = new URL("/api" + path, globalThis.location?.origin || "http://localhost");
  url.searchParams.set("connection", context.connection_id || "disconnected");
  return url.pathname + url.search;
}

export function runtimeSocketURL(path, context = current) {
  return `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}${runtimeURL(path, context)}`;
}

// Local VMs of one desktop share the gateway's browser origin, so each VM
// keeps its own workbench layout. The default VM keeps the original keys.
export function clientStorageKey(key, context = current) {
  if (context.mode === "remote") return `${key}:remote:${context.instance_id || context.host_id || "disconnected"}`;
  const vm = context.virtual_machine_id;
  return vm && vm !== "default" ? `${key}:vm:${vm}` : key;
}

export function clientKeyboard(serverKeyboard, context = current) {
  if (context.mode !== "remote") return serverKeyboard;
  try {
    const saved = localStorage.getItem(clientStorageKey("cage-keyboard", context));
    if (["mac", "native"].includes(saved)) return saved;
  } catch { /* The client platform remains a usable default without storage. */ }
  return context.client_platform === "darwin" ? "mac" : "native";
}

export function saveClientKeyboard(value) {
  if (!["mac", "native"].includes(value)) throw new Error("键盘习惯无效");
  localStorage.setItem(clientStorageKey("cage-keyboard"), value);
  window.dispatchEvent(new Event("cage-client-preferences"));
}
