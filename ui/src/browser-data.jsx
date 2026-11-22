import React, { useEffect, useMemo, useRef, useState } from "react";
import { ArrowLeft, ChevronRight, Clock3, Cookie, ExternalLink, Globe2, KeyRound, LockKeyhole, Plus, RefreshCw, RotateCcw, Search, ShieldCheck, Trash2, X } from "lucide-react";
import { api } from "./api";
import { Modal } from "./modal";
import { InlineSelect } from "./inline-select";
import { EmptyState } from "./drawings";
import { BrowserCookies } from "./browser-cookies";
import { autoLine, builtInNote, categories, categoryMeta, formatBytes, formatCount, groupByDay, hostOf, isLoopbackSite, keptBy, keptCoverage, keptDomain, privateCategories, ranges, relativeVisit, resultLine, siteOfURL, sorts, timeLabel } from "./browser-data-state.mjs";
import "./browser-data.css";

// Browser data management (docs/guide/browser.md, design in the branch's
// notes): the clear dialog, a site's data (the former cookie manager), the
// address bar's site menu, reset, and the privacy section of the browser
// settings. Everything here is computed on demand while it is on screen.

// The launcher (browser-data-launcher.jsx, main chunk) answers these events.
// Importing it, or platform.mjs, from this chunk would split them out of the
// main chunk into files of their own on the first paint.
const openBrowserData = detail => document.dispatchEvent(new CustomEvent("cage-browser-data", { detail }));
const takePendingPrivacyView = () => { const request = { view: "" }; document.dispatchEvent(new CustomEvent("cage-browser-data-take", { detail: request })); return request.view; };
// platform.mjs shortcut("⌘⇧⌫"), spelled out.
const clearShortcut = /Mac/i.test(globalThis.navigator?.platform || "") ? "⌘⇧⌫" : "Ctrl+Shift+Delete";

const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const data = (input, signal) => api("/browser/data", input, undefined, {}, { signal });

let capability = null;
// Older servers (404), older guests ("update"), other guest systems
// ("platform") and a guest whose extension page is missing ("unavailable")
// keep the earlier screens. Anything else (not ready, busy, no answer) is an
// error to show and retry, not a reason to switch screens.
const legacyReasons = ["server", "update", "platform", "unavailable"];
const usesLegacy = value => Boolean(value && !value.available && legacyReasons.includes(value.reason));
async function loadCapability() {
  if (capability && Date.now() - capability.at < 60_000) return capability.value;
  let value;
  try { value = await data({ mode: "capabilities" }); }
  catch (error) { value = { available: false, reason: error.status === 404 ? "server" : "error", message: error.message }; }
  if (value.reason !== "error") capability = { value, at: Date.now() };
  return value;
}
function useCapability() {
  const [value, setValue] = useState(capability && Date.now() - capability.at < 60_000 ? capability.value : null);
  const [attempt, setAttempt] = useState(0);
  useEffect(() => { let live = true; loadCapability().then(next => { if (live) setValue(next); }); return () => { live = false; }; }, [attempt]);
  return [value, () => { setValue(null); setAttempt(attempt + 1); }];
}

function Unavailable({ value, onRetry, onClose }) {
  const body = <p className="error-message" role="alert">{value.message || "浏览器暂时没有响应。"}</p>;
  if (!onClose) return <div className="bd-unavailable">{body}<button className="compact" onClick={onRetry}><RefreshCw size={14} />重试</button></div>;
  return <Modal title="浏览数据暂时不可用" onClose={onClose} footer={<><button onClick={onClose}>关闭</button><button className="primary" onClick={onRetry}>重试</button></>}>
    <div className="modal-body" data-browser-dialog>{body}</div>
  </Modal>;
}

async function runJob(input, onProgress) {
  let { job } = await data(input);
  onProgress?.(job);
  while (job.state === "running") {
    await delay(job.elapsed > 2000 ? 400 : 150);
    ({ job } = await data({ mode: "job", id: job.id }));
    onProgress?.(job);
  }
  if (job.state === "failed") throw new Error(job.error || "浏览数据未能清除，请重试");
  return job.result;
}

const defaultSettings = { defaults: { range: "day", types: ["history", "site", "cache"] }, keep: [], auto: { enabled: false, trigger: "stop", days: 7, types: ["history", "site", "cache"] } };
// An older server (404) has no settings and shows its defaults. A settings
// file the host cannot read is an error to show with 重试: the kept sites are
// never taken for none.
async function loadSettings() {
  try { return await api("/browser/data-settings"); }
  catch (error) { if (error.status === 404) return defaultSettings; throw error; }
}
function useSettings() {
  const [settings, setSettings] = useState(null), [error, setError] = useState(""), [attempt, setAttempt] = useState(0);
  useEffect(() => {
    let live = true;
    setError("");
    loadSettings().then(value => { if (live) setSettings(value); }, failure => { if (live) setError(failure.message || "浏览数据设置暂时读不出来，请重试"); });
    return () => { live = false; };
  }, [attempt]);
  return { settings, setSettings, error, retry: () => setAttempt(attempt + 1) };
}
const saveSettings = input => api("/browser/data-settings", input);
// A message from the host, ended as a sentence before more text follows.
const sentence = text => String(text || "").replace(/[。.！!]?\s*$/, "。");

const privateOpen = tabs => (tabs || []).some(tab => tab.incognito && !tab.login);

