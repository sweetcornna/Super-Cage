import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { transformWithOxc } from "vite";
import { agentAccountsPath, createAgentAccountsSequencer } from "./agent-accounts-state.mjs";
import { vaultPollActive } from "./idle-poll.mjs";
import { networkState } from "./safety-state.mjs";

const srcDir = path.dirname(fileURLToPath(import.meta.url));
const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

function deferred() {
  let resolve, reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

// Lightweight React hooks runtime for executing real JSX components under node --test
// without a DOM. Tracks hook slots, effect dependencies/cleanups, and unmount lifecycle.
function createHooksHarness() {
  let hooks = [];
  let cursor = 0;
  let pendingEffects = [];
  let activeCleanups = [];
  let mounted = true;
  let rerender = null;
  let visibility = true;

  const depsEqual = (a, b) => {
    if (!a || !b || a.length !== b.length) return false;
    for (let i = 0; i < a.length; i++) {
      if (!Object.is(a[i], b[i])) return false;
    }
    return true;
  };

  const React = {
    Fragment: Symbol("Fragment"),
    createElement(type, props, ...children) {
      return { type, props: { ...(props || {}), children: children.flat(Infinity) } };
    },
    useState(initial) {
      const i = cursor++;
      if (!(i in hooks)) {
        hooks[i] = typeof initial === "function" ? initial() : initial;
      }
      const setState = (next) => {
        if (!mounted) return;
        const value = typeof next === "function" ? next(hooks[i]) : next;
        if (!Object.is(hooks[i], value)) {
          hooks[i] = value;
          rerender?.();
        }
      };
      return [hooks[i], setState];
    },
    useRef(initial) {
      const i = cursor++;
      if (!(i in hooks)) hooks[i] = { current: initial };
      return hooks[i];
    },
    useMemo(factory, deps) {
      const i = cursor++;
      const prev = hooks[i];
      if (!prev || !depsEqual(prev.deps, deps)) {
        hooks[i] = { value: factory(), deps };
      }
      return hooks[i].value;
    },
    useCallback(fn, deps) {
      return React.useMemo(() => fn, deps);
    },
    useEffect(fn, deps) {
      const i = cursor++;
      const prev = hooks[i];
      if (!prev || !deps || !depsEqual(prev.deps, deps)) {
        pendingEffects.push({ index: i, fn, deps, prevCleanup: prev?.cleanup });
      }
    },
  };

  const flushEffects = () => {
    while (pendingEffects.length) {
      const batch = pendingEffects;
      pendingEffects = [];
      for (const item of batch) {
        item.prevCleanup?.();
        const cleanup = item.fn();
        hooks[item.index] = { deps: item.deps, cleanup: typeof cleanup === "function" ? cleanup : null };
        activeCleanups[item.index] = hooks[item.index].cleanup;
      }
    }
  };

  let lastTree = null;
  let currentComponent = null;
  let currentProps = null;
  let rendering = false;
  let queuedRender = false;

  const renderNow = () => {
    if (!mounted || !currentComponent) return lastTree;
    if (rendering) {
      queuedRender = true;
      return lastTree;
    }
    rendering = true;
    try {
      do {
        queuedRender = false;
        cursor = 0;
        lastTree = currentComponent(currentProps);
        flushEffects();
      } while (queuedRender && mounted);
    } finally {
      rendering = false;
    }
    return lastTree;
  };
  rerender = renderNow;

  return {
    React,
    useWindowVisibility: () => visibility,
    mount(component, props) {
      currentComponent = component;
      currentProps = props;
      return renderNow();
    },
    update(props) {
      currentProps = props;
      return renderNow();
    },
    setVisible(next) {
      visibility = Boolean(next);
      return renderNow();
    },
    tree: () => lastTree,
    unmount() {
      mounted = false;
      for (const cleanup of activeCleanups) cleanup?.();
      activeCleanups = [];
    },
  };
}

function findNodes(node, predicate, out = []) {
  if (!node || typeof node !== "object") return out;
  if (Array.isArray(node)) {
    for (const item of node) findNodes(item, predicate, out);
    return out;
  }
  if (predicate(node)) out.push(node);
  if (node.props?.children) findNodes(node.props.children, predicate, out);
  return out;
}

function nodeText(node) {
  if (node == null || typeof node === "boolean") return "";
  if (typeof node === "string" || typeof node === "number") return String(node);
  if (Array.isArray(node)) return node.map(nodeText).join("");
  if (typeof node === "object" && node.props?.children) return nodeText(node.props.children);
  return "";
}

// Compile a real JSX module from ui/src with Vite's OXC JSX transform and bind
// its imports to our harness + test mocks so tests run the actual source file.
async function loadJSXExports(filename, { React, api, writeClipboard, useWindowVisibility }) {
  const fullPath = path.join(srcDir, filename);
  const raw = fs.readFileSync(fullPath, "utf8");
  const { code } = await transformWithOxc(raw, fullPath, {
    lang: "jsx",
    jsx: { runtime: "classic" },
  });
  const IconStub = (props) => React.createElement("icon", props);
  const ComponentStub = (props) => React.createElement("stub", props);
  const lucideProxy = new Proxy({}, { get: () => IconStub });

  // Strip import statements (binding recognized identifiers via function arguments)
  // and collect exported declarations.
  const stripped = code
    .replace(/^\s*import\s+[\s\S]*?from\s+["'][^"']+["'];?\s*$/gm, "")
    .replace(/^\s*import\s+["'][^"']+["'];?\s*$/gm, "");
  const exported = [];
  const body = stripped.replace(/\bexport\s+(?:async\s+)?function\s+([A-Za-z0-9_$]+)/g, (m, name) => {
    exported.push(name);
    return m.replace(/^export\s+/, "");
  });

  const bindings = {
    React,
    useState: React.useState,
    useRef: React.useRef,
    useEffect: React.useEffect,
    useCallback: React.useCallback,
    useMemo: React.useMemo,
    api,
    writeClipboard: writeClipboard || (async () => {}),
    useWindowVisibility,
    vaultPollActive,
    networkState,
    agentAccountsPath,
    createAgentAccountsSequencer,
    projectName: (p) => (p ? p.split("/").pop() : ""),
    InlineSelect: ComponentStub,
    SafetyMessage: ComponentStub,
    ProxyYAMLInput: ComponentStub,
    NetworkExplanation: ComponentStub,
    EmptyState: ComponentStub,
    AgentIcon: ComponentStub,
    // Icons from lucide-react used in the tested modules:
    ...Object.fromEntries(
      [
        "Activity", "ArrowDown", "ArrowRight", "ArrowUp", "Check", "ChevronDown",
        "CircleHelp", "Copy", "Download", "ExternalLink", "Flame", "KeyRound",
        "LoaderCircle", "LogIn", "Monitor", "Network", "Pencil", "Plus",
        "RefreshCw", "Route", "Search", "Server", "Settings", "ShieldAlert",
        "ShieldCheck", "SlidersHorizontal", "Smartphone", "Square", "Terminal",
        "TerminalSquare", "Timer", "Trash2", "X",
      ].map((name) => [name, lucideProxy[name]]),
    ),
  };

  const fn = new Function(...Object.keys(bindings), `${body}\nreturn { ${exported.join(", ")} };`);
  return fn(...Object.values(bindings));
}

// F5 (proxy-settings.jsx): hiding the window while mutate() or measure() is in
// flight must still clear pending so controls do not stay disabled after show.
test("F5: ProxySettings clears pending when window hides during mutate() and measure()", async (t) => {
  const proxyPayload = {
    active: true,
    running: true,
    port: 17896,
    selected: "n1",
    route: ["n1"],
    core_available: true,
    network_open: true,
    state_label: "已放行",
    sources: [{ id: "s1", name: "Sub 1", nodes: [{ id: "n1", name: "Node 1", type: "ss" }] }],
  };

  await t.test("hide during mutate()", async () => {
    const harness = createHooksHarness();
    const routeReq = deferred();
    const api = async (url) => {
      if (url === "/proxy") return proxyPayload;
      if (url === "/proxy/route") return routeReq.promise;
      if (url === "/status") return { job: { id: "job-1", running: false, message: "ok" } };
      throw new Error("unexpected api " + url);
    };
    const { ProxySettings } = await loadJSXExports("proxy-settings.jsx", {
      React: harness.React,
      api,
      useWindowVisibility: harness.useWindowVisibility,
    });
    t.after(() => harness.unmount());

    const props = {
      status: { ready: true, vm_running: true, job: { running: false } },
      onApplied: async () => {},
      onConfirm: async () => true,
      onDirty: () => {},
    };
    harness.mount(ProxySettings, props);
    await settle();

    const applyBtn = () => findNodes(harness.tree(), (n) => n.type === "button" && nodeText(n).includes("重新应用"))[0];
    assert.ok(applyBtn(), "expected apply button after initial /proxy load");
    assert.equal(applyBtn().props.disabled, false);

    // Trigger mutate("route", ...), then hide the window while the request is in flight.
    applyBtn().props.onClick();
    assert.equal(applyBtn().props.disabled, true, "apply button must be disabled while mutate is pending");

    harness.setVisible(false);
    routeReq.resolve({ id: "job-1" });
    // mutate() polls /status after 500 ms.
    await new Promise((resolve) => setTimeout(resolve, 600));

    harness.setVisible(true);
    await settle();
    assert.equal(applyBtn().props.disabled, false, "controls must not stay disabled after hiding during mutate()");
  });

  await t.test("hide during measure()", async () => {
    const harness = createHooksHarness();
    const delayReq = deferred();
    const api = async (url) => {
      if (url === "/proxy") return proxyPayload;
      if (url === "/proxy/delay") return delayReq.promise;
      throw new Error("unexpected api " + url);
    };
    const { ProxySettings } = await loadJSXExports("proxy-settings.jsx", {
      React: harness.React,
      api,
      useWindowVisibility: harness.useWindowVisibility,
    });
    t.after(() => harness.unmount());

    const props = {
      status: { ready: true, vm_running: true, job: { running: false } },
      onApplied: async () => {},
      onConfirm: async () => true,
      onDirty: () => {},
    };
    harness.mount(ProxySettings, props);
    await settle();

    const measureBtn = () => findNodes(harness.tree(), (n) => n.type === "button" && nodeText(n).trim() === "测速")[0];
    assert.ok(measureBtn(), "expected measure button");
    assert.equal(measureBtn().props.disabled, false);

    measureBtn().props.onClick();
    assert.equal(measureBtn().props.disabled, true, "measure button must be disabled while measuring");

    harness.setVisible(false);
    delayReq.resolve({ delay: 42 });
    await settle();

    harness.setVisible(true);
    await settle();
    assert.equal(measureBtn().props.disabled, false, "controls must not stay disabled after hiding during measure()");
  });
});

// F5 (tailscale.jsx): hiding during act()/login() must clear busy and keep
// loginRequested intact so the login URL still opens when shown again, and
// !ready during onInstall() must still clear busy.
test("F5: TailscalePanel clears busy on hide during act()/install and preserves loginRequested", async (t) => {
  await t.test("hide during login/act preserves loginRequested and clears busy", async () => {
    const harness = createHooksHarness();
    const connectReq = deferred();
    let tailscaleState = { installed: true, enabled: false, backend: "Stopped", auth_url: "" };
    const openedLogin = [];
    const api = async (url) => {
      if (url === "/tailscale") return tailscaleState;
      if (url === "/tailscale/connect") return connectReq.promise;
      throw new Error("unexpected api " + url);
    };
    const { TailscalePanel } = await loadJSXExports("tailscale.jsx", {
      React: harness.React,
      api,
      useWindowVisibility: harness.useWindowVisibility,
    });
    t.after(() => harness.unmount());

    const props = {
      status: { ready: true, job: { running: false } },
      onOpenLogin: async (value) => { openedLogin.push(value.auth_url); return true; },
      onOpenBrowser: async () => true,
      onInstall: async () => {},
      onConfirm: async () => true,
    };
    harness.mount(TailscalePanel, props);
    await settle();

    const loginBtn = () => findNodes(harness.tree(), (n) => n.type === "button" && /连接并登录|重新发起登录/.test(nodeText(n)))[0];
    assert.ok(loginBtn(), "expected login button");
    assert.equal(loginBtn().props.disabled, false);

    // Click login -> act("connect") in flight -> hide window.
    loginBtn().props.onClick();
    assert.equal(loginBtn().props.disabled, true, "button must be disabled while act() runs");

    harness.setVisible(false);
    tailscaleState = { installed: true, enabled: true, backend: "NeedsLogin", auth_url: "" };
    connectReq.resolve(tailscaleState);
    await settle();

    // Window comes back and poll receives auth_url.
    tailscaleState = { installed: true, enabled: true, backend: "NeedsLogin", auth_url: "https://login.tailscale.com/a/test" };
    harness.setVisible(true);
    await settle();

    assert.equal(loginBtn().props.disabled, false, "Tailscale controls must not stay disabled after hide during act()");
    assert.deepEqual(openedLogin, ["https://login.tailscale.com/a/test"], "loginRequested must survive window hide so login opens automatically");
  });

  await t.test("install clears busy even when ready flips false mid-install", async () => {
    const harness = createHooksHarness();
    const installReq = deferred();
    const api = async (url) => {
      if (url === "/tailscale") return { installed: false, enabled: false, backend: "NotInstalled" };
      throw new Error("unexpected api " + url);
    };
    const { TailscalePanel } = await loadJSXExports("tailscale.jsx", {
      React: harness.React,
      api,
      useWindowVisibility: harness.useWindowVisibility,
    });
    t.after(() => harness.unmount());

    const baseProps = {
      status: { ready: true, job: { running: false } },
      onOpenLogin: async () => true,
      onOpenBrowser: async () => true,
      onInstall: () => installReq.promise,
      onConfirm: async () => true,
    };
    harness.mount(TailscalePanel, baseProps);
    await settle();

    const installBtn = () => findNodes(harness.tree(), (n) => n.type === "button" && nodeText(n).includes("安装 Tailscale"))[0];
    assert.ok(installBtn(), "expected install button");
    installBtn().props.onClick();
    assert.equal(installBtn().props.disabled, true);

    // Job starts -> ready flips false while onInstall is awaiting.
    harness.update({ ...baseProps, status: { ready: true, job: { running: true } } });
    installReq.resolve();
    await settle();

    // Job ends -> ready flips true again.
    harness.update({ ...baseProps, status: { ready: true, job: { running: false } } });
    await settle();

    assert.equal(installBtn().props.disabled, false, "install button must not stay disabled after !ready during onInstall()");
  });
});

// F11: usePasswordVault must reset state to emptyBrowser when switching browser tabs
// so the previous site's saved-login candidates never flash for the new tab.
test("F11: usePasswordVault resets state immediately when tab changes", async (t) => {
  const harness = createHooksHarness();
  const tabBReq = deferred();
  const api = async (url, body) => {
    if (url === "/vault/browser" && body?.tab === "tab-a") {
      return {
        available: true,
        origin: "https://site-a.example",
        entries: [{ id: "e1", username: "alice" }],
        candidates: [{ id: "c1", tab: "tab-a", origin: "https://site-a.example", username: "alice" }],
      };
    }
    if (url === "/vault/browser" && body?.tab === "tab-b") {
      return tabBReq.promise;
    }
    throw new Error("unexpected api " + url);
  };
  const { usePasswordVault } = await loadJSXExports("password-vault.jsx", {
    React: harness.React,
    api,
    useWindowVisibility: harness.useWindowVisibility,
  });
  t.after(() => harness.unmount());

  function Harness({ ready, tab, browserVisible }) {
    return usePasswordVault(ready, tab, browserVisible);
  }

  harness.mount(Harness, { ready: true, tab: "tab-a", browserVisible: true });
  await settle();
  assert.equal(harness.tree().state.origin, "https://site-a.example");
  assert.equal(harness.tree().state.candidates.length, 1);

  // Switch tab to tab-b while its status poll is still in flight.
  harness.update({ ready: true, tab: "tab-b", browserVisible: true });
  assert.deepEqual(
    harness.tree().state,
    { available: false, entries: [], candidates: [] },
    "switching tabs must immediately reset vault state to emptyBrowser instead of keeping previous site's candidates",
  );

  // Resolve tab-b, then hide & show window on the SAME tab — state stays intact.
  tabBReq.resolve({ available: true, origin: "https://site-b.example", entries: [], candidates: [] });
  await settle();
  assert.equal(harness.tree().state.origin, "https://site-b.example");
  harness.setVisible(false);
  assert.equal(harness.tree().state.origin, "https://site-b.example", "hiding on the same tab must not wipe state");
});

// F13: in AgentAccountsPanel, a load() that supersedes an in-flight mutate()
// (e.g. workspace prop change mid-POST) must still clear submitting while
// discarding the stale mutation response.
test("F13: AgentAccountsPanel clears submitting when load() supersedes an in-flight mutate()", async (t) => {
  const harness = createHooksHarness();
  const postReq = deferred();
  const load2Req = deferred();
  const makeState = (autoCooldown) => ({
    agent: { id: "antigravity", name: "Antigravity", short_name: "Antigravity", auth: { methods: ["oauth"], api_key_env_vars: [] }, isolation: { mode: "dir" } },
    accounts: [{ id: "acc-1", label: "Work", source: "managed", auth_mode: "oauth", enabled: true, status: "ready", weight: 10 }],
    scheduling: { mode: "smart", project_affinity_minutes: 120, auto_cooldown: autoCooldown, project_pins: {} },
    schedule_preview: { account_id: "acc-1" },
    active_counts: {},
    detection_status: "signed_out",
  });
  const api = async (url, body) => {
    if (body) return postReq.promise;
    if (url.includes("workspace=%2Fws2")) return load2Req.promise;
    return makeState(true);
  };
  const { AgentAccountsPanel } = await loadJSXExports("agent-accounts.jsx", {
    React: harness.React,
    api,
    useWindowVisibility: harness.useWindowVisibility,
  });
  t.after(() => harness.unmount());

  const baseProps = {
    agentId: "antigravity",
    workspace: "/ws1",
    projects: ["/ws1", "/ws2"],
    running: true,
    environmentBusy: false,
    onConfirm: async () => true,
  };
  harness.mount(AgentAccountsPanel, baseProps);
  await settle();

  const toggleInput = () => findNodes(harness.tree(), (n) => n.type === "input" && n.props?.type === "checkbox")[0];
  assert.ok(toggleInput(), "expected scheduling auto_cooldown checkbox");
  assert.equal(toggleInput().props.disabled, false);

  // Trigger mutate() (POST), leaving it in flight.
  toggleInput().props.onChange({ target: { checked: false } });
  assert.equal(toggleInput().props.disabled, true, "controls must be disabled while mutate() is in flight");

  // Workspace prop changes mid-POST -> effect triggers load() superseding mutate()'s ticket.
  harness.update({ ...baseProps, workspace: "/ws2" });
  load2Req.resolve(makeState(true));
  await settle();

  // Superseded POST resolves late with stale auto_cooldown:false.
  postReq.resolve(makeState(false));
  await settle();

  assert.equal(toggleInput().props.disabled, false, "submitting must clear even when load() supersedes mutate()");
  assert.equal(toggleInput().props.checked, true, "stale superseded mutate() response must not overwrite newer load() state");
});
