import React, { useMemo, useRef } from "react";
import { Plus } from "lucide-react";
import { ProviderMark } from "./provider-mark";
import { AgentIcon } from "./agent-icon";
import { ClaudeAccountsPanel } from "./claude-accounts";
import { CodexAccountsPanel } from "./codex-accounts";
import { AgentAccountsPanel } from "./agent-accounts";
import "./accounts-hub.css";

export function AccountsHub({
  activeTab,
  onSelectTab,
  claudeProfiles,
  codexProfiles,
  codexNative = false,
  agents = [],
  agentAccountsRevision = 0,
  tabs = [],
  workspace = "",
  projects = [],
  projectGroups = [],
  running = false,
  environmentBusy = false,
  onConfirm,
  onClaudeLogin,
  onCodexLogin,
  onStartAgentLogin,
  onLaunchAgent,
  onOpenSettings,
  onAgentChanged,
}) {
  const tabListRef = useRef(null);

  const claudeReady = useMemo(
    () => (claudeProfiles?.accounts || []).filter((a) => a.status === "ready" || a.ready).length,
    [claudeProfiles?.accounts],
  );
  const claudeTotal = (claudeProfiles?.accounts || []).length;

  const codexReady = useMemo(
    () => (codexProfiles?.accounts || []).filter((a) => a.status === "ready" || a.ready).length,
    [codexProfiles?.accounts],
  );
  const codexTotal = (codexProfiles?.accounts || []).length;

  const providerTabs = useMemo(() => {
    const items = [
      {
        id: "claude",
        label: "Claude",
        kind: "claude",
        ready: claudeReady,
        total: claudeTotal,
      },
    ];
    const codexCatalogEntry = agents.find((entry) => entry.spec?.id === "codex");
    if (codexNative || codexCatalogEntry?.runtime?.installed || (codexCatalogEntry?.accounts?.total || 0) > 0) {
      items.push({
        id: "codex",
        label: "Codex",
        kind: codexNative ? "codex-native" : "agent",
        spec: codexCatalogEntry?.spec || null,
        ready: codexNative ? codexReady : (codexCatalogEntry?.accounts?.ready || 0),
        total: codexNative ? codexTotal : (codexCatalogEntry?.accounts?.total || 0),
      });
    }
    const seen = new Set(items.map((item) => item.id));
    for (const entry of agents) {
      const id = entry.spec?.id;
      if (!id || seen.has(id)) continue;
      const installed = Boolean(entry.runtime?.installed);
      const hasAccounts = (entry.accounts?.total || 0) > 0;
      const isSelected = activeTab === id;
      if (!installed && !hasAccounts && !isSelected) continue;
      seen.add(id);
      items.push({
        id,
        label: entry.spec.short_name || entry.spec.name || id,
        kind: "agent",
        spec: entry.spec,
        ready: entry.accounts?.ready || 0,
        total: entry.accounts?.total || 0,
      });
    }
    return items;
  }, [agents, activeTab, claudeReady, claudeTotal, codexNative, codexReady, codexTotal]);

  const currentTab = providerTabs.find((item) => item.id === activeTab) || providerTabs[0];

  const onTabKeyDown = (event, index) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    const nextIndex =
      event.key === "Home"
        ? 0
        : event.key === "End"
          ? providerTabs.length - 1
          : (index + (event.key === "ArrowRight" ? 1 : -1) + providerTabs.length) % providerTabs.length;
    const next = providerTabs[nextIndex];
    if (!next) return;
    onSelectTab(next.id);
    const buttons = tabListRef.current?.querySelectorAll('[role="tab"]');
    buttons?.[nextIndex]?.focus();
  };

  return (
    <div className="accounts-hub">
      <div className="accounts-hub-bar">
        <div className="accounts-hub-tabs" role="tablist" aria-label="Agent 账号分类" ref={tabListRef}>
          {providerTabs.map((item, index) => {
            const selected = currentTab.id === item.id;
            return (
              <button
                key={item.id}
                type="button"
                role="tab"
                id={`accounts-tab-${item.id}`}
                aria-selected={selected}
                aria-controls={`accounts-panel-${item.id}`}
                tabIndex={selected ? 0 : -1}
                className={`accounts-hub-tab ${selected ? "selected" : ""}`}
                onClick={() => onSelectTab(item.id)}
                onKeyDown={(event) => onTabKeyDown(event, index)}
              >
                {item.id === "claude" || item.id === "codex" ? (
                  <ProviderMark provider={item.id} decorative />
                ) : (
                  <AgentIcon
                    agentId={item.id}
                    glyph={item.spec?.glyph}
                    accentColor={item.spec?.accent_color}
                    size={16}
                  />
                )}
                <span className="accounts-hub-tab-label">{item.label}</span>
                {item.total > 0 && (
                  <small
                    className="accounts-hub-tab-count"
                    title={`${item.ready} 个就绪 / 共 ${item.total} 个账号`}
                  >
                    {item.ready}/{item.total}
                  </small>
                )}
              </button>
            );
          })}
        </div>
        {onOpenSettings && (
          <button
            type="button"
            className="accounts-hub-add"
            onClick={onOpenSettings}
            title="安装主流 Agent CLI 或注册自定义 Agent（如 Antigravity / Grok 等）"
          >
            <Plus size={14} />
            <span>添加 / 管理 Agent</span>
          </button>
        )}
      </div>
      <div
        className="accounts-hub-panel"
        role="tabpanel"
        id={`accounts-panel-${currentTab.id}`}
        aria-labelledby={`accounts-tab-${currentTab.id}`}
      >
        {currentTab.kind === "claude" ? (
          <ClaudeAccountsPanel
            store={claudeProfiles}
            tabs={tabs}
            onConfirm={onConfirm}
            running={running}
            environmentBusy={environmentBusy}
            login={onClaudeLogin}
          />
        ) : currentTab.kind === "codex-native" ? (
          <CodexAccountsPanel
            store={codexProfiles}
            tabs={tabs}
            onConfirm={onConfirm}
            running={running}
            environmentBusy={environmentBusy}
            login={onCodexLogin}
          />
        ) : (
          <AgentAccountsPanel
            key={`${currentTab.id}:${agentAccountsRevision}`}
            agentId={currentTab.id}
            workspace={workspace}
            projects={projects}
            projectGroups={projectGroups}
            running={running}
            environmentBusy={environmentBusy}
            onConfirm={onConfirm}
            onStartLogin={onStartAgentLogin}
            onLaunchAgent={onLaunchAgent}
            onOpenSettings={onOpenSettings}
            onChanged={onAgentChanged}
          />
        )}
      </div>
    </div>
  );
}