// ── The clear dialog ─────────────────────────────────────────
function ClearDialog({ tab, tabs, preset, onClose, onInternal, onVault }) {
  const canPrivate = privateOpen(tabs);
  const [session, setSession] = useState(tab?.incognito && canPrivate ? "private" : "normal");
  const { settings, error: settingsError, retry: retrySettings } = useSettings();
  const [range, setRange] = useState(preset?.range || "day");
  const [types, setTypes] = useState(() => new Set(preset?.types || defaultSettings.defaults.types));
  const [keepEnabled, setKeepEnabled] = useState(true);
  const [summary, setSummary] = useState(null);
  const [phase, setPhase] = useState("edit");
  const [step, setStep] = useState(null);
  const [result, setResult] = useState(null);
  const [error, setError] = useState("");
  useEffect(() => {
    if (settings && !preset) { setRange(settings.defaults.range); setTypes(new Set(settings.defaults.types)); }
  }, [settings]);
  const isPrivate = session === "private";
  const effectiveRange = isPrivate ? "all" : range;
  useEffect(() => {
    if (!settings && !settingsError) return;
    const controller = new AbortController();
    setSummary(null);
    data({ mode: "summary", session, range: effectiveRange }, controller.signal).then(setSummary)
      .catch(failure => { if (!controller.signal.aborted) setSummary({ error: failure.message }); });
    return () => controller.abort();
  }, [settings, settingsError, session, effectiveRange]);
  const visible = categories.filter(item => !isPrivate || privateCategories.includes(item.id));
  const chosen = visible.filter(item => types.has(item.id)).map(item => item.id);
  const toggle = id => setTypes(previous => { const next = new Set(previous); next.has(id) ? next.delete(id) : next.add(id); return next; });
  const clear = async () => {
    setPhase("clearing"); setError("");
    try {
      // A preset (history's 按时间清除…) is a one-off and leaves the saved defaults alone.
      const value = await runJob({ mode: "clear", session, range: effectiveRange, types: chosen, ...(keepEnabled ? {} : { protect: "none" }), remember: !isPrivate && !preset }, setStep);
      setResult(value); setPhase("done");
    } catch (failure) { setError(failure.message); setPhase("edit"); }
  };
  const kept = settings?.keep?.length || 0;
  // Site data and the cache skip the kept sites; without the list such a clear waits.
  const needsKept = !isPrivate && keepEnabled && (chosen.includes("site") || chosen.includes("cache"));
  const blocked = needsKept && !settings;
  const footer = phase === "done"
    ? <button className="primary" onClick={onClose} autoFocus>完成</button>
    : <>
      <span className="save-note" role={error ? "alert" : "status"}>{error || (phase === "clearing" ? `正在清除…${step?.steps ? ` ${Math.min(step.step, step.steps)}/${step.steps}` : ""}` : summary?.error || "")}</span>
      <button onClick={onClose} disabled={phase === "clearing"}>取消</button>
      <button className="danger-fill" onClick={clear} disabled={phase === "clearing" || !chosen.length || blocked || (!settings && !settingsError)}>{phase === "clearing" ? "正在清除…" : "清除数据"}</button>
    </>;
  return <Modal title="清除浏览数据" onClose={phase === "clearing" ? undefined : onClose} footer={footer}>
    <div className="modal-body bd-clear" data-browser-dialog aria-busy={phase === "clearing"}>
      {phase === "clearing" && <div className="progress-line" />}
      {phase === "done" ? <div className="bd-result" role="status">
        <ShieldCheck size={20} />
        <p>{resultLine(result)}</p>
        {isPrivate ? <small>无痕会话的其他内容在关闭全部无痕标签后清理。</small> : <small>需要时可在 <button className="text-button" onClick={() => { onClose(); openBrowserData({ view: "privacy" }); }}>浏览器设置 → 隐私与网站数据</button> 中管理保持登录的网站和自动清理。</small>}
      </div> : <>
        <div className="bd-clear-scope">
          <label className="bd-field"><span>时间范围</span>
            <InlineSelect aria-label="时间范围" value={effectiveRange} disabled={isPrivate || phase === "clearing"} onValueChange={setRange}>
              {ranges.map(item => <option key={item.id} value={item.id}>{item.label}</option>)}
            </InlineSelect>
          </label>
          {(canPrivate || isPrivate) && <label className="bd-field"><span>浏览会话</span>
            <InlineSelect aria-label="浏览会话" value={session} disabled={phase === "clearing"} onValueChange={setSession}>
              <option value="normal">普通会话</option><option value="private" disabled={!canPrivate}>无痕会话</option>
            </InlineSelect>
          </label>}
        </div>
        {isPrivate && <p className="bd-note">无痕会话只在内存中保存，按整个会话清除；它没有浏览记录。</p>}
        {settingsError && !isPrivate && <div className="bd-unavailable"><p className="error-message" role="alert">{sentence(settingsError)}{needsKept ? "读出来之前不清除 Cookie、网站数据和缓存，以免清掉保持登录的网站。" : ""}</p>
          <button className="compact" onClick={retrySettings}><RefreshCw size={14} />重试</button></div>}
        <div className="choice-list bd-categories" role="group" aria-label="要清除的内容">
          {visible.map(item => <label key={item.id} className={"choice" + (types.has(item.id) ? " checked" : "")}>
            <input type="checkbox" checked={types.has(item.id)} disabled={phase === "clearing"} onChange={() => toggle(item.id)} />
            <span><strong>{item.title}<em className="bd-meta">{categoryMeta(item.id, summary?.error ? {} : summary, effectiveRange)}</em></strong><small>{item.detail}</small></span>
          </label>)}
        </div>
        {!isPrivate && (types.has("site") || types.has("cache")) && <label className="check-line bd-keep">
          <input type="checkbox" checked={keepEnabled} disabled={phase === "clearing"} onChange={event => setKeepEnabled(event.target.checked)} />
          <span>跳过{kept ? ` ${kept} 个` : ""}保持登录的网站和 Super Cage 内置页面（文件编辑器、Claude Science）</span>
          <button type="button" className="text-button" onClick={() => { onClose(); openBrowserData({ view: "privacy", section: "kept" }); }}>管理</button>
        </label>}
        <ul className="bd-aside">
          <li><LockKeyhole size={14} /><span>网站权限不在这里清除。<button type="button" className="text-button" onClick={() => { onClose(); onInternal?.("permissions"); }}>查看网站权限</button></span></li>
          <li><KeyRound size={14} /><span>已保存的密码属于 Super Cage 密码保险箱。<button type="button" className="text-button" onClick={() => { onClose(); onVault?.(); }}>在设置中管理</button></span></li>
        </ul>
      </>}
    </div>
  </Modal>;
}

