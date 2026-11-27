import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Activity,
  ArrowDown,
  ArrowUp,
  Check,
  Flame,
  KeyRound,
  LoaderCircle,
  LogIn,
  Pencil,
  Plus,
  RefreshCw,
  Settings,
  ShieldAlert,
  ShieldCheck,
  SlidersHorizontal,
  Smartphone,
  TerminalSquare,
  Timer,
  Trash2,
} from "lucide-react";
import { api } from "./api";
import { AgentIcon } from "./agent-icon";
import { InlineSelect } from "./inline-select";
import { LaunchAccountPolicy, launchModeLabel } from "./launch-account-policy";
import { agentAccountsPath, createAgentAccountsSequencer } from "./agent-accounts-state.mjs";
import { projectName } from "./workspace-state.mjs";
import { useWindowVisibility } from "./window-visibility";
import "./agent-accounts.css";

const STRATEGY_LABELS = {
  smart: "智能综合评分（推荐：权重 + 负载 + 额度紧迫度 + 缓存亲和）",
  balanced: "加权负载均衡（按并发负载比例分配）",
  sequential: "顺序优先（优先使用排在前面的健康账号）",
  sticky_project: "同项目强亲和（最大化 Prompt Cache 命中）",
  quota_first: "额度重置优先（优先消耗即将重置的账号配额）",
  drain: "即将重置优先（优先用尽临近重置窗口额度）",
};

function formatCooldown(secondsLeft) {
  if (secondsLeft <= 0) return "即将恢复";
  if (secondsLeft < 60) return `${secondsLeft}秒`;
  const m = Math.ceil(secondsLeft / 60);
  if (m < 60) return `${m}分钟`;
  const h = Math.floor(m / 60);
  return `${h}小时${m % 60 ? ` ${m % 60}分` : ""}`;
}

