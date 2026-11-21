// The 计算资源 view state for 设置 → 环境与恢复 (internal/cage/resource-allocation.md §7).
// Pure: the server answer of GET /api/environment/resources in, what to show out.
// An older server sends none of the allocation fields; it keeps today's controls.
const GiB = 1073741824;

// "6 GB", "2.4 GB": the server's sentences say GB for GiB, so the view does too.
export function gb(bytes) {
  if (!Number.isFinite(bytes) || bytes < 0) return "—";
  const value = bytes / GiB;
  return (Number.isInteger(value) || value >= 10 ? Math.round(value) : Math.round(value * 10) / 10) + " GB";
}

const positive = (value) => Number.isFinite(value) && value > 0;

// The POST bodies the view can send.
export const automaticApply = Object.freeze({ allocation: Object.freeze({ automatic: true }), apply: true });
export const keepAllocation = (data) => ({ allocation: { automatic: false, memory_gib: data.memory_gib, cpus: data.cpus } });

export function resourceAllocationView(data, { now = Date.now() } = {}) {
  if (!data || data.runtime === "container") return { kind: "unsupported" };
  if (data.allocation_origin === undefined) return { kind: "older" };
  const automaticMemory = positive(data.automatic_memory_gib) ? data.automatic_memory_gib : data.recommended_memory_bytes / GiB;
  const automaticCpus = positive(data.automatic_cpus) ? data.automatic_cpus : data.recommended_cpus;
  const kind = data.automatic ? "automatic" : data.allocation_origin === "legacy_default" ? "legacy" : "chosen";
  const view = {
    kind,
    automatic: { memoryGib: automaticMemory, cpus: automaticCpus, label: `内存 ${gb(automaticMemory * GiB)} · CPU ${automaticCpus} 核` },
    current: { memoryGib: data.memory_gib, cpus: data.cpus, label: `内存 ${gb(data.memory_gib * GiB)} · CPU ${data.cpus} 核` },
    range: { memory: [data.min_memory_gib || 2, data.max_memory_gib], cpus: [data.min_cpus || 1, data.max_cpus] },
    notice: null,
    advice: null,
    restart: false,
    live: liveFigures(data, now),
  };
  // The server's own sentence; the offer is its one action.
  if (data.notice) {
    view.notice = {
      message: data.notice,
      offer: data.offer ? { label: "改为自动", body: automaticApply, restart: Boolean(data.offer.restart_required) } : null,
      // Saving the same values through settings marks them as chosen, which
      // is how the server stops offering (resources.go applyResources).
      keep: data.legacy_allocation && data.offer ? { label: "保留当前设置", body: keepAllocation(data) } : null,
      restart: !data.offer && Boolean(data.running && data.restart_required),
    };
  }
  const advice = data.advice || data.observation?.advice;
  if (advice?.message) {
    // Manual and short: the way out is automatic; automatic and short: a restart.
    let action = !advice.restart_required ? null : data.automatic ? "restart" : "automatic";
    // The notice may already carry that action (the outage case: an old
    // default at its maximum); then the advice is said, not offered twice.
    if ((action === "automatic" && view.notice?.offer) || (action === "restart" && view.notice?.restart)) action = null;
    view.advice = { message: advice.message, restart: Boolean(advice.restart_required), action };
  }
  // A plain "saved, restart to apply" when nothing above already says so.
  view.restart = Boolean(data.running && data.restart_required) && !view.notice?.restart && !view.notice?.offer && view.advice?.action !== "restart";
  return view;
}

// One quiet row of live figures, or null when the sample is missing or stale.
export function liveFigures(data, now = Date.now()) {
  const observation = data?.observation;
  if (!data?.running || !observation || observation.error) return null;
  const observed = Date.parse(observation.observed_at);
  if (!Number.isFinite(observed) || now - observed > 10000) return null;
  const guest = observation.guest;
  const items = [];
  if (guest && positive(guest.total_bytes)) items.push({ label: "在用", value: gb(guest.total_bytes - (guest.available_bytes || 0)) });
  if (positive(observation.target_bytes)) items.push({ label: "当前分配", value: gb(observation.target_bytes) });
  if (positive(observation.maximum_bytes)) items.push({ label: "上限", value: gb(observation.maximum_bytes) });
  if (observation.short && positive(observation.capacity_bytes)) items.push({ label: "机器可给", value: gb(observation.capacity_bytes) });
  if (Number.isFinite(observation.cpu_percent)) items.push({ label: "CPU", value: `${observation.cpu_percent.toFixed(0)}%` });
  const state = observation.short
    ? observation.limited_by === "machine" ? { tone: "amber", label: "内存紧张 · 机器没有更多" } : { tone: "amber", label: "内存紧张 · 已到上限" }
    : observation.pressure ? { tone: "amber", label: "内存紧张" } : { tone: "green", label: "内存充足" };
  return { items, state, summary: observation.summary || "" };
}