const loopback = ["localhost", "127.0.0.1"];
const loopbackWarning = "本机地址包含 Super Cage 内置页面，清除后文件编辑器用浏览器菜单的“恢复编辑器”重新进入，Claude Science 需要重新打开。";

// ── One site's data: storage, cookies (the former cookie manager) ─────────
function SiteDetail({ site, session = "normal", settings, onSettings, onBack, onCleared, compact = false }) {
  const [page, setPage] = useState(0), [query, setQuery] = useState(""), [revision, setRevision] = useState(0);
  const [detail, setDetail] = useState(null), [error, setError] = useState(""), [value, setValue] = useState(null);
  const [busy, setBusy] = useState(false), [confirm, setConfirm] = useState(false), [notice, setNotice] = useState("");
  useEffect(() => {
    const controller = new AbortController();
    setError("");
    data({ mode: "site", session, site, page, query }, controller.signal).then(setDetail)
      .catch(failure => { if (!controller.signal.aborted) setError(failure.message); });
    return () => controller.abort();
  }, [site, session, page, query, revision]);
  const kept = keptBy(site, settings?.keep);
  const cookie = async (key, action) => {
    setBusy(true); setError("");
    try {
      const result = await data({ mode: "cookie", session, site, key, action });
      if (action === "value") setValue({ key, value: result.value });
      else { setValue(null); setRevision(revision + 1); }
    } catch (failure) { setError(failure.message); } finally { setBusy(false); }
  };
  const clearSite = async () => {
    setBusy(true); setError("");
    try {
      const result = await runJob({ mode: "clear-sites", session, sites: [site] });
      setConfirm(false); setNotice(resultLine(result)); setRevision(revision + 1); onCleared?.(result);
    } catch (failure) { setError(failure.message); } finally { setBusy(false); }
  };
  const toggleKeep = async () => {
    setBusy(true);
    try { onSettings?.(await saveSettings({ action: kept ? "keep-remove" : "keep-add", domain: kept ? kept.domain : site })); }
    catch (failure) { setError(failure.message); } finally { setBusy(false); }
  };
  const storage = detail?.origins?.reduce((sum, item) => sum + item.bytes, 0) || 0;
  return <section className={"bd-site" + (compact ? " compact" : "")} aria-label={`${site} 的网站数据`}>
    <header className="bd-view-head">
      {onBack && <button className="icon-button" aria-label="返回网站列表" onClick={onBack}><ArrowLeft size={16} /></button>}
      <div className="bd-view-title"><h3>{site}</h3>
        <p>{session === "private" ? "无痕会话 · " : ""}{detail ? `${formatCount(detail.total)} 个 Cookie · 存储 ${formatBytes(storage)}` : "正在读取…"}</p></div>
      {session === "normal" && <button className={kept ? "compact bd-kept-on" : "compact"} aria-pressed={Boolean(kept)} disabled={busy} onClick={toggleKeep}>
        <ShieldCheck size={15} />{kept ? "已保持登录" : "保持登录"}</button>}
      <button className="compact danger" disabled={busy || !detail} onClick={() => setConfirm(!confirm)}><Trash2 size={15} />清除此网站数据</button>
    </header>
    {confirm && <div className="bd-confirm" role="alert">
      <span>清除 {site} 的全部 Cookie 和网站存储？{session === "private" ? "只影响无痕会话。" : ""}此网站需要重新登录，打开的网页会重新加载。{loopback.includes(site) ? loopbackWarning : ""}</span>
      <button className="danger-fill compact" disabled={busy} onClick={clearSite}>确认清除</button>
      <button className="compact" disabled={busy} onClick={() => setConfirm(false)}>取消</button>
    </div>}
    {notice && <p className="bd-notice" role="status">{notice}</p>}
    {error && <p className="error-message" role="alert">{error}</p>}
    {detail?.origins?.length > 0 && <>
      <h4 className="section-label">网站存储</h4>
      <ul className="list bd-origins">{detail.origins.map(item => <li key={item.origin} className="list-row"><span className="list-row-text"><span className="list-row-title">{item.origin}</span></span><span className="bd-size">{formatBytes(item.bytes)}</span></li>)}</ul>
    </>}
    <div className="bd-subhead"><h4 className="section-label">Cookie</h4>
      <label className="bd-search compact"><Search size={14} /><input aria-label="搜索 Cookie" placeholder="按名称或域名筛选" maxLength={200} value={query} onChange={event => { setQuery(event.target.value); setPage(0); }} /></label></div>
    {!detail ? <p className="bd-loading">正在读取 Cookie…</p> : detail.cookies.length === 0 ? <p className="bd-loading">{query ? "没有匹配的 Cookie。" : "此网站没有 Cookie。"}</p> :
      <ul className="list bd-cookies" aria-label="Cookie 列表">{detail.cookies.map(item => <li key={item.key} className="bd-cookie">
        <div className="bd-cookie-head"><strong>{item.name || "（空名称）"}</strong>
          <button className="text-button" disabled={busy} onClick={() => value?.key === item.key ? setValue(null) : cookie(item.key, "value")}>{value?.key === item.key ? "隐藏值" : "查看值"}</button>
          <button className="icon-button" aria-label={`删除 Cookie ${item.name}`} disabled={busy || item.partitionKeyOpaque}
            title={item.partitionKeyOpaque ? "此分区 Cookie 不可单独删除，请清除此网站数据" : "删除此 Cookie"} onClick={() => cookie(item.key, "delete")}><Trash2 size={14} /></button></div>
        <div className="bd-cookie-meta">{item.domain} · 路径 {item.path} · {formatBytes(item.size)} · {item.session ? "会话结束时过期" : `过期：${new Date(item.expires * 1000).toLocaleString("zh-CN")}`}
          {item.secure && " · Secure"}{item.httpOnly && " · HttpOnly"}{item.sameSite && ` · SameSite=${item.sameSite}`}
          {(item.partitionKey || item.partitionKeyOpaque) && ` · 分区：${item.partitionKey?.topLevelSite || "不透明"}`}</div>
        {value?.key === item.key && <pre className="bd-cookie-value">{value.value || "（空值）"}</pre>}
      </li>)}</ul>}
    {detail && detail.total > detail.pageSize && <Pager page={detail.page} total={detail.total} size={detail.pageSize} busy={busy} onPage={setPage} />}
  </section>;
}

