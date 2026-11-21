import React, { useEffect, useState } from "react";
import { api } from "./api";
import { SafetyMessage } from "./safety-message";
import { automaticApply, resourceAllocationView } from "./resource-allocation.mjs";

const gib = (bytes) => `${(bytes / 1073741824).toFixed(1)} GiB`;
const allocationFrom = (data) => ({ automatic: data.automatic, memory_gib: data.memory_gib, cpus: data.cpus });

export function ResourceSettings({ status, onConfirm, onDirtyChange }) {
  const [data, setData] = useState(null);
  const [error, setError] = useState("");
  const [readError, setReadError] = useState("");
  const [starting, setStarting] = useState(false);
  const [draft, setDraft] = useState(null);
  const [saved, setSaved] = useState(false);
  useEffect(() => {
    if (data && data.runtime !== "container") setDraft((current) => current || allocationFrom(data));
  }, [data]);
  const save = async (event) => {
    event.preventDefault();
    setStarting(true);
    setSaved(false);
    try {
      await api("/environment/resources", { allocation: draft });
      const next = await api("/environment/resources");
      setData(next);
      setDraft(allocationFrom(next));
      setSaved(true);
      setError("");
    } catch (e) { setError(e.message); }
    finally { setStarting(false); }
  };
  useEffect(() => {
    let live = true;
    let timer;
    const refresh = async () => {
      try {
        const next = await api("/environment/resources");
        if (live) { setData(next); setReadError(""); }
      } catch (e) { if (live) setReadError(e.message); }
      if (live) timer = setTimeout(refresh, 4000);
    };
    refresh();
    return () => { live = false; clearTimeout(timer); };
  }, [status.vm_running, status.job?.running]);
  const apply = async () => {
    if (!(await onConfirm({
      title: "重启并应用已保存的资源配置？",
      body: "正在运行的命令会结束，浏览器和编辑器会重启。请先保存内容；项目文件和登录数据保留。手动暂停的网络仍保持暂停。",
      confirm: "重启并应用",
    }))) return;
    setStarting(true);
    try { await api("/environment/resources", {}); setError(""); }
    catch (e) { setError(e.message); }
    finally { setStarting(false); }
  };
  const observation = data?.observation;
  const guest = observation?.guest;
  const macOS = data?.runtime === "vz-macos" || data?.runtime === "qemu-macos";
  const fresh = observation && Date.now() - Date.parse(observation.observed_at) < 10000;
  const resourceError = error || readError || data?.resource_error;
  const resourcesVerified = data?.resources_verified === true && !resourceError;
  const busy = starting || status.job?.running;
  const dirty = draft && data && (draft.automatic !== data.automatic || (!draft.automatic && (draft.memory_gib !== data.memory_gib || draft.cpus !== data.cpus)));
  useEffect(() => {
    onDirtyChange?.(Boolean(dirty));
    return () => onDirtyChange?.(false);
  }, [dirty, onDirtyChange]);
  const edit = (key, value) => {
    setDraft((current) => ({ ...current, automatic: false, [key]: value === "" ? "" : Number(value) }));
    setSaved(false);
  };
  const reset = () => {
    setDraft({ automatic: true, memory_gib: data.recommended_memory_bytes / 1073741824, cpus: data.recommended_cpus });
    setSaved(false);
    setError("");
  };
  const view = resourceAllocationView(data);
  // One request from the allocation notice or advice; a running environment
  // restarts with it (apply), so ask first, as the restart below does.
  const post = async (body, restart) => {
    if (restart && !(await onConfirm({
      title: "改为自动分配并重启环境？",
      body: "正在运行的命令会结束，浏览器和编辑器会重启。请先保存内容；项目文件和登录数据保留。手动暂停的网络仍保持暂停。",
      confirm: "改为自动并重启",
    }))) return;
    setStarting(true);
    try {
      await api("/environment/resources", body);
      const next = await api("/environment/resources");
      setData(next);
      setDraft(allocationFrom(next));
      setError("");
    } catch (e) { setError(e.message); }
    finally { setStarting(false); }
  };
  const manual = () => {
    setDraft((current) => ({ ...current, automatic: false,
      memory_gib: Math.min(Math.max(current.memory_gib, view.range?.memory[0] || 2), view.range?.memory[1] || current.memory_gib),
      cpus: Math.min(Math.max(current.cpus, view.range?.cpus[0] || 1), view.range?.cpus[1] || current.cpus) }));
    setSaved(false);
  };
  if (data?.runtime === "container") return <section className="environment-versions" aria-label="计算资源">
    <h4>服务器容器资源上限</h4>
    <p className="field-help">CPU、内存与进程数量由服务器上的容器资源组限制。</p>
    <div className="setting-summary-row"><span>资源限制核验</span><strong>{data.running ? resourcesVerified ? "已核验" : "未通过核验" : "环境未运行"}</strong></div>
    <div className="setting-summary-row"><span>容器内存上限</span><strong>{data.running ? resourcesVerified ? gib(data.configured_memory_bytes) : "未核验" : gib(data.recommended_memory_bytes)}</strong></div>
    <div className="setting-summary-row"><span>容器 CPU 上限</span><strong>{data.running ? resourcesVerified ? `${data.configured_cpus} 个逻辑核心` : "未核验" : `${data.recommended_cpus} 个逻辑核心`}</strong></div>
    <div className="setting-summary-row"><span>进程与线程总数上限</span><strong>{data.running ? resourcesVerified ? data.pids_limit : "未核验" : "运行后核验"}</strong></div>
    <p className="field-help">{data.running ? resourcesVerified ? "已从服务器核验容器所属资源组的实际限制。实时内存与 CPU 用量尚未提供。" : "无法确认运行中容器的实际资源限制，请查看下方错误或等待重新核验。" : "环境尚未运行，当前显示部署配置中的资源上限。"}</p>
    {resourceError && <p role="alert" className="error-message"><SafetyMessage message={resourceError} /></p>}
  </section>;
  if (view.kind !== "older" && view.kind !== "unsupported") {
    const live = view.live;
    return <section className="environment-versions resource-settings resource-automatic" aria-label="计算资源" data-allocation={view.kind}>
      <h4>计算资源</h4>
      {view.notice && <div className="resource-notice nudge-card" role="status">
        <p>{view.notice.message}</p>
        {(view.notice.offer || view.notice.keep || view.notice.restart) && <div className="resource-notice-actions">
          {view.notice.offer && <button type="button" className="clay resource-offer" disabled={busy}
            onClick={() => post(view.notice.offer.body, view.notice.offer.restart && data.running)}>{view.notice.offer.label}</button>}
          {view.notice.restart && <button type="button" className="clay resource-offer" disabled={busy} onClick={apply}>重启并应用已保存配置</button>}
          {view.notice.keep && <button type="button" className="text-button" disabled={busy} onClick={() => post(view.notice.keep.body, false)}>{view.notice.keep.label}</button>}
        </div>}
      </div>}
      {draft ? <form className="resource-allocation" onSubmit={save}>
        <div className="choice-list resource-modes" role="radiogroup" aria-label="分配方式">
          <label className={`choice ${draft.automatic ? "checked" : ""}`}>
            <input type="radio" name="resource-mode" disabled={busy} checked={draft.automatic} onChange={reset} />
            <span>
              <strong>自动（推荐）<span className="resource-mode-figure">{view.automatic.label}</span></strong>
              <small>跟随这台机器的配置：环境需要时自动增加内存，内存紧张时先限制浏览器，保住会话。</small>
            </span>
          </label>
          <label className={`choice ${draft.automatic ? "" : "checked"}`}>
            <input type="radio" name="resource-mode" disabled={busy} checked={!draft.automatic} onChange={manual} />
            <span>
              <strong>手动<span className="resource-mode-figure">{draft.automatic ? `可调 ${view.range.memory[0]}–${view.range.memory[1]} GB、${view.range.cpus[0]}–${view.range.cpus[1]} 核` : `内存 ${draft.memory_gib || "—"} GB · CPU ${draft.cpus || "—"} 核`}</span></strong>
              <small>固定上限，不随机器变化。</small>
            </span>
          </label>
        </div>
        {!draft.automatic && <fieldset disabled={busy} className="resource-fields resource-sliders">
          <legend className="sr-only">资源配置</legend>
          <div className="resource-field">
            <span>内存上限</span>
            <span className="resource-slider-row">
              <input type="range" aria-hidden="true" tabIndex={-1} min={view.range.memory[0]} max={view.range.memory[1]} step="1"
                value={draft.memory_gib === "" ? view.range.memory[0] : draft.memory_gib} onChange={(e) => edit("memory_gib", e.target.value)} />
              <span className="resource-input"><input aria-label="内存上限（GiB）" aria-describedby="resource-memory-help" type="number" min={view.range.memory[0]} max={view.range.memory[1]} step="1" required value={draft.memory_gib} onChange={(e) => edit("memory_gib", e.target.value)} /><span aria-hidden="true">GB</span></span>
            </span>
            <small id="resource-memory-help">可调 {view.range.memory[0]}–{view.range.memory[1]} GB · 自动为 {view.automatic.memoryGib} GB</small>
          </div>
          <div className="resource-field">
            <span>CPU 上限</span>
            <span className="resource-slider-row">
              <input type="range" aria-hidden="true" tabIndex={-1} min={view.range.cpus[0]} max={view.range.cpus[1]} step="1"
                value={draft.cpus === "" ? view.range.cpus[0] : draft.cpus} onChange={(e) => edit("cpus", e.target.value)} />
              <span className="resource-input"><input aria-label="CPU 逻辑核心数" aria-describedby="resource-cpu-help" type="number" min={view.range.cpus[0]} max={view.range.cpus[1]} step="1" required value={draft.cpus} onChange={(e) => edit("cpus", e.target.value)} /><span aria-hidden="true">核</span></span>
            </span>
            <small id="resource-cpu-help">可调 {view.range.cpus[0]}–{view.range.cpus[1]} 核 · 自动为 {view.automatic.cpus} 核</small>
          </div>
        </fieldset>}
        <div className="resource-actions">
          <span className="resource-mode">{draft.automatic ? "自动分配" : "手动分配"}{dirty ? " · 未保存" : ""}</span>
          <button type="submit" disabled={busy || !dirty}>{starting ? "正在保存…" : "保存资源配置"}</button>
        </div>
        {(saved || dirty) && <p role={saved ? "status" : undefined} className="field-help resource-save-note">{saved ? "已保存，" : "保存后，"}{data.running ? "重启环境后生效。" : "下次启动环境时生效。"}</p>}
      </form> : null}
      {view.advice && <div className="resource-advice" role="status">
        <p>{view.advice.message}</p>
        {view.advice.action === "automatic" && <button type="button" className="text-button" disabled={busy} onClick={() => post(automaticApply, data.running)}>改为自动</button>}
        {view.advice.action === "restart" && <button type="button" className="text-button" disabled={busy} onClick={apply}>重启并应用已保存配置</button>}
      </div>}
      {view.restart && <div className="resource-advice resource-restart">
        <p>需要重启环境后生效。</p>
        <button type="button" className="text-button" disabled={busy} onClick={apply}>重启并应用已保存配置</button>
      </div>}
      {live && <div className="resource-live" aria-label="当前运行">
        <span className="resource-live-state" data-tone={live.state.tone}>{live.state.label}</span>
        {live.items.map((item) => <span key={item.label} className="resource-live-figure"><small>{item.label}</small>{item.value}</span>)}
      </div>}
      {live && observation.guest?.oom_kills > 0 && <p className="field-help" role="status">本次环境启动后，系统因内存不足终止过 {observation.guest.oom_kills} 个进程。请降低重任务的并行数量。</p>}
      {(resourceError || observation?.error) && <p role="alert" className="error-message"><SafetyMessage message={resourceError || observation.error} /></p>}
      {data.running && <details className="resource-observation-note"><summary>资源统计说明</summary><p className="field-help">{live?.summary ? live.summary + " " : ""}内存用量不含可回收缓存；这些数值反映环境内部状态，主机系统监视器还会统计虚拟化开销。</p></details>}
    </section>;
  }
  return <section className="environment-versions resource-settings" aria-label="计算资源">
    <h4>计算资源</h4>
    <p className="field-help resource-intro">默认使用主机一半的资源，可在下方调整。{macOS ? "macOS 客机使用固定的启动内存容量：运行中不会把内存归还主机，主机活动监视器里的占用会逐渐接近所设上限，这不是内存泄漏；需要给主机留出更多内存时请调低上限并重启客机。" : "内存按需使用，持续空闲后逐步归还主机。"}CPU 按实际任务使用。</p>
    {draft ? <form className="resource-allocation" onSubmit={save}>
      <fieldset disabled={busy} className="resource-fields">
        <legend className="sr-only">资源配置</legend>
        <label><span>内存{macOS ? "容量" : "上限"}</span>
          <span className="resource-input"><input aria-label="内存上限（GiB）" aria-describedby="resource-memory-help" type="number" min={data.min_memory_gib || 2} max={data.max_memory_gib} step="1" required value={draft.memory_gib} onChange={(e) => edit("memory_gib", e.target.value)} /><span aria-hidden="true">GiB</span></span>
          <small id="resource-memory-help">默认 {gib(data.recommended_memory_bytes)} · 可调 {data.min_memory_gib || 2}–{data.max_memory_gib} GiB</small>
        </label>
        <label><span>CPU 上限</span>
          <span className="resource-input"><input aria-label="CPU 逻辑核心数" aria-describedby="resource-cpu-help" type="number" min={data.min_cpus || 1} max={data.max_cpus} step="1" required value={draft.cpus} onChange={(e) => edit("cpus", e.target.value)} /><span aria-hidden="true">逻辑核心</span></span>
          <small id="resource-cpu-help">默认 {data.recommended_cpus} 个 · 可调 {data.min_cpus || 1}–{data.max_cpus} 个</small>
        </label>
      </fieldset>
      <div className="resource-actions">
        <span className="resource-mode">{draft.automatic ? "默认配置" : "自定义配置"}{dirty ? " · 未保存" : ""}</span>
        <button className="text-button" type="button" disabled={busy || draft.automatic} onClick={reset}>恢复默认</button>
        <button type="submit" disabled={busy || !dirty}>{starting ? "正在保存…" : "保存资源配置"}</button>
      </div>
      <p role={saved ? "status" : undefined} className="field-help resource-save-note">{saved ? "已保存，" : "保存后，"}{data.running ? "重启环境后生效。" : "下次启动环境时生效。"}</p>
    </form> : !resourceError && <p role="status" className="field-help">正在读取资源配置…</p>}
    {macOS && data.minimum_requirements?.source === "existing-configuration" && <p className="field-help resource-minimum-note">当前最低值沿用已有启动配置。尚未从原恢复镜像读回 Apple 的最低要求。</p>}
    {data?.running && <div className="resource-runtime">
      <div className="resource-runtime-heading"><h5>当前运行</h5>{data.restart_required && <button className="text-button" disabled={busy} onClick={apply}>重启并应用已保存配置</button>}</div>
      <div className="setting-summary-row"><span>本次启动的内存上限</span><strong>{gib(data.configured_memory_bytes)}</strong></div>
      <div className="setting-summary-row"><span>本次启动的 CPU 上限</span><strong>{data.configured_cpus} 个逻辑核心</strong></div>
      <div className="setting-summary-row"><span>{macOS ? "环境内存用量（估算）" : "环境内存用量"}</span><strong>{fresh && guest ? gib(guest.total_bytes - guest.available_bytes) : "等待采样"}</strong></div>
      <div className="setting-summary-row"><span>{macOS ? "客机物理内存容量" : "环境当前可用内存总量"}</span><strong>{fresh && guest ? gib(guest.total_bytes) : "—"}</strong></div>
      <div className="setting-summary-row"><span>CPU 使用率</span><strong>{fresh && Number.isFinite(observation.cpu_percent) ? `${observation.cpu_percent.toFixed(0)}% / ${observation.cpus} 个核心` : "等待采样"}</strong></div>
      {fresh && guest?.oom_kills > 0 && <p className="field-help" role="status">本次环境启动后，系统因内存不足终止过 {guest.oom_kills} 个进程。请降低重任务的并行数量，或使用内存更多的主机。</p>}
      {(observation?.pressure || observation?.host_pressure) && <p className="field-help" role="status">{macOS ? "客机内存压力升高，请减少并行任务，或保存工作后调整内存上限并重启。" : observation.host_pressure ? "主机内存紧张，暂缓扩大分配。" : "环境内存紧张，正在上限内增加可用内存。达到上限后请减少同时运行的任务。"}</p>}
    </div>}
    {(resourceError || observation?.error) && <p role="alert" className="error-message"><SafetyMessage message={resourceError || observation.error} /></p>}
    {data?.running && <details className="resource-observation-note"><summary>资源统计说明</summary><p className="field-help">{macOS ? "内存估算使用客机物理容量减去空闲与非活跃页，压缩页计入用量。CPU 来自连续两次 Mach 计数采样。OOM 终止计数尚未提供。主机系统监视器还会统计虚拟化开销。" : data.runtime === "hyperv" ? "内存与 CPU 用量来自 Windows 客机的系统观测。Hyper-V 会在设置的内存上下限内分配和回收内存；主机系统监视器还会统计虚拟化开销。" : "内存用量不含可回收缓存；这些数值反映环境内部状态，主机系统监视器还会统计虚拟化开销。"}</p></details>}
  </section>;
}
