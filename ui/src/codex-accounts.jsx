import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Check, Pencil, Plus, RefreshCw, TerminalSquare, Trash2 } from "lucide-react";
import { EmptyState } from "./drawings";
import { ProviderMark } from "./provider-mark";
import { api } from "./api";
import { InlineSelect } from "./inline-select";
import { QuotaClock, QuotaWindow, UsageNote, useUsageCheck } from "./claude-accounts";
import { codexAccountName, codexActiveCounts, codexQuotaView, codexWindowLabel } from "./codex-accounts-state.mjs";
import { useWindowVisibility } from "./window-visibility";
import "./claude-accounts.css";

// Ordinary reads never start an upstream usage query. enabled is false where
// Codex is absent (codexSurfaces): nothing is read then.
export function useCodexAccounts(enabled = true) {
  const [registry, setRegistry] = useState(null);
  const [error, setError] = useState("");
  const generation = useRef(0);
  const pendingMutation = useRef(0);
  const polling = useRef(false);
  const load = useCallback(async (change, visiblePanel = false) => {
    if (!change && (pendingMutation.current || (visiblePanel && polling.current))) return;
    const request = ++generation.current;
    if (change) pendingMutation.current++;
    else if (visiblePanel) polling.current = true;
    try {
      const value = await api(visiblePanel ? "/codex-accounts?panel=visible" : "/codex-accounts", change);
      if (!Array.isArray(value?.accounts)) throw new Error("无法读取 Codex 账号列表");
      if (generation.current === request) {
        setRegistry(value);
        setError("");
      }
      return value.accounts;
    } catch (e) {
      if (generation.current === request) setError(e.message);
      if (change) throw e;
    } finally {
      if (change) pendingMutation.current--;
      else if (visiblePanel) polling.current = false;
    }
  }, []);
  useEffect(() => {
    if (!enabled) return;
    load();
    return () => { generation.current++; };
  }, [load, enabled]);
  return { accounts: registry?.accounts ?? null, detected: registry?.detected,
    detectionStatus: registry?.detection_status, detectionError: registry?.detection_error,
    usageRunning: registry?.usage_panel_running, usageUnavailable: registry?.usage_unavailable_reason,
    guestSupported: registry?.guest_supported !== false, loginPending: Boolean(registry?.login_pending),
    scheduling: registry?.scheduling || {mode:"balanced",switch_policy:"ask"}, loginNotice: registry?.login_notice, magpieTakeover: Boolean(registry?.magpie_takeover), error, load };
}

const statusLabels = {
  pending: "等待登录", ready: "已登录", signed_out: "需要登录",
  duplicate: "账号已在列表中", changed: "登录身份已改变",
};

function CodexUsage({ quota, state }) {
  return <div className="claude-quota-grid">
    <QuotaWindow label={codexWindowLabel(quota?.primary?.window_minutes, "primary")} snapshot={quota?.primary} state={state} />
    <QuotaWindow label={codexWindowLabel(quota?.secondary?.window_minutes, "secondary")} snapshot={quota?.secondary} state={state} />
  </div>;
}

function DefaultLogin({ profile }) {
  return <article className="claude-profile claude-detected" aria-label={profile.email}>
    <div className="claude-profile-heading">
      <span className="claude-profile-avatar" aria-hidden="true"><ProviderMark provider="codex" size={16} decorative /></span>
      <div className="claude-profile-identity">
        <div className="claude-profile-name">
          <h4 title={profile.email}>{profile.email}</h4>
          {profile.plan && <span className="claude-profile-plan">{profile.plan}</span>}
        </div>
        <div className="claude-profile-meta">
          <span className="claude-profile-status" data-status="detected">环境默认登录（~/.codex）</span>
        </div>
      </div>
      <div className="claude-profile-usage">
        <CodexUsage quota={profile.quota} state={profile.quota ? "local" : "unknown"} />
      </div>
    </div>
    <p className="field-help">账号列表为空时，新建 Codex 终端使用此登录；列表中有账号后改为自动分配。额度来自此登录最近的本地会话记录。</p>
  </article>;
}

