import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { api } from "./api";

// Manages the universal Agent CLI catalog, guest installation state,
// prerequisites, and per-agent terminal configuration without background polling.
export function useAgents({ vmRunning = false, ready = false, job = null } = {}) {
  const [data, setData] = useState(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const generation = useRef(0);
  const lastJobId = useRef("");

  const refresh = useCallback(async (force = false) => {
    const request = ++generation.current;
    setLoading(true);
    try {
      const result = await api(force ? "/agent-clis?force=1&refresh=1" : "/agent-clis");
      if (generation.current === request) {
        setData(result);
        setError("");
      }
      return result;
    } catch (e) {
      if (generation.current === request) setError(e.message);
      return null;
    } finally {
      if (generation.current === request) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh(false);
    return () => { generation.current++; };
  }, [refresh, vmRunning, ready]);

  // Automatically re-probe once an agent install/uninstall or environment tools job finishes.
  useEffect(() => {
    if (!job || job.running || !job.id) return;
    if (lastJobId.current === job.id) return;
    if (
      job.kind === "agent-install" ||
      job.kind === "agent-uninstall" ||
      job.kind === "environment-tools" ||
      job.kind === "start"
    ) {
      lastJobId.current = job.id;
      void refresh(true);
    }
  }, [job, refresh]);

  const install = useCallback(async (agentId, channel) => {
    return await api("/agent-clis/install", { agent_id: agentId, channel });
  }, []);

  const uninstall = useCallback(async (agentId, channel = "") => {
    return await api("/agent-clis/uninstall", { agent_id: agentId, channel });
  }, []);

  const saveConfig = useCallback(async (patch) => {
    const result = await api("/agent-clis/config", patch);
    if (result?.agents) {
      generation.current++;
      setLoading(false);
      setData(result);
      setError("");
    }
    return result;
  }, []);

  const saveCustom = useCallback(async (spec) => {
    const result = await api("/agent-clis/custom", spec);
    if (result?.agents) {
      setData(result);
      setError("");
    }
    return result;
  }, []);

  const deleteCustom = useCallback(async (id) => {
    const result = await api("/agent-clis/custom/" + encodeURIComponent(id), undefined, "DELETE");
    if (result?.agents) {
      setData(result);
      setError("");
    }
    return result;
  }, []);

  const agents = data?.agents || [];
  const installedAgents = useMemo(
    () => agents.filter((entry) => entry.spec.id === "claude" || Boolean(entry.runtime?.installed)),
    [agents],
  );
  const byId = useMemo(
    () => new Map(agents.map((entry) => [entry.spec.id, entry])),
    [agents],
  );

  return {
    data,
    agents,
    installedAgents,
    byId,
    prerequisites: data?.prerequisites || {},
    probedAt: data?.probed_at || 0,
    probeError: data?.probe_error || "",
    loading,
    error,
    refresh,
    install,
    uninstall,
    saveConfig,
    saveCustom,
    deleteCustom,
  };
}

// Manages the optional T3 Code integration without any idle background polling.
export function useT3Code({ vmRunning = false, ready = false, job = null } = {}) {
  const [data, setData] = useState(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const generation = useRef(0);
  const lastJobId = useRef("");

  const applyPayload = useCallback((result) => {
    if (result && typeof result === "object") {
      generation.current++;
      setLoading(false);
      setData(result);
      setError("");
    }
    return result;
  }, []);

  const refresh = useCallback(async () => {
    const request = ++generation.current;
    setLoading(true);
    try {
      const result = await api("/t3code");
      if (generation.current === request) {
        setData(result);
        setError("");
      }
      return result;
    } catch (e) {
      if (generation.current === request) setError(e.message);
      return null;
    } finally {
      if (generation.current === request) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
    return () => { generation.current++; };
  }, [refresh, vmRunning, ready]);

  useEffect(() => {
    if (!job || job.running || !job.id) return;
    if (lastJobId.current === job.id) return;
    if (
      job.kind === "t3code-install" ||
      job.kind === "environment-tools" ||
      job.kind === "start" ||
      job.kind === "stop"
    ) {
      lastJobId.current = job.id;
      void refresh();
    }
  }, [job, refresh]);

  const saveConfig = useCallback(async (patch) => {
    const result = await api("/t3code/config", patch);
    return applyPayload(result);
  }, [applyPayload]);

  const install = useCallback(async (channel = "npm") => {
    return await api("/t3code/install", { channel });
  }, []);

  const start = useCallback(async () => {
    const result = await api("/t3code/start", {});
    return applyPayload(result);
  }, [applyPayload]);

  const stop = useCallback(async () => {
    const result = await api("/t3code/stop", {});
    return applyPayload(result);
  }, [applyPayload]);

  const restart = useCallback(async () => {
    const result = await api("/t3code/restart", {});
    return applyPayload(result);
  }, [applyPayload]);

  const open = useCallback(async (options = {}) => {
    const result = await api("/t3code/open", options);
    if (result && typeof result === "object") {
      setData((prev) => (prev ? {
        ...prev,
        running: prev.mode === "managed" ? true : prev.running,
        url: result.url || prev.url,
        error: "",
      } : prev));
      setError("");
    }
    return result;
  }, []);

  return {
    data,
    enabled: Boolean(data?.enabled),
    mode: data?.mode || "managed",
    external_url: data?.external_url || "",
    port: data?.port || 0,
    open_mode: data?.open_mode || "workbench",
    focus_chrome: data?.focus_chrome !== false,
    account_bindings: data?.account_bindings || {},
    installed: Boolean(data?.installed),
    version: data?.version || "",
    running: Boolean(data?.running),
    url: data?.url || "",
    active_bindings: data?.active_bindings || {},
    supports_managed: data?.supports_managed,
    direct_available: data?.direct_available,
    serverError: data?.error || "",
    loading,
    error,
    refresh,
    saveConfig,
    install,
    start,
    stop,
    restart,
    open,
  };
}

// Manages the optional Magpie model router & local gateway integration without idle polling.
export function useMagpie({ vmRunning = false, ready = false, job = null } = {}) {
  const [data, setData] = useState(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const generation = useRef(0);
  const lastJobId = useRef("");

  const applyPayload = useCallback((result) => {
    if (result && typeof result === "object") {
      generation.current++;
      setLoading(false);
      setData(result);
      setError("");
    }
    return result;
  }, []);

  const refresh = useCallback(async (force = false) => {
    const request = ++generation.current;
    setLoading(true);
    try {
      const result = await api(force ? "/magpie?force=1" : "/magpie");
      if (generation.current === request) {
        setData(result);
        setError("");
      }
      return result;
    } catch (e) {
      if (generation.current === request) setError(e.message);
      return null;
    } finally {
      if (generation.current === request) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh(false);
    return () => { generation.current++; };
  }, [refresh, vmRunning, ready]);

  useEffect(() => {
    if (!job || job.running || !job.id) return;
    if (lastJobId.current === job.id) return;
    if (
      job.kind === "magpie-install" ||
      job.kind === "magpie-uninstall" ||
      job.kind === "environment-tools" ||
      job.kind === "start" ||
      job.kind === "stop"
    ) {
      lastJobId.current = job.id;
      void refresh(true);
    }
  }, [job, refresh]);

  const saveConfig = useCallback(async (patch) => {
    const result = await api("/magpie/config", patch);
    return applyPayload(result);
  }, [applyPayload]);

  const install = useCallback(async () => {
    return await api("/magpie/install", {});
  }, []);

  const uninstall = useCallback(async () => {
    return await api("/magpie/uninstall", {});
  }, []);

  const start = useCallback(async () => {
    const result = await api("/magpie/start", {});
    return applyPayload(result);
  }, [applyPayload]);

  const stop = useCallback(async () => {
    const result = await api("/magpie/stop", {});
    return applyPayload(result);
  }, [applyPayload]);

  const restart = useCallback(async () => {
    const result = await api("/magpie/restart", {});
    return applyPayload(result);
  }, [applyPayload]);

  const open = useCallback(async (options = {}) => {
    const result = await api("/magpie/open", options);
    if (result && typeof result === "object") {
      setData((prev) => (prev ? {
        ...prev,
        running: prev.mode === "managed" ? true : prev.running,
        gateway_running: typeof result.gateway_running === "boolean"
          ? result.gateway_running
          : prev.gateway_running,
        url: result.url || prev.url,
        gateway_url: result.gateway_url || prev.gateway_url,
        error: "",
      } : prev));
      setError("");
    }
    return result;
  }, []);

  return {
    data,
    enabled: Boolean(data?.enabled),
    mode: data?.mode || "managed",
    external_url: data?.external_url || "",
    port: data?.port || 0,
    gateway_port: data?.gateway_port || 3425,
    gateway_url: data?.gateway_url || "",
    gateway_running: Boolean(data?.gateway_running),
    gateway_only: Boolean(data?.gateway_only),
    open_mode: data?.open_mode || "workbench",
    focus_chrome: data?.focus_chrome !== false,
    installed: Boolean(data?.installed),
    version: data?.version || "",
    running: Boolean(data?.running),
    url: data?.url || "",
    supports_managed: data?.supports_managed,
    direct_available: data?.direct_available,
    serverError: data?.error || "",
    loading,
    error,
    refresh,
    saveConfig,
    install,
    uninstall,
    start,
    stop,
    restart,
    open,
  };
}

// Manages the optional Herdr (herdr-core) persistent terminals without idle polling.
export function useHerdr({ vmRunning = false, ready = false, job = null } = {}) {
  const [data, setData] = useState(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const generation = useRef(0);
  const lastJobId = useRef("");

  const applyPayload = useCallback((result) => {
    if (result && typeof result === "object") {
      generation.current++;
      setLoading(false);
      setData(result);
      setError("");
    }
    return result;
  }, []);

  const refresh = useCallback(async (force = false) => {
    const request = ++generation.current;
    setLoading(true);
    try {
      const result = await api(force ? "/herdr?force=1" : "/herdr");
      if (generation.current === request) {
        setData(result);
        setError("");
      }
      return result;
    } catch (e) {
      if (generation.current === request) setError(e.message);
      return null;
    } finally {
      if (generation.current === request) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh(false);
    return () => { generation.current++; };
  }, [refresh, vmRunning, ready]);

  useEffect(() => {
    if (!job || job.running || !job.id) return;
    if (lastJobId.current === job.id) return;
    if (
      job.kind === "herdr-install" ||
      job.kind === "herdr-uninstall" ||
      job.kind === "environment-tools" ||
      job.kind === "start" ||
      job.kind === "stop"
    ) {
      lastJobId.current = job.id;
      void refresh(true);
    }
  }, [job, refresh]);

  const saveConfig = useCallback(async (patch) => {
    const result = await api("/herdr/config", patch);
    return applyPayload(result);
  }, [applyPayload]);

  // update: also replace a working herdr-core with a newer release.
  const install = useCallback(async (update = false) => {
    return await api("/herdr/install", { update: Boolean(update) });
  }, []);

  // force: end the terminals still running in Herdr (the card asks first).
  const uninstall = useCallback(async (force = false) => {
    return await api("/herdr/uninstall", { force: Boolean(force) });
  }, []);

  const start = useCallback(async () => {
    const result = await api("/herdr/start", {});
    return applyPayload(result);
  }, [applyPayload]);

  const stop = useCallback(async (force = false) => {
    const result = await api("/herdr/stop", { force: Boolean(force) });
    return applyPayload(result);
  }, [applyPayload]);

  const restart = useCallback(async () => {
    const result = await api("/herdr/restart", {});
    return applyPayload(result);
  }, [applyPayload]);

  return {
    data,
    enabled: Boolean(data?.enabled),
    takeover_terminals: data?.takeover_terminals !== false,
    installed: Boolean(data?.installed),
    version: data?.version || "",
    binary_path: data?.binary_path || "",
    server_running: Boolean(data?.server_running),
    terminals_count: Number(data?.terminals_count) || 0,
    legacy_installed: Boolean(data?.legacy_installed),
    supports_managed: data?.supports_managed,
    serverError: data?.error || "",
    loading,
    error,
    refresh,
    saveConfig,
    install,
    uninstall,
    start,
    stop,
    restart,
  };
}

// Manages the optional Orca Server (stablyai/orca server-only orcad) integration without idle polling.
export function useOrca({ vmRunning = false, ready = false, job = null } = {}) {
  const [data, setData] = useState(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const generation = useRef(0);
  const lastJobId = useRef("");

  const applyPayload = useCallback((result) => {
    if (result && typeof result === "object") {
      generation.current++;
      setLoading(false);
      setData(result);
      setError("");
    }
    return result;
  }, []);

  const refresh = useCallback(async (force = false) => {
    const request = ++generation.current;
    setLoading(true);
    try {
      const result = await api(force ? "/orca?force=1" : "/orca");
      if (generation.current === request) {
        setData(result);
        setError("");
      }
      return result;
    } catch (e) {
      if (generation.current === request) setError(e.message);
      return null;
    } finally {
      if (generation.current === request) setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh(false);
    return () => { generation.current++; };
  }, [refresh, vmRunning, ready]);

  useEffect(() => {
    if (!job || job.running || !job.id) return;
    if (lastJobId.current === job.id) return;
    if (
      job.kind === "orca-install" ||
      job.kind === "orca-uninstall" ||
      job.kind === "environment-tools" ||
      job.kind === "start" ||
      job.kind === "stop"
    ) {
      lastJobId.current = job.id;
      void refresh(true);
    }
  }, [job, refresh]);

  const saveConfig = useCallback(async (patch) => {
    const result = await api("/orca/config", patch);
    return applyPayload(result);
  }, [applyPayload]);

  const install = useCallback(async (version = "match_desktop") => {
    return await api("/orca/install", { version });
  }, []);

  const uninstall = useCallback(async ({ purgeData = false, removePaired = false } = {}) => {
    return await api("/orca/uninstall", { purge_data: Boolean(purgeData), remove_paired: Boolean(removePaired) });
  }, []);

  const start = useCallback(async () => {
    const result = await api("/orca/start", {});
    return applyPayload(result);
  }, [applyPayload]);

  const stop = useCallback(async () => {
    const result = await api("/orca/stop", {});
    return applyPayload(result);
  }, [applyPayload]);

  const restart = useCallback(async () => {
    const result = await api("/orca/restart", {});
    return applyPayload(result);
  }, [applyPayload]);

  const resetIdentity = useCallback(async () => {
    const result = await api("/orca/reset-identity", {});
    return applyPayload(result);
  }, [applyPayload]);

  const open = useCallback(async (options = {}) => {
    const result = await api("/orca/open", options);
    if (result && typeof result === "object") {
      setData((prev) => (prev ? {
        ...prev,
        running: true,
        tunnel_active: true,
        tunnel_port: result.tunnel_port || prev.tunnel_port,
        tunnel_endpoint: result.endpoint || prev.tunnel_endpoint,
        paired: Boolean(result.paired),
        paired_name: result.environment_name || prev.paired_name,
        error: "",
      } : prev));
      setError("");
    }
    return result;
  }, []);

  const pair = useCallback(async (options = {}) => {
    return await api("/orca/pair", options);
  }, []);

  const registerCodexAccount = useCallback(async accountID => {
    const result = await api("/orca/register_codex_account", { account_id: accountID });
    await refresh();
    return result;
  }, [refresh]);

  const syncProjects = useCallback(async () => {
    return await api("/orca/sync-projects", {});
  }, []);

  return {
    data,
    enabled: Boolean(data?.enabled),
    guest_port: data?.guest_port || 16768,
    host_tunnel_port: data?.host_tunnel_port || 0,
    bind_mode: data?.bind_mode || "loopback",
    server_name: data?.server_name || "",
    auto_sync_projects: data?.auto_sync_projects !== false,
    terminal_memory_max: data?.terminal_memory_max || "",
    account_bindings: data?.account_bindings || {},
    installed: Boolean(data?.installed),
    version: data?.version || "",
    running: Boolean(data?.running),
    bind_ip: data?.bind_ip || "",
    tailscale_ip: data?.tailscale_ip || "",
    has_rollback: Boolean(data?.has_rollback),
    tunnel_active: Boolean(data?.tunnel_active),
    tunnel_port: data?.tunnel_port || 0,
    tunnel_endpoint: data?.tunnel_endpoint || "",
    paired: Boolean(data?.paired),
    paired_name: data?.paired_name || "",
    local_desktop: data?.local_desktop || { installed: false },
    can_auto_pair: Boolean(data?.can_auto_pair),
    download_url: data?.download_url || "https://orca.so",
    active_bindings: data?.active_bindings || {},
    supports_managed: data?.supports_managed,
    is_macos_guest: Boolean(data?.is_macos_guest),
    serverError: data?.error || "",
    loading,
    error,
    refresh,
    saveConfig,
    install,
    uninstall,
    start,
    stop,
    restart,
    resetIdentity,
    open,
    pair,
    registerCodexAccount,
    syncProjects,
  };
}