function Pager({ page, total, size, busy, onPage }) {
  const pages = Math.max(1, Math.ceil(total / size));
  return <div className="bd-pager"><span>第 {page + 1}/{pages} 页 · 共 {formatCount(total)} 项</span>
    <button className="compact" disabled={busy || page === 0} onClick={() => onPage(page - 1)}>上一页</button>
    <button className="compact" disabled={busy || page + 1 >= pages} onClick={() => onPage(page + 1)}>下一页</button></div>;
}

// ── Sites with data ──────────────────────────────────────────
function SiteList({ tabs, settings, onSettings, onOpen, onBack }) {
  const canPrivate = privateOpen(tabs);
  const [session, setSession] = useState("normal"), [query, setQuery] = useState(""), [sort, setSort] = useState("size"), [page, setPage] = useState(0);
  const [list, setList] = useState(null), [error, setError] = useState(""), [revision, setRevision] = useState(0);
  const [selected, setSelected] = useState(() => new Set()), [confirm, setConfirm] = useState(false), [busy, setBusy] = useState(false), [notice, setNotice] = useState("");
  useEffect(() => {
    const controller = new AbortController();
    const timer = setTimeout(() => {
      setError("");
      data({ mode: "sites", session, query, sort, page }, controller.signal).then(setList)
        .catch(failure => { if (!controller.signal.aborted) setError(failure.message); });
    }, query ? 180 : 0);
    return () => { clearTimeout(timer); controller.abort(); };
  }, [session, query, sort, page, revision]);
  useEffect(() => { setSelected(new Set()); setConfirm(false); }, [session]);
  const choose = site => setSelected(previous => { const next = new Set(previous); next.has(site) ? next.delete(site) : next.add(site); return next; });
  const clearSelected = async () => {
    setBusy(true); setError("");
    try {
      const result = await runJob({ mode: "clear-sites", session, sites: [...selected] });
      setNotice(resultLine(result)); setSelected(new Set()); setConfirm(false); setRevision(revision + 1);
    } catch (failure) { setError(failure.message); } finally { setBusy(false); }
  };
  const allOnPage = list?.sites.length > 0 && list.sites.every(item => selected.has(item.site));
  return <section className="bd-sites" aria-label="网站数据">
    <header className="bd-view-head">
      {onBack && <button className="icon-button" aria-label="返回隐私与网站数据" onClick={onBack}><ArrowLeft size={16} /></button>}
      <div className="bd-view-title"><h3>网站数据</h3><p>{list ? `${formatCount(list.total)} 个网站 · 共 ${formatBytes(list.bytes)}` : "正在统计…"}</p></div>
      {(canPrivate || session === "private") && <InlineSelect aria-label="网站数据浏览会话" value={session} onValueChange={next => { setSession(next); setPage(0); }}>
        <option value="normal">普通会话</option><option value="private" disabled={!canPrivate}>无痕会话</option></InlineSelect>}
      <button className="icon-button" aria-label="重新统计网站数据" disabled={busy} onClick={() => setRevision(revision + 1)}><RefreshCw size={15} /></button>
    </header>
    <div className="bd-toolbar">
      <label className="bd-search"><Search size={15} /><input aria-label="搜索网站" placeholder="搜索网站" maxLength={200} value={query} onChange={event => { setQuery(event.target.value); setPage(0); }} /></label>
      <InlineSelect aria-label="网站排序" value={sort} onValueChange={next => { setSort(next); setPage(0); }}>{sorts.map(item => <option key={item.id} value={item.id}>{item.label}</option>)}</InlineSelect>
    </div>
    {selected.size > 0 && <div className="bd-selection" role="region" aria-label="已选网站">
      <span>已选 {selected.size} 个网站</span>
      {confirm ? <><span className="bd-selection-warn">清除所选网站的 Cookie 和存储？这些网站需要重新登录。</span>
        <button className="danger-fill compact" disabled={busy} onClick={clearSelected}>确认清除</button><button className="compact" disabled={busy} onClick={() => setConfirm(false)}>取消</button></>
        : <><button className="compact danger" onClick={() => setConfirm(true)}><Trash2 size={14} />清除所选网站数据</button><button className="compact" onClick={() => setSelected(new Set())}>取消选择</button></>}
    </div>}
    {notice && <p className="bd-notice" role="status">{notice}</p>}
    {error && <p className="error-message" role="alert">{error}</p>}
    {!list ? <p className="bd-loading">正在统计网站数据…</p> : list.sites.length === 0 ? <EmptyState compact drawing="folder" title={query ? "没有匹配的网站" : session === "private" ? "无痕会话没有网站数据" : "还没有网站数据"}>{query ? "换个关键词试试。" : "访问网站后，它们保存的 Cookie 和存储会显示在这里。"}</EmptyState> :
      <>
        <div className="bd-list-head"><label className="check-line"><input type="checkbox" aria-label="选择本页全部网站" checked={allOnPage} onChange={() => setSelected(previous => { const next = new Set(previous); for (const item of list.sites) allOnPage ? next.delete(item.site) : next.add(item.site); return next; })} /><span>网站</span></label><span>Cookie</span><span>占用</span></div>
        <ul className="list bd-site-rows">{list.sites.map(item => {
          const kept = session === "normal" && keptBy(item.site, settings?.keep);
          return <li key={item.site} className="list-row bd-site-row">
            <input type="checkbox" aria-label={`选择 ${item.site}`} checked={selected.has(item.site)} onChange={() => choose(item.site)} />
            <button className="bd-site-open" onClick={() => onOpen(item.site, session)} aria-label={`${item.site} 详情`}>
              <span className="list-row-text"><span className="list-row-title">{item.site}{item.protected && <span className="chip">内置</span>}{kept && <span className="chip ok">保持登录</span>}</span>
                <span className="list-row-meta">{[item.origins > 1 ? `${item.origins} 个地址` : "", relativeVisit(item.last)].filter(Boolean).join(" · ") || "没有访问记录"}</span></span>
              <span className="bd-count">{formatCount(item.cookies)}</span><span className="bd-size">{formatBytes(item.bytes)}</span><ChevronRight size={15} />
            </button>
          </li>;
        })}</ul>
        {list.total > list.pageSize && <Pager page={list.page} total={list.total} size={list.pageSize} busy={busy} onPage={setPage} />}
        {session === "normal" && !list.historyComplete && <p className="field-help">只统计最近的 5000 条浏览记录涉及的网站；更早访问过的网站在“清除浏览数据”中一并清除。</p>}
      </>}
  </section>;
}