function CodexProfileCard({ profile, environment, usageEnvironment, busy, active, perform, change, login, remove }) {
  const [renaming, setRenaming] = useState(false);
  const [label, setLabel] = useState("");
  const displayName = codexAccountName(profile);
  const savedLabel = label.trim() || profile.email;
  const view = useMemo(() => codexQuotaView(profile), [profile]);
  const usage = useUsageCheck(view, usageEnvironment, perform, change);
  return <article className="claude-profile" aria-label={displayName} data-enabled={profile.enabled}>
    <div className="claude-profile-heading">
      <span className="claude-profile-avatar" aria-hidden="true"><ProviderMark provider="codex" size={16} decorative /></span>
      <div className="claude-profile-identity">
        {renaming ? <form className="claude-profile-rename" onSubmit={(e) => {
          e.preventDefault();
          perform(async () => { await change({ action: "update", id: profile.id, label: savedLabel }); setRenaming(false); });
        }}>
          <input autoFocus aria-label="账号备注" maxLength={64} placeholder={profile.email || "账号名称"} value={label} onChange={(e) => setLabel(e.target.value)} />
          <button disabled={busy || !savedLabel} aria-label="保存备注"><Check size={15} /></button>
          <button type="button" disabled={busy} onClick={() => setRenaming(false)}>取消</button>
        </form> : <div className="claude-profile-name">
          <h4 title={displayName}>{displayName}</h4>
          {profile.plan && <span className="claude-profile-plan">{profile.plan}</span>}
          <button type="button" className="icon-button" disabled={busy} aria-label={`修改 ${displayName} 的备注`}
            onClick={() => { setLabel(profile.label === profile.email ? "" : profile.label || ""); setRenaming(true); }}><Pencil size={14} /></button>
        </div>}
        <div className="claude-profile-meta">
          <span className="claude-profile-status" data-ready={profile.status === "ready"} data-status={profile.status}>{statusLabels[profile.status] || "状态未知"}</span>
          {profile.email && profile.email !== displayName && <span className="claude-profile-email" title={profile.email}>{profile.email}</span>}
        </div>
      </div>
      <div className="claude-profile-usage">
        <CodexUsage quota={profile.quota} state={usage.state} />
        <UsageNote profile={view} state={usage.state} />
      </div>
      <div className="claude-profile-actions">
        <label className="check-line claude-participation">
          <input type="checkbox" aria-label="参与自动分配" checked={profile.enabled} disabled={busy}
            onChange={(e) => { const enabled = e.target.checked; perform(() => change({ action: "update", id: profile.id, enabled })); }} />
          <span className="claude-switch-track" aria-hidden="true" />
          <span>{profile.enabled ? "参与分配" : "已暂停"}</span>
        </label>
        {profile.status === "ready"
          ? <button className="quiet-button claude-icon-action" aria-label="重新登录" disabled={busy || !!environment || active > 0} onClick={() => perform(() => login(profile.id))}>
            <TerminalSquare size={15} /></button>
          : <button className="quiet-button" disabled={busy || !!environment || active > 0} onClick={() => perform(() => login(profile.id))}>
            <TerminalSquare size={14} />打开登录终端</button>}
        <button className="quiet-button claude-icon-action" aria-label="检查状态" disabled={busy || !!usageEnvironment || usage.state === "checking"} onClick={usage.check}>
          <RefreshCw size={15} className={usage.state === "checking" ? "spin" : undefined} /></button>
        <button className="quiet-button claude-icon-action claude-remove-action" aria-label="移除账号" disabled={busy || active > 0} onClick={() => perform(() => remove(profile))}>
          <Trash2 size={15} /></button>
      </div>
    </div>
    {active > 0 && <p className="claude-profile-note">{active} 个终端仍在运行，关闭后可重新登录或移除</p>}
    {profile.status === "changed" && <p className="field-help">请用原账号重新登录。若要使用新的身份，请点击「登录新账号」。</p>}
    {profile.status === "signed_out" && <p className="field-help">此账号已退出登录，暂不参与分配。点击「重新登录」恢复。</p>}
    {profile.status === "duplicate" && <p className="field-help">同一订阅只参与一次分配。请在此位置登录另一个账号，或移除此记录。</p>}
  </article>;
}

