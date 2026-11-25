// Shared idle-gating delay helpers for always-mounted workbench pollers.
// Returning null means "do not schedule another timer until visibility or
// state changes wake the poller".

export function statusPollDelay({ jobRunning = false, windowVisible = true } = {}) {
  if (jobRunning) return windowVisible ? 400 : 2000;
  return windowVisible ? 5000 : null;
}

export function accountPollDelay({ windowVisible = true } = {}) {
  return windowVisible ? 2500 : null;
}

export function desktopFilesPollDelay({ failures = 0, windowVisible = true, desktopVisible = false } = {}) {
  if (!windowVisible) return null;
  const base = desktopVisible ? 1500 : 10000;
  return Math.min(base * 2 ** Math.min(Math.max(0, failures), 4), 30000);
}

export function vaultPollActive({ ready = false, tab = "", browserVisible = true, windowVisible = true } = {}) {
  return Boolean(ready && tab && browserVisible && windowVisible);
}
