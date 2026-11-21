import assert from "node:assert/strict";
import test from "node:test";
import { automaticApply, gb, liveFigures, resourceAllocationView } from "./resource-allocation.mjs";

const GiB = 2 ** 30;
const now = Date.parse("2026-11-21T10:00:00Z");
const observation = (extra = {}) => ({ observed_at: new Date(now - 2000).toISOString(), maximum_bytes: 6 * GiB, target_bytes: 4 * GiB, cpus: 5, cpu_percent: 23.4,
  guest: { total_bytes: 4 * GiB, available_bytes: 1.6 * GiB }, summary: "环境内存充足（分配 4 GB，需要 2.1 GB）。", ...extra });
const base = { running: true, memory_gib: 6, cpus: 5, max_memory_gib: 6, max_cpus: 5, recommended_memory_bytes: 6 * GiB, recommended_cpus: 5,
  configured_memory_bytes: 6 * GiB, configured_cpus: 5, restart_required: false, automatic_memory_gib: 6, automatic_cpus: 5, possible_memory_bytes: 6 * GiB };

test("gb rounds like the server's sentences", () => {
  assert.equal(gb(6 * GiB), "6 GB");
  assert.equal(gb(2.44 * GiB), "2.4 GB");
  assert.equal(gb(12.4 * GiB), "12 GB");
  assert.equal(gb(undefined), "—");
});

test("automatic: shows what it gives on this machine, nothing to act on", () => {
  const view = resourceAllocationView({ ...base, automatic: true, allocation_origin: "automatic", observation: observation() }, { now });
  assert.equal(view.kind, "automatic");
  assert.equal(view.automatic.label, "内存 6 GB · CPU 5 核");
  assert.deepEqual(view.range, { memory: [2, 6], cpus: [1, 5] });
  assert.equal(view.notice, null);
  assert.equal(view.advice, null);
  assert.equal(view.restart, false);
  assert.deepEqual(view.live.items.map((item) => item.label), ["在用", "当前分配", "上限", "CPU"]);
  assert.equal(view.live.items[0].value, "2.4 GB");
  assert.equal(view.live.state.tone, "green");
});

test("chosen by the user: manual, never nudged", () => {
  const view = resourceAllocationView({ ...base, automatic: false, allocation_origin: "user", memory_gib: 4, cpus: 2 }, { now });
  assert.equal(view.kind, "chosen");
  assert.equal(view.current.label, "内存 4 GB · CPU 2 核");
  assert.equal(view.notice, null);
  assert.equal(view.live, null, "no observation, no live row");
});

test("legacy default: the notice with the offer as its one action and a way to keep", () => {
  const notice = "内存 3 GB、CPU 3 核与旧版本的默认设置相同。改为自动（推荐）后，这台机器可以给到 6 GB、5 核，需要重启环境；如果这是你特意设的，可以保留。";
  const view = resourceAllocationView({ ...base, automatic: false, allocation_origin: "legacy_default", memory_gib: 3, cpus: 3, legacy_allocation: true,
    notice, offer: { automatic: true, memory_gib: 6, cpus: 5, restart_required: true } }, { now });
  assert.equal(view.kind, "legacy");
  assert.equal(view.notice.message, notice);
  assert.equal(view.notice.offer.label, "改为自动");
  assert.deepEqual(view.notice.offer.body, automaticApply);
  assert.equal(view.notice.offer.restart, true);
  assert.deepEqual(view.notice.keep.body, { allocation: { automatic: false, memory_gib: 3, cpus: 3 } });
  assert.equal(view.restart, false, "the offer already restarts");
});

test("automatic recalculated while running: restart needed, said once", () => {
  const view = resourceAllocationView({ ...base, automatic: true, allocation_origin: "automatic", restart_required: true, configured_memory_bytes: 3 * GiB,
    notice: "自动分配已按这台机器重新计算为内存 6 GB、CPU 5 核，重启环境后生效。", observation: observation() }, { now });
  assert.equal(view.notice.offer, null);
  assert.equal(view.notice.keep, null);
  assert.equal(view.notice.restart, true);
  assert.equal(view.restart, false);
});

test("saved manual change while running: plain restart line", () => {
  const view = resourceAllocationView({ ...base, automatic: false, allocation_origin: "user", memory_gib: 4, cpus: 3, restart_required: true }, { now });
  assert.equal(view.notice, null);
  assert.equal(view.restart, true);
});