export function CodexAccountsPanel({ store, running, environmentBusy, tabs, login, onConfirm }) {
  const windowVisible = useWindowVisibility();
  const { accounts, detected, detectionStatus, detectionError, loginNotice, loginPending, usageRunning, usageUnavailable, guestSupported, load, error } = store;
  const [busy, setBusy] = useState(false);
  const [requestingUsage, setRequestingUsage] = useState(false);
  const [actionError, setActionError] = useState("");
  const environment = !guestSupported ? "unsupported" : !running || detectionStatus === "offline" ? "offline" :
    detectionStatus === "update_required" ? "update_required" : "";
  const usageEnvironment = environment === "unsupported" ? "blocked" : environment || (usageUnavailable ? "blocked" : "");
  const checking = !usageEnvironment && (usageRunning || accounts?.some((profile) => profile.usage_checking));
  const activeCounts = useMemo(() => codexActiveCounts(tabs), [tabs]);
  const clockAccounts = useMemo(() => [detected, ...(accounts || [])].filter(Boolean).map(codexQuotaView), [accounts, detected]);
  useEffect(() => {
    let requested = false, mounted = true;
    const openPanel = () => {
      if (requested || document.hidden) return;
      requested = true;
      setRequestingUsage(running && guestSupported);
      if (running && guestSupported) {
        load({ action: "usage_panel" })
          .catch(() => {})
          .finally(() => { if (mounted) setRequestingUsage(false); });
      } else load();
    };
    openPanel();
    document.addEventListener("visibilitychange", openPanel);
    return () => { mounted = false; document.removeEventListener("visibilitychange", openPanel); };
  }, [load, running, guestSupported]);
  useEffect(() => {
    if (!windowVisible || !running || !guestSupported) return;
    const timer = setInterval(() => load(undefined, true), checking || loginPending ? 2000 : 12000);
    return () => clearInterval(timer);
  }, [load, checking, loginPending, running, guestSupported, windowVisible]);
  const perform = async (operation) => {
    if (busy) return;
    setBusy(true);
    setActionError("");
    try { await operation(); }
    catch (e) { setActionError(e.message); }
    finally { setBusy(false); }
  };
  const remove = async (profile) => {
    const last = accounts.length === 1;
    if (await onConfirm({ title: `移除「${codexAccountName(profile) || "未命名账号"}」？`,
      body: (last ? "这是列表中的最后一个账号。移除后，新建 Codex 终端会沿用环境的 ~/.codex 登录。" : "此账号不再参与 Codex 终端的自动分配。") +
        "虚拟机中的登录资料和会话记录会保留在原目录。",
      confirm: "移除账号", danger: true })) {
      await load({ action: "remove", id: profile.id });
    }
  };
  return <section className="claude-profiles" aria-label="Codex 账号管理">
    <div className="section-title claude-profiles-intro">
      <p>单独登录多个 ChatGPT 账号使用 Codex，新建 Codex 终端时自动分配。每个账号使用独立的 CODEX_HOME，共用 ~/.codex 中的指令、提示词和技能。</p>
      <div className="claude-add-profile">
        <button className="primary" disabled={busy || environmentBusy || !!environment}
          onClick={() => perform(() => login())}><Plus size={16} />登录新账号</button>
      </div>
    </div>
    {!guestSupported && <p className="claude-profile-note" role="status">Codex 账号目前只支持 Linux 环境。macOS 和 Windows 环境请在普通终端中使用 Codex。</p>}
    <details className="claude-scheduling">
      <summary>调度设置 · {store.scheduling.mode === "sequential" ? "顺序优先" : "均衡分配"}</summary>
      <label>新会话分配<InlineSelect inline aria-label="新会话分配" value={store.scheduling.mode} disabled={busy}
        onValueChange={mode => perform(() => load({action:"scheduling",scheduling:{...store.scheduling,mode}}))}>
        <option value="balanced">均衡分配</option><option value="sequential">按列表顺序优先</option>
      </InlineSelect></label>
      <label>新会话切换授权<InlineSelect inline aria-label="新会话切换授权" value={store.scheduling.switch_policy} disabled={busy}
        onValueChange={switch_policy => perform(() => load({action:"scheduling",scheduling:{...store.scheduling,switch_policy}}))}>
        <option value="ask">每次询问</option><option value="allow">额度耗尽后自动接续</option><option value="wait">始终等待原账号</option>
      </InlineSelect></label>
      <p className="field-help">Codex 与 Claude 的调度设置互相独立。已打开的 Codex 终端保持原账号，不会在会话中途切换；切换授权会随设置保存，供以后的接续功能使用。</p>
      {store.scheduling.mode === "sequential" && accounts?.length > 1 && <div className="claude-account-order" aria-label="账号优先顺序">
        {accounts.map((profile,index) => <div key={profile.id}><span>{index+1}. {codexAccountName(profile)}</span>
          <button disabled={busy || index===0} aria-label={`提高 ${codexAccountName(profile)} 的优先级`} onClick={() => {
            const order=accounts.map(a=>a.id); [order[index-1],order[index]]=[order[index],order[index-1]];
            perform(() => load({action:"scheduling",scheduling:store.scheduling,order}));
          }}>上移</button></div>)}
      </div>}
    </details>
    <p className="field-help claude-login-help">登录终端会显示 https://auth.openai.com/codex/device 和一次性验证码。在任意浏览器打开该页面、登录 ChatGPT 并输入验证码，完成后终端自动结束。</p>
    {(actionError || error) && <p className="account-error" role="alert">{actionError || error}</p>}
    {loginNotice && <p className="claude-profile-note" role="status">{loginNotice}</p>}
    {loginPending && <p className="claude-profile-note" role="status">登录终端仍在等待授权。</p>}
    {detectionError && <p className="account-error" role="alert">{detectionError}</p>}
    {usageUnavailable && !environment && <p className="claude-profile-note" role="status">{usageUnavailable}</p>}
    {guestSupported && (!running ? <p className="claude-profile-note">启动 Super Cage 环境后可检测 Codex 登录，也可登录新账号。</p> :
      detectionStatus === "offline" ? <p className="claude-profile-note">工作环境就绪后可检测和登录账号。</p> :
      detectionStatus === "signed_out" && <p className="claude-profile-note">环境的 ~/.codex 尚未登录 ChatGPT。账号列表为空时，Codex 终端会提示登录。</p>)}
    <div className="claude-list-heading">
      <h4>账号列表 <span>{accounts?.length ?? "—"}</span></h4>
      {requestingUsage || checking ? <small role="status">{requestingUsage ? "正在请求账号用量…" : "正在查询账号用量…"}</small> :
        accounts?.length > 0 && <small>{accounts.filter((a) => a.enabled).length} 个启用调度</small>}
    </div>
    <div className="claude-profile-list" role="region" aria-label="Codex 账号列表" tabIndex={0}>
      {accounts === null && !error && <p role="status">正在读取账号…</p>}
      {accounts?.length === 0 && <EmptyState drawing="key" title="还没有 Codex 账号" className="claude-profile-empty">
        <span>尚无已加入调度的 Codex 账号。</span>加入第一个账号前，新建 Codex 终端沿用环境的 ~/.codex 登录。
      </EmptyState>}
      <QuotaClock accounts={clockAccounts}>
        {detected && <DefaultLogin key={detected.identity} profile={detected} />}
        {accounts?.map((profile) => <CodexProfileCard key={profile.id} profile={profile} environment={environment} usageEnvironment={usageEnvironment}
          busy={busy || environmentBusy} perform={perform} change={load} login={login} remove={remove}
          active={activeCounts.get(profile.id) || 0} />)}
      </QuotaClock>
    </div>
  </section>;
}