// ── History ──────────────────────────────────────────────────
const historyCap = 1000;
function HistoryView({ onOpenPage, onBack }) {
  const [query, setQuery] = useState(""), [items, setItems] = useState(null), [next, setNext] = useState(0);
  const [error, setError] = useState(""), [busy, setBusy] = useState(false), [revision, setRevision] = useState(0), [confirm, setConfirm] = useState(null);
  const generation = useRef(0);
  useEffect(() => {
    const version = ++generation.current, controller = new AbortController();
    const timer = setTimeout(() => {
      setError("");
      data({ mode: "history", query, limit: 100 }, controller.signal).then(result => { if (version === generation.current) { setItems(result.items); setNext(result.next); } })
        .catch(failure => { if (!controller.signal.aborted) setError(failure.message); });
    }, query ? 200 : 0);
    return () => { clearTimeout(timer); controller.abort(); };
  }, [query, revision]);
  const more = async () => {
    setBusy(true);
    try { const result = await data({ mode: "history", query, before: next, limit: 100 }); setItems(previous => [...previous, ...result.items].slice(0, historyCap)); setNext(items.length + result.items.length >= historyCap ? 0 : result.next); }
    catch (failure) { setError(failure.message); } finally { setBusy(false); }
  };
  const remove = async input => {
    setBusy(true); setError("");
    try { await data({ mode: "history-delete", ...input }); setConfirm(null); setRevision(revision + 1); }
    catch (failure) { setError(failure.message); } finally { setBusy(false); }
  };
  // A long address arrives cut (the guest bounds each entry): it is fetched
  // whole only to open it, and deleted by its id.
  const open = async item => {
    if (!item.truncated) return onOpenPage?.({ url: item.url });
    setError("");
    try { onOpenPage?.({ url: (await data({ mode: "history-url", id: item.id, time: item.time })).url }); }
    catch (failure) { setError(failure.message); }
  };
  const groups = useMemo(() => groupByDay(items || []), [items]);
  return <section className="bd-history" aria-label="浏览记录">
    <header className="bd-view-head">
      {onBack && <button className="icon-button" aria-label="返回隐私与网站数据" onClick={onBack}><ArrowLeft size={16} /></button>}
      <div className="bd-view-title"><h3>浏览记录</h3><p>普通会话访问过的网页。无痕会话不保存浏览记录。</p></div>
      <button className="compact" onClick={() => openBrowserData({ view: "clear", preset: { types: ["history"], range: "hour" } })}><Trash2 size={15} />按时间清除…</button>
    </header>
    <div className="bd-toolbar"><label className="bd-search"><Search size={15} /><input aria-label="搜索浏览记录" placeholder="搜索标题或网址" maxLength={200} value={query} onChange={event => setQuery(event.target.value)} /></label></div>
    {confirm && <div className="bd-confirm" role="alert"><span>{confirm.text}</span>
      <button className="danger-fill compact" disabled={busy} onClick={() => remove(confirm.input)}>删除</button><button className="compact" disabled={busy} onClick={() => setConfirm(null)}>取消</button></div>}
    {error && <p className="error-message" role="alert">{error}</p>}
    {!items ? <p className="bd-loading">正在读取浏览记录…</p> : items.length === 0 ? <EmptyState compact drawing="squiggle" title={query ? "没有匹配的记录" : "浏览记录是空的"}>{query ? "换个关键词试试。" : "在普通标签中访问的网页会出现在这里。"}</EmptyState> :
      groups.map(group => <section key={group.start} className="bd-day" aria-label={group.label}>
        <div className="bd-day-head"><h4>{group.label}</h4>
          {!query && <button className="text-button" disabled={busy} onClick={() => setConfirm({ text: `删除${group.label.split(" · ")[0]}的全部浏览记录？`, input: { start: group.start, end: group.end } })}>删除这一天</button>}</div>
        <ul className="list">{group.items.map(item => {
          const host = hostOf(item.url);
          return <li key={item.url + item.time} className="list-row bd-history-row">
            <time className="bd-time">{timeLabel(item.time)}</time>
            <button className="bd-history-open" title={item.url + (item.truncated ? "…" : "")} onClick={() => open(item)}>
              <span className="list-row-text"><span className="list-row-title">{item.title || host}</span><span className="list-row-meta">{item.label ? `${item.label} · ` : ""}{host}</span></span></button>
            <button className="icon-button" aria-label={`删除 ${item.title || host} 的记录`} disabled={busy} onClick={() => remove(item.truncated ? { entries: [{ id: item.id, time: item.time }] } : { urls: [item.url] })}><X size={14} /></button>
            <button className="text-button bd-history-site" disabled={busy || !host} onClick={() => setConfirm({ text: `删除 ${host} 的全部浏览记录？`, input: { host } })}>此网站</button>
          </li>;
        })}</ul>
      </section>)}
    {items && next > 0 && <div className="bd-more"><button className="compact" disabled={busy} onClick={more}>{busy ? "正在读取…" : "显示更早的记录"}</button></div>}
    {items?.length >= historyCap && <p className="field-help">已显示最近 {historyCap} 条。请用搜索查找更早的记录。</p>}
  </section>;
}

