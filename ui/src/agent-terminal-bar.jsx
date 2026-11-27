import React, { useEffect, useState } from "react";
import { Flame, RefreshCw, TerminalSquare, UsersRound } from "lucide-react";
import { api } from "./api";
import { AgentIcon } from "./agent-icon";
import { InlineSelect } from "./inline-select";
import "./agent-accounts.css";

// Compact per-terminal account control bar for universal Agent CLIs.
// Supports instant 1-click rate-limit cooldown + failover to the optimal healthy account.
export function AgentTerminalBar({
  session,
  agentSpec = null,
  onLaunchAgent,
  onOpenAccounts,
  onStatus,
}) {
  const [open, setOpen] = useState(false);
  const [accounts, setAccounts] = useState([]);
  const [targetId, setTargetId] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const agentId = session.agent_id;
  useEffect(() => {
    if (!open || !agentId) return;
    let live = true;
    api(`/agent-clis/${encodeURIComponent(agentId)}/accounts?skip_detect=1`)
      .then((res) => {
        if (!live) return;
        const list = res.accounts || [];
        setAccounts(list);
        const others = list.filter((a) => a.id !== session.account_id);
        setTargetId((prev) => (others.some((a) => a.id === prev) ? prev : others[0]?.id || ""));
        setError("");
      })
      .catch((e) => { if (live) setError(e.message); });
    return () => { live = false; };
  }, [open, agentId, session.account_id]);

  if (!agentId || session.terminalKind !== "agent") return null;

  const rateLimited = Boolean(session.rate_limit_detected);

  const markRateLimitedAndFailover = async () => {
    if (busy) return;
    setBusy(true);
    setError("");
    try {
      if (session.account_id) {
        await api(`/agent-clis/${encodeURIComponent(agentId)}/accounts`, {
          action: "cooldown",
          id: session.account_id,
          error_kind: "rate_limit",
          cooldown_seconds: 300,
        });
      }
      setOpen(false);
      onStatus?.("已将当前账号置入 5 分钟限流冷却，正在路由至最优健康账号…");
      await onLaunchAgent?.(agentId, "", session.workspace);
    } catch (e) {
      setError(e.message);
    } finally {
      setBusy(false);
    }
  };

  const launchWithTarget = async () => {
    if (busy || !targetId) return;
    setBusy(true);
    setError("");
    try {
      setOpen(false);
      await onLaunchAgent?.(agentId, targetId, session.workspace);
    } catch (e) {
      setError(e.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section
      className="agent-terminal-bar"
      data-rate-limited={rateLimited || undefined}
      aria-label={`${session.agent_name || agentId} 账号控制栏`}
    >
      <div className="agent-terminal-bar-row">
        <button
          type="button"
          className="agent-terminal-bar-trigger"
          aria-expanded={open}
          onClick={() => setOpen((v) => !v)}
        >
          <AgentIcon
            agentId={agentId}
            accentColor={agentSpec?.accent_color}
            size={16}
            decorative
          />
          <span>{session.agent_name || agentId}</span>
          <span>·</span>
          <span>{session.account_label || "默认环境"}</span>
        </button>
        {rateLimited ? (
          <>
            <span
              className="agent-terminal-rate-badge"
              role="status"
              title={session.rate_limit_message || "检测到速率限制或配额耗尽"}
            >
              <Flame size={12} aria-hidden="true" />
              检测到限流
            </span>
            {session.rate_limit_message && (
              <span className="agent-terminal-bar-reason" title={session.rate_limit_message}>
                {session.rate_limit_message}
              </span>
            )}
            <button
              type="button"
              className="agent-terminal-failover-btn"
              disabled={busy}
              onClick={markRateLimitedAndFailover}
            >
              {busy ? <RefreshCw size={12} className="spin" /> : <Flame size={12} />}
              冷却并切换最优账号
            </button>
          </>
        ) : (
          session.schedule_reason && (
            <span className="agent-terminal-bar-reason" title={session.schedule_reason}>
              {session.schedule_reason}
            </span>
          )
        )}
      </div>
      {open && (
        <div className="agent-terminal-bar-panel">
          <div className="agent-terminal-bar-actions">
            {session.account_id && (
              <button
                type="button"
                className="primary"
                disabled={busy}
                onClick={markRateLimitedAndFailover}
              >
                {busy ? <RefreshCw size={14} className="spin" /> : <Flame size={14} />}
                遇到限流：冷却当前账号并用最优账号重开
              </button>
            )}
            <button
              type="button"
              onClick={() => { setOpen(false); onOpenAccounts?.(agentId); }}
            >
              <UsersRound size={14} />
              前往账号与调度管理
            </button>
          </div>
          {accounts.filter((a) => a.id !== session.account_id).length > 0 && (
            <div className="agent-terminal-bar-actions">
              <div style={{ minWidth: 200, flex: "1 1 200px" }}>
                <InlineSelect
                  aria-label="指定其他账号"
                  value={targetId}
                  disabled={busy}
                  onValueChange={setTargetId}
                >
                  {accounts
                    .filter((a) => a.id !== session.account_id)
                    .map((a) => (
                      <option key={a.id} value={a.id}>
                        {a.label || a.email || a.username || a.key_hint || a.id}
                      </option>
                    ))}
                </InlineSelect>
              </div>
              <button type="button" disabled={busy || !targetId} onClick={launchWithTarget}>
                <TerminalSquare size={14} />
                用所选账号新建终端
              </button>
            </div>
          )}
          {error && <p className="error-message" role="alert" style={{ margin: 0 }}>{error}</p>}
        </div>
      )}
    </section>
  );
}
