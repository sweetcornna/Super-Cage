import { useEffect, useMemo, useState } from "react";
import { api } from "./api";

const providerNames = {
  claude: "Claude Code", codex: "Codex", antigravity: "Antigravity", opencode: "OpenCode", gemini: "Gemini CLI",
};
const defaultProviders = {
  orca: ["claude", "codex", "antigravity", "opencode"],
  t3code: ["claude", "codex", "opencode", "gemini"],
};

async function readAccounts(id, nativeCodexOnly, signal) {
  const read = path => api(path, undefined, undefined, {}, { signal });
  if (id === "claude") return { accounts: (await read("/claude-accounts")).accounts || [] };
  const agentPath = `/agent-clis/${encodeURIComponent(id)}/accounts?skip_detect=1`;
  if (id !== "codex") return { accounts: (await read(agentPath)).accounts || [] };
  let native = {};
  let warning = "";
  try {
    native = await read("/codex-accounts");
  } catch (error) {
    if (error.status !== 404) {
      if (nativeCodexOnly) throw error;
      warning = error.message;
    }
  }
  const nativeAccounts = native.guest_supported
    ? (native.accounts || []).map(account => ({ ...account, nativeCodex: true })) : [];
  if (nativeCodexOnly && nativeAccounts.length) return { accounts: nativeAccounts };
  try {
    const agentAccounts = (await read(agentPath)).accounts || [];
    return {
      accounts: [...nativeAccounts, ...agentAccounts.filter(account => !nativeAccounts.some(entry => entry.id === account.id))],
      warning,
    };
  } catch (error) {
    if (!nativeAccounts.length) throw error;
    return { accounts: nativeAccounts, warning: error.message };
  }
}

export function useServiceAccounts(agentsStore, bindings, { available, service }) {
  const [accounts, setAccounts] = useState(() => Object.create(null));
  const [errors, setErrors] = useState(() => Object.create(null));
  const [loading, setLoading] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const bindableAgents = useMemo(() => {
    const entries = new Map((agentsStore?.agents || []).map(entry => [entry.spec.id, entry]));
    const ids = new Set([...defaultProviders[service], ...Object.keys(bindings || {}).filter(id => /^[a-z0-9][a-z0-9_-]*$/.test(id))]);
    for (const [id, entry] of entries) {
      if (entry.runtime?.installed || entry.accounts?.total > 0) ids.add(id);
    }
    return [...ids].map(id => entries.get(id) || { spec: { id, name: Object.hasOwn(providerNames, id) ? providerNames[id] : id } });
  }, [agentsStore?.agents, bindings, service]);
  const bindableKey = bindableAgents.map(({ spec }) => spec.id).join(",");
  useEffect(() => {
    if (!available || !bindableKey) return;
    const controller = new AbortController();
    const ids = bindableKey.split(",");
    setLoading(true);
    void Promise.allSettled(ids.map(id => readAccounts(id, service === "orca", controller.signal))).then(results => {
      if (controller.signal.aborted) return;
      const next = Object.create(null);
      const failed = Object.create(null);
      results.forEach((result, index) => {
        const id = ids[index];
        if (result.status === "fulfilled") {
          next[id] = result.value.accounts.filter(account =>
            account.id && account.id !== "default" && account.enabled !== false && (account.status === "ready" || account.ready),
          );
          if (result.value.warning) failed[id] = result.value.warning;
        } else {
          failed[id] = result.reason.message;
        }
      });
      setAccounts(next);
      setErrors(failed);
      setLoading(false);
    });
    return () => controller.abort();
  }, [available, bindableKey, service, attempt]);
  return { bindableAgents, accounts, errors, loading, retry: () => setAttempt(value => value + 1) };
}