// ── Kept sites, automatic cleaning, reset ────────────────────
function KeptSites({ settings, onSettings, tabs }) {
  const [domain, setDomain] = useState(""), [error, setError] = useState(""), [busy, setBusy] = useState(false);
  // Sites of the open regular pages, offered as one-click additions.
  // Super Cage's own pages (127.0.0.1, localhost) are kept by default and not offered.
  const open = [...new Set((tabs || []).filter(tab => !tab.incognito && !tab.login && hostOf(tab.url)).map(tab => siteOfURL(tab.url)))]
    .filter(site => site && !isLoopbackSite(site) && !keptBy(site, settings?.keep)).slice(0, 3);
  const change = async (action, value) => {
    setBusy(true); setError("");
    try { onSettings(await saveSettings({ action, domain: value })); if (action === "keep-add") setDomain(""); }
    catch (failure) { setError(failure.message); } finally { setBusy(false); }
  };
  const typed = keptDomain(domain);
  return <section className="bd-block" aria-label="保持登录的网站" id="bd-kept">
    <h3>保持登录的网站</h3>
    <p className="bd-lead">清除浏览数据和自动清理时保留这些网站的 Cookie 和存储。一个域名包含它的子域名，例如 github.com 也保留 gist.github.com；co.uk、github.io 这类公共后缀不能整个保留。</p>
    <ul className="list bd-kept">
      {(settings?.keep || []).map(entry => <li key={entry.domain} className="list-row"><ShieldCheck size={16} className="bd-row-icon" />
        <span className="list-row-text"><span className="list-row-title">{entry.domain}</span><span className="list-row-meta">{keptCoverage(entry.domain)}</span></span>
        <button className="compact" disabled={busy} aria-label={`不再保留 ${entry.domain}`} onClick={() => change("keep-remove", entry.domain)}>移除</button></li>)}
      <li className="list-row"><LockKeyhole size={16} className="bd-row-icon" /><span className="list-row-text"><span className="list-row-title">Super Cage 内置页面<span className="chip">内置</span></span>
        <span className="list-row-meta">文件编辑器、Claude Science 和浏览器组件使用的本机地址</span></span></li>
    </ul>
    <form className="bd-add" onSubmit={event => { event.preventDefault(); if (typed) change("keep-add", typed); else setError("请输入网站域名，例如 github.com"); }}>
      <input aria-label="要保留的网站" placeholder="输入域名，例如 github.com" maxLength={300} value={domain} disabled={busy} onChange={event => { setDomain(event.target.value); setError(""); }} />
      <button className="compact" disabled={busy || !domain.trim()}><Plus size={15} />添加</button>
    </form>
    {open.length > 0 && <div className="bd-suggest"><span>从打开的网页添加：</span>{open.map(site => <button key={site} type="button" className="compact" disabled={busy} onClick={() => change("keep-add", site)}><Plus size={14} />{site}</button>)}</div>}
    {error && <p className="error-message" role="alert">{error}</p>}
    <details className="bd-details">
      <summary><ChevronRight size={14} aria-hidden="true" />了解详情</summary>
      <p className="field-help">{builtInNote} 清除时取消勾选“跳过…”会一并清除内置页面的登录，文件编辑器可用浏览器菜单的“恢复编辑器”重新进入。</p>
    </details>
  </section>;
}

function AutoClean({ settings, onSettings }) {
  const auto = settings?.auto || defaultSettings.auto;
  const [error, setError] = useState(""), [busy, setBusy] = useState(false);
  const save = async next => {
    setBusy(true); setError("");
    // The host reads only these fields (and refuses unknown ones).
    const value = { ...auto, ...next };
    try { onSettings(await saveSettings({ action: "auto", enabled: value.enabled, trigger: value.trigger, days: value.days, types: value.types })); }
    catch (failure) { setError(failure.message); } finally { setBusy(false); }
  };
  const toggleType = id => { const types = auto.types.includes(id) ? auto.types.filter(item => item !== id) : [...auto.types, id]; if (types.length) void save({ types }); };
  return <section className="bd-block" aria-label="自动清理">
    <h3>自动清理</h3>
    <label className="toggle-row"><span><strong>自动清除浏览数据</strong><small>只在停止或启动环境时运行，不在后台扫描。始终保留保持登录的网站。</small></span>
      <input type="checkbox" role="switch" aria-label="自动清除浏览数据" checked={auto.enabled} disabled={busy || !settings} onChange={event => save({ enabled: event.target.checked })} /></label>
    {auto.enabled && <div className="bd-auto">
      <div className="choice-list" role="radiogroup" aria-label="清理时机">
        <label className={"choice" + (auto.trigger === "stop" ? " checked" : "")}><input type="radio" name="bd-trigger" checked={auto.trigger === "stop"} disabled={busy} onChange={() => save({ trigger: "stop" })} />
          <span><strong>每次停止环境时</strong><small>停止环境前清除所选内容；重启环境不清除。</small></span></label>
        <label className={"choice" + (auto.trigger === "days" ? " checked" : "")}><input type="radio" name="bd-trigger" checked={auto.trigger === "days"} disabled={busy} onChange={() => save({ trigger: "days" })} />
          <span><strong>定期清除</strong><small>间隔到期后，在下一次启动或停止环境时清除。</small></span>
          {auto.trigger === "days" && <InlineSelect aria-label="清理间隔" value={String(auto.days)} disabled={busy} onValueChange={value => save({ days: Number(value) })}>
            <option value="1">每天</option><option value="7">每 7 天</option><option value="30">每 30 天</option></InlineSelect>}</label>
      </div>
      <div className="bd-auto-types" role="group" aria-label="自动清除的内容">{categories.map(item => <label key={item.id} className="check-line">
        <input type="checkbox" checked={auto.types.includes(item.id)} disabled={busy || (auto.types.length === 1 && auto.types.includes(item.id))} onChange={() => toggleType(item.id)} /><span>{item.title}</span></label>)}</div>
      <p className="field-help">{autoLine(auto)}</p>
    </div>}
    {error && <p className="error-message" role="alert">{error}</p>}
  </section>;
}

