// Mocked /api/claude-science for UI fixtures. Each fixture is a reply of the
// workbench contract documented in internal/workbench/claude_science.go;
// a Go test decodes every 200 body into the server's own type.
import { createRequire } from "node:module";
const require = createRequire(import.meta.url);
export const claudeScienceFixtures = require("./claude-science-fixtures.json");

// Install leaves the build stopped, start runs it signed out, stop stops it.
const transitions = { install: "stopped", update: "stopped", start: "running-signed-out", stop: "stopped" };

export function claudeScienceRoute(route, request, path, body, state) {
  const fixture = claudeScienceFixtures[state.claudeScience] || claudeScienceFixtures["not-installed"];
  if (path === "/claude-science/host-render" && request.method() === "GET") {
    if (fixture.status !== 200 || !fixture.body.host_render) return route.fulfill({ status: 404, body: "404 page not found" });
    return route.fulfill({ json: { ...fixture.body.host_render, ...state.hostRender } });
  }
  if (path === "/claude-science" && request.method() === "GET") {
    if (fixture.status !== 200) return route.fulfill({ status: fixture.status, contentType: "text/plain; charset=utf-8", body: fixture.text });
    if (fixture.body.host_render && state.hostRender) return route.fulfill({ json: { ...fixture.body, host_render: { ...fixture.body.host_render, ...state.hostRender } } });
    return route.fulfill({ json: fixture.body });
  }
  const action = path.slice("/claude-science/".length);
  if (request.method() === "POST" && ["host-render", "ticket", "release"].includes(action)) return hostRenderRoute(route, action, body, state, fixture);
  if (request.method() !== "POST" || !["install", "update", "start", "stop", "open"].includes(action))
    return route.fulfill({ status: 404, body: "404 page not found" });
  const keys = Object.keys(body || {});
  if (keys.some(key => key !== "fresh") || (keys.length && action !== "open"))
    return route.fulfill({ status: 400, contentType: "text/plain; charset=utf-8", body: "请求格式无效" });
  if (fixture.status === 200 && !fixture.body.supported)
    return route.fulfill({ status: 409, contentType: "text/plain; charset=utf-8", body: fixture.body.unsupported_reason });
  if (action === "open") {
    if (fixture.status !== 200 || !fixture.body.ready)
      return route.fulfill({ status: 409, contentType: "text/plain; charset=utf-8", body: "Claude Science 尚未启动，请先启动后再打开。" });
    let tab = state.tabs.find(item => item.kind === "science");
    const reused = Boolean(tab) && body?.fresh !== true;
    if (!tab) {
      tab = { id: "science-" + ++state.sequence, kind: "science", title: "Claude Science", url: "http://localhost:47800/" };
      state.tabs.push(tab);
    }
    state.active = tab.id;
    return route.fulfill({ json: { id: tab.id, kind: "science", title: "Claude Science", reused } });
  }
  state.claudeScience = transitions[action];
  state.status.job = { id: "claude-science-" + ++state.sequence, kind: "claude-science-" + action, running: false };
  return route.fulfill({ json: state.status.job });
}

// Host rendering (science_host.go): the switch moves between the two
// host-render fixtures; state.hostRender overlays the front's live fields
// (session, refused, signin…) on GET; the ticket's URL (state.frameOrigin)
// is served by the smoke itself.
function hostRenderRoute(route, action, body, state, fixture) {
  const render = fixture.body?.host_render;
  if (action === "release") return route.fulfill({ status: 204, body: "" });
  if (!render || fixture.status !== 200) return route.fulfill({ status: 409, contentType: "text/plain; charset=utf-8", body: "本机渲染尚未开启。" });
  if (action === "host-render") {
    if (typeof body?.enabled !== "boolean" || Object.keys(body).length !== 1) return route.fulfill({ status: 400, contentType: "text/plain; charset=utf-8", body: "请求格式无效" });
    if (body.enabled && !render.available) return route.fulfill({ status: 409, contentType: "text/plain; charset=utf-8", body: render.unavailable_reason });
    state.claudeScience = body.enabled ? "running-host-render" : "running-host-render-available";
    state.hostRender = {};
    return route.fulfill({ json: claudeScienceFixtures[state.claudeScience].body.host_render });
  }
  const host = body?.host;
  if (!host || typeof host !== "object" || Object.keys(body).length !== 1) return route.fulfill({ status: 400, contentType: "text/plain; charset=utf-8", body: "请求格式无效" });
  if (!render.enabled || !render.running) return route.fulfill({ status: 409, contentType: "text/plain; charset=utf-8", body: "本机渲染尚未开启。" });
  state.tickets = (state.tickets || 0) + 1;
  state.hostRender = { ...state.hostRender, session: false, refused: undefined, refused_at: undefined };
  return route.fulfill({ json: { url: (state.frameOrigin || "http://localhost:17840") + "/_cage/enter#" + String(state.tickets).padStart(64, "0") } });
}
