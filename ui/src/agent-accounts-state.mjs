export function agentAccountsPath(agentId, { workspace = "", forceRefresh = false } = {}) {
  const params = new URLSearchParams();
  if (workspace) params.set("workspace", workspace);
  if (forceRefresh) params.set("refresh", "1");
  const qs = params.toString() ? `?${params.toString()}` : "";
  return `/agent-clis/${encodeURIComponent(agentId)}/accounts${qs}`;
}

// Monotonic request sequencer shared between load() and mutate() so an
// in-flight GET can never overwrite a newer POST mutation response.
export function createAgentAccountsSequencer() {
  let current = 0;
  return {
    begin() {
      current += 1;
      return current;
    },
    isCurrent(ticket) {
      return ticket === current;
    },
    invalidate() {
      current += 1;
    },
  };
}