function ResetDialog({ onClose, onStarted }) {
  const [busy, setBusy] = useState(false), [error, setError] = useState(""), [understood, setUnderstood] = useState(false);
  const reset = async () => {
    setBusy(true); setError("");
    try { await data({ mode: "reset" }); onStarted?.(); onClose(); }
    catch (failure) { setError(failure.message); setBusy(false); }
  };
  return <Modal title="重置浏览器？" onClose={busy ? undefined : onClose} footer={<>
    {error && <span className="save-note" role="alert">{error}</span>}
    <button onClick={onClose} disabled={busy}>取消</button>
    <button className="danger-fill" disabled={busy || !understood} onClick={reset}>{busy ? "正在重置…" : "重置浏览器"}</button></>}>
    <div className="modal-body bd-reset" data-browser-dialog>
      <p>浏览器会以全新的资料重新启动，适合资料损坏、网页反复出错时使用。需要几秒钟，期间浏览器画面会断开。</p>
      <div className="bd-reset-lists">
        <div><h4 className="section-label">会清除</h4><ul><li>所有网站的登录、Cookie 和网站存储</li><li>浏览记录、缓存和 Chromium 的下载记录</li><li>网站权限和 Chromium 内部设置</li><li>在 Chromium 扩展页安装的扩展，以及各扩展自己的设置</li><li>当前打开的网页和无痕会话</li></ul></div>
        <div><h4 className="section-label">会保留</h4><ul><li>收藏夹和密码保险箱</li><li>通过 Super Cage 导入的扩展（重新载入）</li><li>新标签页地址、保持登录名单和自动清理设置</li><li>已下载的文件和下载面板</li></ul></div>
      </div>
      <label className="check-line"><input type="checkbox" checked={understood} disabled={busy} onChange={event => setUnderstood(event.target.checked)} /><span>我了解所有网站都需要重新登录</span></label>
    </div>
  </Modal>;
}

// ── The settings section: 隐私与网站数据 ──────────────────────
export function BrowserPrivacySection({ tab, tabs, onInternal, onOpenPage, onClose }) {
  const [available, retry] = useCapability();
  const [view, setView] = useState(() => takePendingPrivacyView());
  const [site, setSite] = useState(null);
  const { settings, setSettings, error: settingsError, retry: retrySettings } = useSettings();
  const [reset, setReset] = useState(false), [notice, setNotice] = useState("");
  useEffect(() => {
    const open = ({ detail }) => { if (detail?.view === "privacy") { setView(detail.section || ""); setSite(null); } };
    document.addEventListener("cage-browser-data", open);
    return () => document.removeEventListener("cage-browser-data", open);
  }, []);
  useEffect(() => { if (view === "kept") document.getElementById("bd-kept")?.scrollIntoView({ block: "start" }); }, [view, settings]);
  if (!available) return <p className="bd-loading">正在读取浏览器能力…</p>;
  if (!available.available && !usesLegacy(available)) return <Unavailable value={available} onRetry={retry} />;
  if (!available.available) return <LegacyPrivacy tab={tab} tabs={tabs} onInternal={onInternal} onClose={onClose} reason={available.reason} />;
  if (site) return <SiteDetail site={site.site} session={site.session} settings={settings} onSettings={setSettings} onBack={() => setSite(null)} />;
  if (view === "sites") return <SiteList tabs={tabs} settings={settings} onSettings={setSettings} onOpen={(name, session) => setSite({ site: name, session })} onBack={() => setView("")} />;
  if (view === "history") return <HistoryView onOpenPage={onOpenPage} onBack={() => setView("")} />;
  return <div className="bd-privacy">
    <div className="bd-entry-list">
      <button className="bd-entry" onClick={() => openBrowserData({ view: "clear" })}><Trash2 size={18} />
        <span><strong>清除浏览数据</strong><small>按时间清除浏览记录、Cookie、网站数据和缓存</small></span><kbd>{clearShortcut}</kbd><ChevronRight size={16} /></button>
      <button className="bd-entry" onClick={() => setView("sites")}><Cookie size={18} />
        <span><strong>网站数据</strong><small>查看各网站的 Cookie 和存储，逐个或批量清除</small></span><ChevronRight size={16} /></button>
      <button className="bd-entry" onClick={() => setView("history")}><Clock3 size={18} />
        <span><strong>浏览记录</strong><small>按日期查看、搜索和删除访问过的网页</small></span><ChevronRight size={16} /></button>
      <button className="bd-entry" onClick={() => onInternal("permissions")}><LockKeyhole size={18} />
        <span><strong>网站权限</strong><small>摄像头、麦克风、位置和通知等权限，在 Chromium 页面中调整</small></span><ExternalLink size={16} /></button>
    </div>
    {notice && <p className="bd-notice" role="status">{notice}</p>}
    {settingsError ? <section className="bd-block" aria-label="保持登录的网站" id="bd-kept"><h3>保持登录的网站和自动清理</h3>
      <Unavailable value={{ message: sentence(settingsError) + "读出来之前，清除浏览数据不会清除 Cookie、网站数据和缓存。" }} onRetry={retrySettings} /></section> : <>
      <KeptSites settings={settings} onSettings={setSettings} tabs={tabs} />
      <AutoClean settings={settings} onSettings={setSettings} />
    </>}
    <section className="bd-block bd-danger-zone" aria-label="重置浏览器">
      <h3>重置浏览器</h3>
      <div className="bd-reset-row"><p className="bd-lead">浏览器资料损坏、网页反复出错时，以全新的资料重新开始。收藏夹、密码保险箱和导入的扩展会保留。</p>
        <button className="danger" onClick={() => setReset(true)}><RotateCcw size={15} />重置浏览器…</button></div>
    </section>
    {reset && <ResetDialog onClose={() => setReset(false)} onStarted={() => setNotice("浏览器正在以全新的资料重新启动，完成后网页需要重新登录。")} />}
  </div>;
}

// The earlier privacy section, for servers and guests without browser data.
function LegacyPrivacy({ tab, tabs, onInternal, onClose, reason }) {
  const [session, setSession] = useState("normal");
  const privateTab = tabs.find(item => item.incognito && !item.login);
  const cookieTab = session === "private" && privateTab ? privateTab : tab;
  return <>
    {reason === "update" && <p className="inline-info"><RefreshCw size={14} /><span>更新环境组件后，可在这里按时间清除浏览数据、管理网站数据和浏览记录。</span></p>}
    <div className="browser-setting-row"><div><h3>Cookie 与登录状态</h3><p>普通会话与无痕会话分别管理。</p></div>
      <InlineSelect aria-label="Cookie 浏览会话" value={session} onValueChange={setSession}><option value="normal">普通会话</option><option value="private" disabled={!privateTab}>无痕会话</option></InlineSelect></div>
    <BrowserCookies key={cookieTab.id} tab={cookieTab} onClose={onClose} />
    <div className="browser-settings-links"><button onClick={() => onInternal("permissions")}><ExternalLink size={15} />网站权限</button><button onClick={() => onInternal("clear")}><ExternalLink size={15} />清除普通会话浏览数据</button></div>
  </>;
}