function formatResetCountdown(seconds) {
  if (seconds <= 0) return "已重置";
  if (seconds < 60) return `${seconds}秒后重置`;
  const minutes = Math.ceil(seconds / 60);
  if (minutes < 60) return `${minutes}分后重置`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}时${minutes % 60 ? ` ${minutes % 60}分` : ""}后重置`;
  const days = Math.floor(hours / 24);
  return `${days}天${hours % 24 ? ` ${hours % 24}时` : ""}后重置`;
}

function AgentQuotaMeters({ quota, accentColor, now }) {
  if (!quota || (!quota.five_hour && !quota.seven_day)) return null;
  const rows = [
    ["5h", quota.five_hour],
    ["7d", quota.seven_day],
  ].filter(([, snap]) => Boolean(snap));
  if (!rows.length) return null;

  return (
    <div className="agent-quota-grid" aria-label="实时订阅配额">
      {rows.map(([period, snap]) => {
        const hasPct = Number.isFinite(snap.used_percentage);
        const pct = hasPct ? Math.max(0, Math.min(100, Math.round(snap.used_percentage))) : 0;
        const resetsAt = Number(snap.resets_at) || 0;
        const remaining = resetsAt > 0 ? resetsAt - now : null;
        const expired = remaining !== null && remaining <= 0;
        const tier = expired ? "ok" : pct >= 90 ? "high" : pct >= 75 ? "warn" : "ok";
        return (
          <div key={period} className="agent-quota-row" data-tier={tier} role="group" aria-label={`${period} 配额`}>
            <span className="agent-quota-period">{period}</span>
            <div className="agent-quota-bar" aria-hidden="true">
              <div
                className="agent-quota-fill"
                style={{
                  width: `${hasPct && !expired ? pct : 0}%`,
                  background: tier === "ok" ? (accentColor || "var(--accent)") : undefined,
                }}
              />
            </div>
            <div className="agent-quota-reading">
              {hasPct && <strong>{expired ? "0%" : `${pct}%`}</strong>}
              {remaining !== null && (
                <span className="agent-quota-reset">
                  <Timer size={11} aria-hidden="true" />
                  {formatResetCountdown(remaining)}
                </span>
              )}
            </div>
          </div>
        );
      })}
    </div>
  );
}

function AgentAccountCard({
  account,
  index,
  spec,
  activeCount,
  now,
  busy,
  onMutate,
  onLaunchWithAccount,
  onStartLogin,
  onConfirm,
}) {
  const [renaming, setRenaming] = useState(false);
  const [label, setLabel] = useState(account.label || "");
  const [tuningOpen, setTuningOpen] = useState(false);
  const [weight, setWeight] = useState(String(account.weight || 10));
  const [maxConc, setMaxConc] = useState(String(account.max_concurrency || 0));
  const [group, setGroup] = useState(account.handoff_group || "");
  const [newKey, setNewKey] = useState("");
  const [newBaseURL, setNewBaseURL] = useState(account.base_url || "");
  const [newModel, setNewModel] = useState(account.model || "");
  const [newProvider, setNewProvider] = useState(account.provider || "");
  const [newEnvVar, setNewEnvVar] = useState(account.env_var || spec.auth?.api_key_env_vars?.[0] || "");

  useEffect(() => {
    setLabel(account.label || "");
    setWeight(String(account.weight || 10));
    setMaxConc(String(account.max_concurrency || 0));
    setGroup(account.handoff_group || "");
    setNewBaseURL(account.base_url || "");
    setNewModel(account.model || "");
    setNewProvider(account.provider || "");
    setNewEnvVar(account.env_var || spec.auth?.api_key_env_vars?.[0] || "");
  }, [
    account.id,
    account.label,
    account.weight,
    account.max_concurrency,
    account.handoff_group,
    account.base_url,
    account.model,
    account.provider,
    account.env_var,
    spec.auth?.api_key_env_vars,
  ]);

  const displayName = account.label || account.email || account.username || account.provider || account.key_hint || `账号 ${index + 1}`;
  const cooldownLeft = (account.cooldown_until || 0) - now;
  const cooling = cooldownLeft > 0;
  const isAPIKey = account.auth_mode === "api_key" || account.source === "api_key";
  const hasHealth = typeof account.health_ewma === "number" && Number.isFinite(account.health_ewma);
  const healthPct = hasHealth ? Math.round(Math.max(0, Math.min(1, account.health_ewma)) * 100) : 0;
  const healthTier = healthPct >= 80 ? "good" : healthPct >= 50 ? "warn" : "bad";

  const saveTuning = async (e) => {
    e.preventDefault();
    const w = Math.max(1, Math.min(100, Number(weight) || 10));
    const mc = Math.max(0, Math.min(64, Number(maxConc) || 0));
    const payload = {
      action: "update",
      id: account.id,
      weight: w,
      max_concurrency: mc,
      handoff_group: group.trim(),
    };
    if (isAPIKey) {
      payload.env_var = newEnvVar.trim() || account.env_var || spec.auth?.api_key_env_vars?.[0] || "";
      payload.base_url = newBaseURL.trim();
      payload.model = newModel.trim();
      payload.provider = newProvider.trim();
      if (newKey.trim()) {
        payload.api_key = newKey.trim();
      }
    }
    await onMutate(payload);
    setNewKey("");
    setTuningOpen(false);
  };

  return (
    <article
      className="agent-account-card"
      data-cooling={cooling}
      data-enabled={account.enabled}
      aria-label={displayName}
    >
      <div className="claude-profile-heading">
        <span className="claude-profile-avatar" style={{ background: `color-mix(in srgb, ${spec.accent_color || "var(--accent)"} 13%, var(--paper))` }}>
          <AgentIcon agentId={spec.id} accountNumber={index + 1} accentColor={spec.accent_color} size={28} />
        </span>
        <div className="claude-profile-identity">
          {renaming ? (
            <form
              className="claude-profile-rename"
              onSubmit={async (e) => {
                e.preventDefault();
                if (!label.trim()) return;
                await onMutate({ action: "update", id: account.id, label: label.trim() });
                setRenaming(false);
              }}
            >
              <input
                autoFocus
                aria-label="账号备注"
                maxLength={64}
                value={label}
                onChange={(e) => setLabel(e.target.value)}
              />
              <button disabled={busy || !label.trim()} aria-label="保存备注"><Check size={15} /></button>
              <button type="button" disabled={busy} onClick={() => setRenaming(false)}>取消</button>
            </form>
          ) : (
            <div className="claude-profile-name">
              <h4 title={displayName}>{displayName}</h4>
              <span className="claude-profile-plan">{isAPIKey ? "API Key" : "OAuth"}</span>
              {account.plan && <span className="claude-profile-plan">{account.plan}</span>}
              {account.handoff_group && <span className="claude-profile-plan">分组: {account.handoff_group}</span>}
              <button
                type="button"
                className="icon-button"
                disabled={busy}
                aria-label={`修改 ${displayName} 的备注`}
                onClick={() => { setLabel(account.label || ""); setRenaming(true); }}
              >
                <Pencil size={14} />
              </button>
            </div>
          )}
          <div className="claude-profile-meta">
            <span className="claude-profile-status" data-ready={!cooling && account.status === "ready"}>
              {cooling
                ? `冷却中 · 剩余 ${formatCooldown(cooldownLeft)}`
                : account.status === "ready"
                  ? "就绪"
                  : account.status === "signed_out"
                    ? "需要登录"
                    : "待检查"}
            </span>
            {cooling && account.last_error_kind && (
              <span className="agent-cooldown-badge">
                <ShieldAlert size={12} />
                {account.last_error_kind}
              </span>
            )}
            {hasHealth && (
              <span
                className="agent-health-badge"
                data-tier={healthTier}
                title="实时 EWMA 健康度评分（根据成功调用与限流异常动态衰减恢复）"
              >
                <Activity size={11} aria-hidden="true" />
                健康度 {healthPct}%
              </span>
            )}
            <span className="claude-profile-email">
              {[
                account.email && account.email !== displayName && account.email,
                account.username && account.username !== displayName && `@${account.username}`,
                account.provider,
                account.key_hint && `密钥 ${account.key_hint}`,
                account.base_url,
                account.model && `模型 ${account.model}`,
              ].filter(Boolean).join(" · ")}
            </span>
          </div>
        </div>
        <label className="check-line claude-participation">
          <input
            type="checkbox"
            aria-label="参与自动调度"
            checked={account.enabled}
            disabled={busy}
            onChange={(e) => onMutate({ action: "update", id: account.id, enabled: e.target.checked })}
          />
          <span className="claude-switch-track" aria-hidden="true" />
          <span>{account.enabled ? "自动调度" : "已停用"}</span>
        </label>
      </div>

      <AgentQuotaMeters quota={account.quota} accentColor={spec.accent_color} now={now} />

      <div className="agent-account-metrics">
        <span>活跃终端：{activeCount || 0}{account.max_concurrency > 0 ? ` / ${account.max_concurrency}` : ""}</span>
        <span>调度权重：{account.weight || 10}</span>
        <span>累计调度：{account.total_allocations || 0} 次</span>
        {account.source === "default" && <span>来源：默认环境目录</span>}
      </div>

      {tuningOpen && (
        <form className="agent-account-tuning" onSubmit={saveTuning}>
          <label>
            调度权重 (1-100)
            <input type="number" min={1} max={100} value={weight} onChange={(e) => setWeight(e.target.value)} />
          </label>
          <label>
            并发上限 (0=不限)
            <input type="number" min={0} max={64} value={maxConc} onChange={(e) => setMaxConc(e.target.value)} />
          </label>
          <label>
            账号分组名（可用于项目绑定）
            <input maxLength={64} value={group} onChange={(e) => setGroup(e.target.value)} placeholder="例如 team-pro" />
          </label>
          {isAPIKey && (
            <>
              <label>
                轮换 API Key（留空保持原密钥）
                <input
                  type="password"
                  autoComplete="new-password"
                  value={newKey}
                  onChange={(e) => setNewKey(e.target.value)}
                  placeholder="留空不改，输入新密钥可替换"
                />
              </label>
              <label>
                Base URL（可选）
                <input value={newBaseURL} onChange={(e) => setNewBaseURL(e.target.value)} placeholder="https://..." spellCheck="false" />
              </label>
              <label>
                默认模型（可选）
                <input value={newModel} onChange={(e) => setNewModel(e.target.value)} placeholder="例如 o3" spellCheck="false" />
              </label>
              <label>
                供应商标识（可选）
                <input value={newProvider} onChange={(e) => setNewProvider(e.target.value)} placeholder="例如 OpenRouter / Azure" />
              </label>
              {(spec.auth?.api_key_env_vars?.length || 0) > 1 && (
                <label>
                  注入环境变量名
                  <InlineSelect
                    aria-label="注入环境变量名"
                    value={newEnvVar}
                    onValueChange={setNewEnvVar}
                  >
                    {spec.auth.api_key_env_vars.map((v) => (
                      <option key={v} value={v}>{v}</option>
                    ))}
                  </InlineSelect>
                </label>
              )}
            </>
          )}
          <div style={{ gridColumn: "1 / -1", display: "flex", gap: 8, justifyContent: "flex-end" }}>
            <button type="button" onClick={() => setTuningOpen(false)}>取消</button>
            <button type="submit" className="primary" disabled={busy}>保存参数</button>
          </div>
        </form>
      )}

      <div className="claude-profile-actions">
        <button
          type="button"
          className="quiet-button"
          disabled={busy}
          onClick={() => onLaunchWithAccount?.(spec.id, account.id)}
        >
          <TerminalSquare size={14} />
          用此账号打开终端
        </button>
        <button
          type="button"
          className="quiet-button"
          disabled={busy}
          onClick={() => setTuningOpen((v) => !v)}
        >
          <SlidersHorizontal size={14} />
          {tuningOpen ? "收起权重与分组" : "权重 / 并发 / 分组"}
        </button>
        {cooling ? (
          <button
            type="button"
            className="quiet-button"
            disabled={busy}
            onClick={() => onMutate({ action: "cooldown", id: account.id, cooldown_seconds: -1 })}
          >
            <ShieldCheck size={14} />
            立即恢复健康
          </button>
        ) : (
          <button
            type="button"
            className="quiet-button"
            disabled={busy}
            onClick={() => onMutate({ action: "cooldown", id: account.id, error_kind: "rate_limit", cooldown_seconds: 300 })}
          >
            <Flame size={14} />
            标记限流冷却 (5m)
          </button>
        )}
        {!isAPIKey && account.source !== "default" && (
          <button
            type="button"
            className="quiet-button"
            disabled={busy}
            onClick={() => onStartLogin?.(spec.id, account.id, "")}
          >
            <LogIn size={14} />
            重新登录
          </button>
        )}
        <button
          type="button"
          className="quiet-button"
          disabled={busy}
          onClick={() => onMutate({ action: "refresh", id: account.id })}
        >
          <RefreshCw size={14} />
          检查状态
        </button>
        <button
          type="button"
          className="text-button"
          disabled={busy}
          onClick={async () => {
            if (!await onConfirm({
              title: `移除 ${displayName}？`,
              body: isAPIKey
                ? "将从调度池移除该账号并清理其隔离 API Key 凭证文件。"
                : "将从调度池移除该账号。客机中的独立登录目录会保留。",
              confirm: "移除账号",
            })) return;
            await onMutate({ action: "remove", id: account.id });
          }}
        >
          <Trash2 size={14} />
          移除
        </button>
      </div>
    </article>
  );
}

export function AgentAccountsPanel({
  agentId,
  workspace,
  projects = [],
  projectGroups = [],
  running,
  environmentBusy,
  onConfirm,
  onStartLogin,
  onLaunchAgent,
  onOpenSettings,
  onChanged,
}) {
  const windowVisible = useWindowVisibility();
  const [state, setState] = useState(null);
  const [loading, setLoading] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const [showAPIKeyForm, setShowAPIKeyForm] = useState(false);
  const [now, setNow] = useState(() => Math.floor(Date.now() / 1000));

  const [keyLabel, setKeyLabel] = useState("");
  const [apiKeyValue, setApiKeyValue] = useState("");
  const [keyEnvVar, setKeyEnvVar] = useState("");
  const [keyBaseURL, setKeyBaseURL] = useState("");
  const [keyModel, setKeyModel] = useState("");
  const [keyProvider, setKeyProvider] = useState("");
  const [keyWeight, setKeyWeight] = useState("10");
  const [keyMaxConc, setKeyMaxConc] = useState("0");
  const [keyGroup, setKeyGroup] = useState("");

  const [sequencer] = useState(() => createAgentAccountsSequencer());
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);

  const load = useCallback(async (forceRefresh = false) => {
    const req = sequencer.begin();
    setLoading(true);
    try {
      const result = await api(agentAccountsPath(agentId, { workspace, forceRefresh }));
      if (sequencer.isCurrent(req)) {
        setState(result);
        setError("");
        setKeyEnvVar((prev) => prev || result.agent?.auth?.api_key_env_vars?.[0] || "");
      }
    } catch (e) {
      if (sequencer.isCurrent(req)) setError(e.message);
    } finally {
      if (sequencer.isCurrent(req)) setLoading(false);
    }
  }, [agentId, workspace, sequencer]);

  useEffect(() => {
    void load(false);
    return () => { sequencer.invalidate(); };
  }, [load, sequencer]);

  // Lightweight clock ONLY while at least one account is cooling down or has a live quota reset countdown.
  const hasCooldown = Boolean(state?.accounts?.some((a) => (a.cooldown_until || 0) > now));
  const hasQuotaCountdown = Boolean(state?.accounts?.some((a) =>
    (a.quota?.five_hour?.resets_at || 0) > now || (a.quota?.seven_day?.resets_at || 0) > now,
  ));
  useEffect(() => {
    if (!windowVisible || (!hasCooldown && !hasQuotaCountdown)) return;
    setNow(Math.floor(Date.now() / 1000));
    const intervalMs = hasCooldown ? 1000 : 30000;
    const timer = setInterval(() => setNow(Math.floor(Date.now() / 1000)), intervalMs);
    return () => clearInterval(timer);
  }, [hasCooldown, hasQuotaCountdown, windowVisible]);

  const mutate = async (body) => {
    const req = sequencer.beginMutation();
    setLoading(false);
    setSubmitting(true);
    setError("");
    try {
      const result = await api(agentAccountsPath(agentId, { workspace }), body);
      if (sequencer.isCurrent(req)) {
        setState(result);
      }
      onChanged?.();
      return result;
    } catch (e) {
      if (sequencer.isCurrent(req)) setError(e.message);
      throw e;
    } finally {
      const stillSubmitting = sequencer.endMutation();
      if (mounted.current) setSubmitting(stillSubmitting);
    }
  };

  const addAPIKey = async (e) => {
    e.preventDefault();
    if (!apiKeyValue.trim()) return;
    try {
      await mutate({
        action: "add_api_key",
        label: keyLabel.trim(),
        api_key: apiKeyValue.trim(),
        env_var: keyEnvVar.trim(),
        base_url: keyBaseURL.trim(),
        model: keyModel.trim(),
        provider: keyProvider.trim(),
        weight: Math.max(1, Math.min(100, Number(keyWeight) || 10)),
        max_concurrency: Math.max(0, Math.min(64, Number(keyMaxConc) || 0)),
        handoff_group: keyGroup.trim(),
      });
      setApiKeyValue("");
      setKeyLabel("");
      setKeyBaseURL("");
      setKeyModel("");
      setKeyProvider("");
      setKeyGroup("");
      setShowAPIKeyForm(false);
    } catch {
      // Error is displayed via state.
    }
  };

  const spec = state?.agent;
  const accounts = state?.accounts || [];
  const scheduling = state?.scheduling || { mode: "smart", project_affinity_minutes: 120, auto_cooldown: true, project_pins: {} };
  const schedulingBase = state?.scheduling_api >= 1 ? {} : scheduling;
  const preview = state?.schedule_preview;
  const busy = environmentBusy || submitting;

  const groups = useMemo(() => {
    return [...new Set(accounts.map((a) => a.handoff_group).filter(Boolean))];
  }, [accounts]);

  const pinWorkspaces = useMemo(() => {
    return [...new Set([workspace, ...projects, ...Object.keys(scheduling.project_pins || {})].filter(Boolean))];
  }, [workspace, projects, scheduling.project_pins]);

  const moveOrder = async (index, delta) => {
    const next = accounts.map((a) => a.id);
    const target = index + delta;
    if (target < 0 || target >= next.length) return;
    [next[index], next[target]] = [next[target], next[index]];
    await mutate({ action: "scheduling", scheduling: schedulingBase, order: next });
  };

  if (loading && !state) {
    return (
      <div className="agent-accounts-page">
        <p className="claude-profile-note"><LoaderCircle size={15} className="spin" /> 正在加载账号与调度信息…</p>
      </div>
    );
  }

  if (!spec) {
    return (
      <div className="agent-accounts-page">
        <div className="error-message" role="alert">{error || "未找到该 Agent 的配置信息。"}</div>
      </div>
    );
  }

  const methods = spec.auth?.methods || [];

  return (
    <div className="agent-accounts-page">
      <section className="agent-accounts-hero" aria-label={`${spec.name} 概览`}>
        <div className="agent-accounts-hero-id">
          <AgentIcon agentId={spec.id} glyph={spec.glyph} accentColor={spec.accent_color} size={38} />
          <div>
            <h3>{spec.name} 账号与智能调度</h3>
            <p>
              启动命令：<code>{state.launch_command}</code> · 共 {accounts.length} 个已管理账号
            </p>
          </div>
        </div>
        <div className="agent-accounts-hero-actions">
          <button
            type="button"
            className="primary"
            disabled={!running || busy}
            onClick={() => onLaunchAgent?.(spec.id, "")}
          >
            <TerminalSquare size={15} />
            新建 {spec.short_name} 终端
          </button>
          <button type="button" onClick={() => onOpenSettings?.()}>
            <Settings size={15} />
            终端与安装设置
          </button>
          <button type="button" disabled={loading || busy} onClick={() => load(true)} title="刷新账号状态与配额">
            <RefreshCw size={15} className={loading ? "spin" : undefined} />
          </button>
        </div>
      </section>

      <div className="agent-schedule-preview" role="status">
        <strong>下次新建终端调度预测：</strong>
        {state.magpie_takeover || preview?.strategy === "magpie" ? (
          <>
            <span className="agent-pill agent-pill--ok">Magpie 接管</span>
            <span>{preview?.reason || "已启用 Magpie，账号调度已自动关闭并由 Magpie 接管"}</span>
          </>
        ) : preview?.account_id ? (
          <>
            <span className="agent-pill agent-pill--ok">{preview.label}</span>
            <span>{preview.reason}</span>
          </>
        ) : (
          <span>{accounts.length === 0 ? "尚未添加多账号，将使用环境默认登录。" : preview?.reason || "暂无可用健康账号"}</span>
        )}
      </div>

      {state.login_notice && (
        <p className="claude-profile-note" role="status">{state.login_notice}</p>
      )}
      {error && <div className="error-message" role="alert">{error}</div>}

      <div className="agent-auth-bar">
        {methods.includes("oauth") && (
          <button
            type="button"
            className="primary"
            disabled={!running || busy}
            onClick={() => onStartLogin?.(spec.id, "", "")}
          >
            <LogIn size={15} />
            浏览器 OAuth 登录新账号
          </button>
        )}
        {methods.includes("device_code") && (
          <button
            type="button"
            disabled={!running || busy}
            onClick={() => onStartLogin?.(spec.id, "", "device")}
          >
            <Smartphone size={15} />
            设备码登录新账号
          </button>
        )}
        {methods.includes("api_key") && (
          <button
            type="button"
            aria-expanded={showAPIKeyForm}
            disabled={!running || busy}
            onClick={() => setShowAPIKeyForm((v) => !v)}
          >
            <KeyRound size={15} />
            {showAPIKeyForm ? "收起 API Key 表单" : "添加 API Key / 自定义网关"}
          </button>
        )}
      </div>

      {showAPIKeyForm && (
        <form className="agent-apikey-form" onSubmit={addAPIKey}>
          <div className="agent-apikey-grid">
            <label>
              账号备注名称
              <input
                value={keyLabel}
                onChange={(e) => setKeyLabel(e.target.value)}
                placeholder={`例如 ${spec.short_name} 公司 Key`}
                maxLength={64}
              />
            </label>
            <label>
              API Key（仅写入客机 0600 隔离文件，不落盘主机配置）
              <input
                required
                type="password"
                autoComplete="new-password"
                value={apiKeyValue}
                onChange={(e) => setApiKeyValue(e.target.value)}
                placeholder="sk-..."
              />
            </label>
            <label>
              注入环境变量名
              {(spec.auth?.api_key_env_vars?.length || 0) > 1 ? (
                <InlineSelect
                  aria-label="注入环境变量名"
                  value={keyEnvVar}
                  onValueChange={setKeyEnvVar}
                >
                  {spec.auth.api_key_env_vars.map((v) => (
                    <option key={v} value={v}>{v}</option>
                  ))}
                </InlineSelect>
              ) : (
                <input
                  required
                  value={keyEnvVar}
                  onChange={(e) => setKeyEnvVar(e.target.value)}
                  placeholder="OPENAI_API_KEY"
                  spellCheck="false"
                />
              )}
            </label>
            <label>
              自定义 Base URL（可选，支持企业网关）
              <input
                value={keyBaseURL}
                onChange={(e) => setKeyBaseURL(e.target.value)}
                placeholder="https://api.example.com/v1"
                spellCheck="false"
              />
            </label>
            <label>
              默认模型（可选）
              <input
                value={keyModel}
                onChange={(e) => setKeyModel(e.target.value)}
                placeholder="留空跟随 CLI 默认"
                spellCheck="false"
              />
            </label>
            <label>
              供应商标识（可选）
              <input
                value={keyProvider}
                onChange={(e) => setKeyProvider(e.target.value)}
                placeholder="例如 OpenRouter / Azure"
              />
            </label>
            <label>
              调度权重 (1-100)
              <input type="number" min={1} max={100} value={keyWeight} onChange={(e) => setKeyWeight(e.target.value)} />
            </label>
            <label>
              最大并发终端数 (0=不限)
              <input type="number" min={0} max={64} value={keyMaxConc} onChange={(e) => setKeyMaxConc(e.target.value)} />
            </label>
            <label>
              账号分组（可选）
              <input value={keyGroup} onChange={(e) => setKeyGroup(e.target.value)} placeholder="例如 prod" maxLength={64} />
            </label>
          </div>
          <div style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
            <button type="button" onClick={() => setShowAPIKeyForm(false)}>取消</button>
            <button type="submit" className="primary" disabled={busy || !apiKeyValue.trim()}>
              {submitting ? <LoaderCircle size={14} className="spin" /> : <Plus size={14} />}
              安全保存并加入调度池
            </button>
          </div>
        </form>
      )}

      <details className="claude-scheduling">
        <summary>调度设置 · {launchModeLabel(scheduling.mode, STRATEGY_LABELS)}</summary>
        <div className="agent-scheduling-grid">
          <LaunchAccountPolicy scheduling={scheduling} accounts={accounts} enabled={state.scheduling_api >= 1}
            busy={busy} label="自动调度策略" legacyModes={STRATEGY_LABELS} change={body => mutate(body).catch(() => {})} />
          <label>
            同项目缓存亲和窗口
            <InlineSelect
              aria-label="同项目缓存亲和窗口"
              value={String(scheduling.project_affinity_minutes ?? 120)}
              disabled={busy}
              onValueChange={(val) => mutate({
                action: "scheduling",
                scheduling: { ...schedulingBase, project_affinity_minutes: Number(val) },
              })}
            >
              <option value="0">关闭项目亲和</option>
              <option value="15">15 分钟</option>
              <option value="30">30 分钟</option>
              <option value="120">2 小时（推荐）</option>
              <option value="360">6 小时</option>
              <option value="1440">24 小时</option>
            </InlineSelect>
          </label>
        </div>

        <label className="check-line" style={{ marginTop: 10 }}>
          <input
            type="checkbox"
            checked={scheduling.auto_cooldown !== false}
            disabled={busy}
            onChange={(e) => mutate({
              action: "scheduling",
              scheduling: { ...schedulingBase, auto_cooldown: e.target.checked },
            })}
          />
          启用自动熔断冷却保护（限流/异常账号自动避让，指数退避后半开探测恢复）
        </label>

        {accounts.length > 1 && (
          <div style={{ marginTop: 12 }}>
            <strong style={{ fontSize: "0.84rem" }}>账号优先级顺序（用于顺序策略与同分决胜）</strong>
            <div className="claude-account-order">
              {accounts.map((a, idx) => (
                <div key={a.id}>
                  <span>{idx + 1}. {a.label || a.email || a.username || a.key_hint || a.id}</span>
                  <button type="button" className="icon-button" disabled={busy || idx === 0} aria-label="上移" onClick={() => moveOrder(idx, -1)}>
                    <ArrowUp size={14} />
                  </button>
                  <button type="button" className="icon-button" disabled={busy || idx === accounts.length - 1} aria-label="下移" onClick={() => moveOrder(idx, 1)}>
                    <ArrowDown size={14} />
                  </button>
                </div>
              ))}
            </div>
          </div>
        )}

        {pinWorkspaces.length > 0 && accounts.length > 0 && (
          <div className="agent-pins-list">
            <strong style={{ fontSize: "0.84rem" }}>项目专属绑定（优先级高于全局策略）</strong>
            {pinWorkspaces.map((ws) => {
              const currentPin = scheduling.project_pins?.[ws] || "";
              return (
                <div key={ws} className="agent-pin-row">
                  <span title={ws}>{projectName(ws, projectGroups)}</span>
                  <InlineSelect
                    aria-label={`${projectName(ws, projectGroups)} 绑定账号`}
                    value={currentPin}
                    disabled={busy}
                    onValueChange={(pinTarget) => mutate({ action: "pin_project", workspace: ws, pin_target: pinTarget })}
                  >
                    <option value="">跟随自动调度</option>
                    {accounts.map((a) => (
                      <option key={a.id} value={`account:${a.id}`}>
                        绑定账号：{a.label || a.email || a.username || a.key_hint}
                      </option>
                    ))}
                    {groups.map((g) => (
                      <option key={g} value={`group:${g}`}>
                        绑定分组：{g}
                      </option>
                    ))}
                  </InlineSelect>
                </div>
              );
            })}
          </div>
        )}
      </details>

      {state.detected && (
        <article className="claude-profile claude-detected" aria-label="已发现的默认登录">
          <div className="claude-profile-heading">
            <span className="claude-profile-avatar">
              <AgentIcon agentId={spec.id} glyph={spec.glyph} accentColor={spec.accent_color} size={28} />
            </span>
            <div className="claude-profile-identity">
              <div className="claude-profile-name">
                <h4>{state.detected.email || state.detected.username || state.detected.provider || `${spec.short_name} 默认登录`}</h4>
                {state.detected.plan && <span className="claude-profile-plan">{state.detected.plan}</span>}
              </div>
              <div className="claude-profile-meta">
                <span className="claude-profile-status" data-ready="true">默认环境已登录 · 未加入调度池</span>
              </div>
            </div>
          </div>
          <div className="claude-profile-actions">
            <button
              type="button"
              className="quiet-button"
              disabled={busy}
              onClick={() => mutate({ action: "join" })}
            >
              <Plus size={14} />
              加入智能调度池
            </button>
            <button
              type="button"
              className="quiet-button"
              disabled={busy}
              onClick={() => mutate({ action: "refresh_default" })}
            >
              <RefreshCw size={14} />
              重新检测
            </button>
          </div>
        </article>
      )}

      {accounts.length === 0 ? (
        <div className="claude-profile-empty">
          <AgentIcon agentId={spec.id} glyph={spec.glyph} accentColor={spec.accent_color} size={42} />
          <p>还没有登记 {spec.name} 账号</p>
          <small>
            你可以点击上方按钮通过浏览器 OAuth 登录、设备码登录或添加独立隔离 API Key，即可自动享受负载均衡、项目缓存亲和与限流熔断保护。
          </small>
        </div>
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
          {accounts.map((acc, idx) => (
            <AgentAccountCard
              key={acc.id}
              account={acc}
              index={idx}
              spec={spec}
              activeCount={state.active_counts?.[acc.id] || 0}
              now={now}
              busy={busy}
              onMutate={mutate}
              onLaunchWithAccount={onLaunchAgent}
              onStartLogin={onStartLogin}
              onConfirm={onConfirm}
            />
          ))}
        </div>
      )}
    </div>
  );
}
