// Whether 设置 shows the 其他客户端 row, kept apart from the section's own logic
// (other-clients-state.mjs) so that opening the settings loads only this: a few
// lines, not the rows, dialogs and job handling a person who never adds
// anything has no use for.

export const AGENTS_PATH = "/agents";

const isObject = (value) => value !== null && typeof value === "object" && !Array.isArray(value);

/** The entry list of a GET /api/agents answer (agents, entries or a bare list), or null. */
export const entriesOf = (payload) =>
  Array.isArray(payload) ? payload : Array.isArray(payload?.agents) ? payload.agents : Array.isArray(payload?.entries) ? payload.entries : null;

/**
 * An entry the section lists: not hidden (never offered in this build), not
 * Claude Code (it comes with the environment, nothing to add), and not a
 * withdrawn one that is not installed (nothing to offer or remove).
 */
export const isListed = (raw) =>
  isObject(raw) && typeof raw.id === "string" && raw.id.trim() !== "" && raw.status !== "hidden" && raw.with_environment !== true && (raw.status !== "withdrawn" || isObject(raw.present));

/** A server's agents_policy (design 7, T1): { allow?: [id], deny?: [id] }; nothing means everything. */
export function policyAllows(policy, id) {
  if (!isObject(policy)) return true;
  if (Array.isArray(policy.allow) && !policy.allow.includes(id)) return false;
  if (Array.isArray(policy.deny) && policy.deny.includes(id)) return false;
  return true;
}
/** A policy that allows nothing hides the row without asking the host. */
export const policyHidesRow = (policy) => isObject(policy) && Array.isArray(policy.allow) && policy.allow.length === 0;

/** The row shows when at least one entry is listed, allowed by the entry itself and by the server's policy. */
export function navRowVisible(payload, policy) {
  if (policyHidesRow(policy)) return false;
  return (entriesOf(payload) || []).some((raw) => isListed(raw) && raw.allowed !== false && policyAllows(policy, raw.id.trim()));
}