test("short with advice: manual points to automatic, automatic to a restart, machine limit to nothing", () => {
  const short = observation({ short: true, limited_by: "maximum", target_bytes: 3 * GiB, maximum_bytes: 3 * GiB, capacity_bytes: 5 * GiB });
  const manual = resourceAllocationView({ ...base, automatic: false, allocation_origin: "user", memory_gib: 3, cpus: 2, observation: short,
    advice: { message: "环境内存已用满手动设置的 3 GB。改为自动（推荐）后可以调到 6 GB，需要重启环境。", memory_bytes: 6 * GiB, restart_required: true } }, { now });
  assert.equal(manual.advice.action, "automatic");
  assert.equal(manual.live.state.label, "内存紧张 · 已到上限");
  assert.ok(manual.live.items.some((item) => item.label === "机器可给"));
  const automatic = resourceAllocationView({ ...base, automatic: true, allocation_origin: "automatic", restart_required: true, observation: short,
    advice: { message: "可以把内存调到 6 GB，需要重启环境。", memory_bytes: 6 * GiB, restart_required: true } }, { now });
  assert.equal(automatic.advice.action, "restart");
  assert.equal(automatic.restart, false, "the advice carries the restart");
  const machine = resourceAllocationView({ ...base, automatic: true, allocation_origin: "automatic", observation: { ...short, limited_by: "machine" },
    advice: { message: "环境已用到这台机器能给的 6 GB。请关闭不用的浏览器标签页或会话，或换一台内存更大的机器。", restart_required: false } }, { now });
  assert.equal(machine.advice.action, null);
  assert.equal(machine.live.state.label, "内存紧张 · 机器没有更多");
});

test("notice and advice together: one action, never two", () => {
  const short = observation({ short: true, limited_by: "maximum", target_bytes: 3 * GiB, maximum_bytes: 3 * GiB });
  // The outage: an old default at its 3 GB maximum, short.
  const outage = resourceAllocationView({ ...base, automatic: false, allocation_origin: "legacy_default", memory_gib: 3, cpus: 3, legacy_allocation: true,
    notice: "内存 3 GB、CPU 3 核与旧版本的默认设置相同。…", offer: { automatic: true, memory_gib: 6, cpus: 5, restart_required: true }, observation: short,
    advice: { message: "环境内存已用满手动设置的 3 GB。改为自动（推荐）后可以调到 6 GB，需要重启环境。", memory_bytes: 6 * GiB, restart_required: true } }, { now });
  assert.equal(outage.notice.offer.label, "改为自动");
  assert.equal(outage.advice.message.startsWith("环境内存已用满"), true);
  assert.equal(outage.advice.action, null, "the notice's offer is the one 改为自动");
  // Automatic recalculated while running, and short at the old maximum.
  const recalculated = resourceAllocationView({ ...base, automatic: true, allocation_origin: "automatic", restart_required: true, observation: short,
    notice: "自动分配已按这台机器重新计算为内存 6 GB、CPU 5 核，重启环境后生效。",
    advice: { message: "可以把内存调到 6 GB，需要重启环境。", memory_bytes: 6 * GiB, restart_required: true } }, { now });
  assert.equal(recalculated.notice.restart, true);
  assert.equal(recalculated.advice.action, null, "the notice's restart is the one restart");
  assert.equal(recalculated.restart, false);
});

test("older server: none of the new fields keeps today's controls", () => {
  assert.equal(resourceAllocationView({ automatic: true, memory_gib: 12, cpus: 5, running: false, recommended_memory_bytes: 12 * GiB, recommended_cpus: 5, max_memory_gib: 12, max_cpus: 5 }).kind, "older");
  assert.equal(resourceAllocationView({ runtime: "container" }).kind, "unsupported");
  assert.equal(resourceAllocationView(null).kind, "unsupported");
});

test("a stale or failed sample gives no live row", () => {
  assert.equal(liveFigures({ running: true, observation: { ...observation(), observed_at: new Date(now - 60000).toISOString() } }, now), null);
  assert.equal(liveFigures({ running: true, observation: { ...observation(), error: "读取失败" } }, now), null);
  assert.equal(liveFigures({ running: false, observation: observation() }, now), null);
});