// ── The address bar's site menu ──────────────────────────────
function SitePopover({ tab, anchor, onClose }) {
  const site = siteOfURL(tab.url), session = tab.incognito ? "private" : "normal";
  const [detail, setDetail] = useState(null), [settings, setSettings] = useState(null);
  const [phase, setPhase] = useState("view"), [result, setResult] = useState(null), [error, setError] = useState("");
  useEffect(() => {
    const controller = new AbortController();
    data({ mode: "site", session, site }, controller.signal).then(setDetail).catch(failure => { if (!controller.signal.aborted) setError(failure.message); });
    if (session === "normal") loadSettings().then(setSettings, () => {});
    return () => controller.abort();
  }, [site, session]);
  const kept = keptBy(site, settings?.keep);
  const clear = async () => {
    setPhase("clearing"); setError("");
    try { setResult(await runJob({ mode: "clear-sites", session, sites: [site] })); setPhase("done"); }
    catch (failure) { setError(failure.message); setPhase("confirm"); }
  };
  const storage = detail?.origins?.reduce((sum, item) => sum + item.bytes, 0) || 0;
  const footer = phase === "done" ? <button className="primary compact" onClick={onClose}>完成</button>
    : phase === "view" ? <><button className="compact" onClick={() => { onClose(); openBrowserData({ view: "site", tab }); }}>网站数据详情</button>
      <button className="compact danger" disabled={!detail} onClick={() => setPhase("confirm")}><Trash2 size={14} />清除此网站数据</button></>
    : <><button className="compact" disabled={phase === "clearing"} onClick={() => setPhase("view")}>取消</button>
      <button className="compact danger-fill" disabled={phase === "clearing"} onClick={clear}>{phase === "clearing" ? "正在清除…" : "确认清除"}</button></>;
  return <Modal title={site} anchor={anchor} align="start" onClose={phase === "clearing" ? undefined : onClose} footer={footer}>
    <div className="modal-body bd-popover" data-browser-dialog>
      {phase === "done" ? <p role="status">{resultLine(result)}</p> : phase === "view" ? <>
        <p className="bd-popover-meta">{session === "private" ? "无痕会话 · " : ""}{detail ? `${formatCount(detail.total)} 个 Cookie · 网站存储 ${formatBytes(storage)}` : error ? "" : "正在读取…"}</p>
        {session === "normal" && settings && <label className="toggle-row bd-popover-keep"><span><strong>保持登录</strong><small>清除浏览数据时保留此网站</small></span>
          <input type="checkbox" role="switch" aria-label={`保持 ${site} 的登录`} checked={Boolean(kept)} onChange={async () => setSettings(await saveSettings({ action: kept ? "keep-remove" : "keep-add", domain: kept ? kept.domain : site }).catch(failure => { setError(failure.message); return settings; }))} /></label>}
      </> : <p>清除 {site} 的 Cookie 和网站存储？{session === "private" ? "只影响无痕会话。" : ""}此网站需要重新登录，网页会重新加载。{loopback.includes(site) ? loopbackWarning : ""}</p>}
      {error && <p className="error-message" role="alert">{error}</p>}
    </div>
  </Modal>;
}

function SiteDialog({ tab, onClose }) {
  const [available] = useCapability();
  const site = siteOfURL(tab?.url), session = tab?.incognito ? "private" : "normal";
  const [settings, setSettings] = useState(null);
  useEffect(() => { loadSettings().then(setSettings, () => {}); }, []);
  return <Modal title="Cookie 和网站数据" wide onClose={onClose} footer={<button className="compact" onClick={() => { onClose(); openBrowserData({ view: "privacy", section: "sites" }); }}>所有网站的数据<ChevronRight size={14} /></button>}>
    <div className="modal-body bd-site-dialog" data-browser-dialog>
      {!available ? <p className="bd-loading">正在读取…</p> : !available.available ? <BrowserCookies tab={tab} onClose={onClose} /> :
        <SiteDetail site={site} session={session} settings={settings} onSettings={setSettings} compact />}
    </div>
  </Modal>;
}

// Opened by the launcher with a request: clear, site, popover or reset.
export default function BrowserDataApp({ request, onClose, tabs = [], onInternal, onVault }) {
  const [available, retry] = useCapability();
  const tab = request.tab;
  useEffect(() => {
    if (usesLegacy(available) && request.view === "clear") { onClose(); onInternal?.("clear"); }
    // The address bar's own cookie panel stays for servers and guests without browser data.
    if (usesLegacy(available) && request.fallback) { onClose(); request.fallback(); }
    if (available?.available && (request.view === "site" || request.view === "popover") && !siteOfURL(tab?.url)) { onClose(); openBrowserData({ view: "privacy", section: "sites" }); }
  }, [available]);
  if (available && !available.available && !usesLegacy(available)) return <Unavailable value={available} onRetry={retry} onClose={onClose} />;
  if (request.view === "site") return available && (siteOfURL(tab?.url) || !available.available) && !(request.fallback && !available.available) ? <SiteDialog tab={tab} onClose={onClose} /> : null;
  if (!available?.available) return null;
  if (request.view === "clear") return <ClearDialog tab={tab} tabs={tabs} preset={request.preset} onClose={onClose} onInternal={onInternal} onVault={onVault} />;
  if (request.view === "popover" && siteOfURL(tab?.url)) return <SitePopover tab={tab} anchor={request.anchor} onClose={onClose} />;
  if (request.view === "reset") return <ResetDialog onClose={onClose} />;
  return null;
}
