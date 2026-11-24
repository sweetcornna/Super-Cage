import { shortcut } from "./platform.mjs";
import React, { useEffect, useRef, useState } from "react";
import {
  CircleAlert,
  ExternalLink,
  FlaskConical,
  Folder,
  FolderOpen,
  Globe2,
  Home,
  Monitor,
  MonitorSmartphone,
  Network,
  PanelLeftClose,
  Plus,
  RefreshCw,
  Settings,
  Server,
  SquareTerminal,
  UsersRound,
  Zap,
} from "lucide-react";
import { EnvironmentShield } from "./environment-shield";
import { BrandMark } from "./brand";
import claudeLogo from "./claude-logo.svg";
import { projectName } from "./workspace-state.mjs";
import { WorkspaceTabs } from "./workspace-tabs";
import { sessionKey } from "./session-order.mjs";
import { mixedSessionRows } from "./session-provider.mjs";
import { AccountMenu } from "./account";
import { useSSHNavigation } from "./remote-hosts";
import { GuestSystemIcon } from "./guest-system-icon";
import { getClientContext, isCloudWorkbench, isRemoteClient } from "./client-context.mjs";
import { VirtualMachines } from "./virtual-machines";
import { AgentIcon } from "./agent-icon";
import { isMagpieDirectAvailable } from "./magpie-panel";

const SESSION_PREVIEW_LIMIT = 5;

export function WorkspaceSidebar({
  workspace,
  guestSystem,
  projects,
  projectGroups = [],
  tabs,
  sessions = [],
  sessionError,
  sessionRefreshing,
  onSessionRefresh,
  onProjectRefresh,
  projectRefreshing,
  onSessionOpen,
  openingSession,
  sessionOpenError,
  onSessionArchive,
  onSessionPin,
  onSessionRename,
  saved,
  active,
  home,
  busy,
  ready,
  running,
  runtimeLabel,
  network,
  onHome,
  browserAvailable = true,
  editorAvailable = true,
  projectsAvailable = true,
  onOpenProject,
  onProjectSelect,
  onSelect,
  onClose,
  onRename,
  onPin,
  onSessionReorder,
  splitDrag,
  onNewTerminal,
  terminalMenuAt,
  onBrowser,
  onTailscale,
  tailscaleOpen,
  onAccounts,
  accountsOpen,
  accountsReadyCount = 0,
  agentsById = null,
  t3code = null,
  t3codeActive = false,
  onOpenT3Code,
  magpie = null,
  magpieActive = false,
  onOpenMagpie,
  herdr = null,
  herdrActive = false,
  onOpenHerdr,
  onInstallAgents,
  onClaudeWeb,
  onClaudeDesktop,
  desktopOpen,
  onClaudeScience,
  claudeScienceOpen,
  onEditor,
  onSettings,
  onAccountConfirm,
  onAccountStatus,
  onSafety,
  onCollapse,
  updateAction,
}) {
  const ssh = useSSHNavigation();
  const [showVirtualMachines, setShowVirtualMachines] = useState(false);
  const [collapsed, setCollapsed] = useState({});
  const [allSessionsShown, setAllSessionsShown] = useState({});
  const sessionBaseline = useRef(new Map());
  const activeFolder = tabs.find((tab) => tab.id === active)?.workspace;
  const activeWorkspace = projectGroups.find(group => group.folders.includes(activeFolder))?.workspace || activeFolder;
  useEffect(() => {
    if (activeWorkspace)
      setCollapsed((previous) => ({ ...previous, [activeWorkspace]: false }));
  }, [active, activeWorkspace]);
  if (ssh?.expanded) return null;
  const healthy = ready && network.mode === "open";
  return (
    <aside className="workspace-sidebar" aria-label="项目与终端">
      <div className="sidebar-top">
        <span className="sidebar-brand">
          <BrandMark size={22} className="brand-logo" />
          <span className="wordmark">Super Cage</span>
        </span>
        <button
          className="icon-button"
          onClick={onCollapse}
          aria-label="收起侧栏"
          title="收起侧栏"
        >
          <PanelLeftClose size={17} />
        </button>
      </div>
      <nav className="sidebar-navigation" aria-label="主导航">
        {/* Groups: work surfaces · connections · accounts. Room for more rows in each. */}
        <div className="sidebar-nav-group">
          <button
            className={`sidebar-link ${home ? "selected" : ""}`}
            onClick={onHome}
          >
            <Home size={17} />
            工作台
          </button>
          <div className={`sidebar-browser-row ${!desktopOpen && !home && !t3codeActive && !magpieActive && tabs.some((tab) => tab.id === active && tab.kind !== "terminal") ? "selected" : ""}`}>
            <button
              className="sidebar-link"
              onClick={onBrowser}
              disabled={!ready || busy || !browserAvailable}
              aria-label="打开浏览器"
              title="打开浏览器"
            >
              <Globe2 size={17} />
              浏览器
            </button>
            <button
              className="icon-button sidebar-claude-shortcut"
              onClick={onClaudeWeb}
              disabled={!ready || busy || !browserAvailable}
              aria-label="claude.ai"
              title="打开 claude.ai"
            >
              <img src={claudeLogo} width="18" height="18" alt="" />
            </button>
          </div>
          {t3code?.enabled && (() => {
            const t3DirectAvailable =
              t3code.direct_available !== false &&
              t3code.data?.direct_available !== false &&
              !isRemoteClient();
            const t3UsesWorkbench = !t3DirectAvailable || t3code.open_mode !== "direct";
            return (
              <div className={`sidebar-browser-row sidebar-t3code-row ${t3codeActive ? "selected" : ""}`}>
                <button
                  className="sidebar-link"
                  onClick={() => onOpenT3Code?.()}
                  disabled={busy || (!running && t3code.mode === "managed") || (t3UsesWorkbench && (!ready || !browserAvailable))}
                  aria-current={t3codeActive ? "page" : undefined}
                  aria-label="打开 T3 Code"
                  title={t3code.running ? "打开 T3 Code（运行中）" : "打开 T3 Code"}
                >
                  <AgentIcon agentId="t3code" size={17} />
                  <span className="sidebar-t3code-label">T3 Code</span>
                  {t3code.running && <i className="sidebar-t3code-dot" aria-hidden="true" title="运行中" />}
                </button>
                {t3DirectAvailable && (
                  <button
                    className="icon-button sidebar-claude-shortcut"
                    onClick={() => onOpenT3Code?.("direct")}
                    disabled={busy || (!running && t3code.mode === "managed")}
                    aria-label="原生极速直连打开 T3 Code"
                    title="原生极速直连窗口打开 T3 Code（0 VNC 延迟）"
                  >
                    <Zap size={14} />
                  </button>
                )}
              </div>
            );
          })()}
          {magpie?.enabled && (() => {
            const magpieDirectAvailable = isMagpieDirectAvailable(magpie);
            const magpieUsesWorkbench = !magpieDirectAvailable || magpie.open_mode !== "direct";
            return (
              <div className={`sidebar-browser-row sidebar-t3code-row ${magpieActive ? "selected" : ""}`}>
                <button
                  className="sidebar-link"
                  onClick={() => onOpenMagpie?.()}
                  disabled={busy || (!running && magpie.mode === "managed") || (magpieUsesWorkbench && (!ready || !browserAvailable))}
                  aria-current={magpieActive ? "page" : undefined}
                  aria-label="打开 Magpie"
                  title={magpie.running ? "打开 Magpie（运行中）" : "打开 Magpie"}
                >
                  <AgentIcon agentId="magpie" size={17} />
                  <span className="sidebar-t3code-label">Magpie</span>
                  {magpie.running && <i className="sidebar-t3code-dot" aria-hidden="true" title="运行中" />}
                </button>
                {magpieDirectAvailable && (
                  <button
                    className="icon-button sidebar-claude-shortcut"
                    onClick={() => onOpenMagpie?.("direct")}
                    disabled={busy || (!running && magpie.mode === "managed")}
                    aria-label="原生极速直连打开 Magpie"
                    title="原生极速直连窗口打开 Magpie（0 VNC 延迟）"
                  >
                    <Zap size={14} />
                  </button>
                )}
              </div>
            );
          })()}
          {herdr?.enabled && (
            <button
              className={`sidebar-link ${herdrActive ? "selected" : ""}`}
              onClick={() => onOpenHerdr?.()}
              disabled={busy || !running || !herdr.installed}
              aria-current={herdrActive ? "page" : undefined}
              aria-label="打开 Herdr 工作区"
              title={herdr.installed
                ? (herdr.server_running ? "打开 Herdr 持久终端工作区（运行中）" : "打开 Herdr 持久终端工作区")
                : "尚未在客机中安装 Herdr，请前往设置安装"}
            >
              <AgentIcon agentId="herdr" size={17} />
              <span className="sidebar-t3code-label">Herdr</span>
              {herdr.blocked_agents_count > 0 ? (
                <small className="sidebar-herdr-blocked" title={`${herdr.blocked_agents_count} 个 Agent 待处理`}>
                  {herdr.blocked_agents_count}
                </small>
              ) : herdr.server_running ? (
                <i className="sidebar-t3code-dot" aria-hidden="true" title="运行中" />
              ) : null}
            </button>
          )}
          <button className={`sidebar-link ${desktopOpen ? "selected" : ""}`} onClick={onClaudeDesktop} disabled={!running || busy} title="安装或打开 Claude Desktop"><Monitor size={17} />Claude Desktop</button>
          <button className={`sidebar-link ${claudeScienceOpen ? "selected" : ""}`} aria-current={claudeScienceOpen ? "page" : undefined} onClick={onClaudeScience} disabled={!running} title="安装或打开 Claude Science"><FlaskConical size={17} />Claude Science</button>
        </div>
        <div className="sidebar-nav-group">
          <button className={`sidebar-link ${tailscaleOpen ? "selected" : ""}`} aria-current={tailscaleOpen ? "page" : undefined} onClick={onTailscale} aria-label="打开 Tailscale"><Network size={17} />Tailscale</button>
          {getClientContext().virtual_machines && <button className="sidebar-link" aria-haspopup="dialog" onClick={() => setShowVirtualMachines(true)}><Monitor size={17} />虚拟机</button>}
          {ssh && <button className="sidebar-link ssh-navigation" onClick={ssh.open} aria-label="SSH 主机"><Server size={17} />SSH</button>}
          {/* 设备: the phones and computers that connect to this computer, and the computers this one joined. They reach this computer's workbench, so only a window on this computer shows it. */}
          {!isRemoteClient() && !isCloudWorkbench() && <button className="sidebar-link" onClick={() => onSettings("devices")}><MonitorSmartphone size={17} />设备</button>}
          {/* 本机终端 manages this computer's ssh access, so only a window on this computer shows it. */}
          {!isRemoteClient() && !isCloudWorkbench() && <button className="sidebar-link" onClick={() => onSettings("local-terminal")}><SquareTerminal size={17} />本机终端</button>}
        </div>
        <div className="sidebar-nav-group">
          <div className={`sidebar-browser-row ${accountsOpen ? "selected" : ""}`}>
            <button
              className="sidebar-link"
              onClick={onAccounts}
              aria-current={accountsOpen ? "page" : undefined}
              title="统一管理 Claude、Codex 及所有主流/自定义 Agent 账号"
            >
              <UsersRound size={17} />
              <span>账号管理</span>
              {accountsReadyCount > 0 && (
                <small className="sidebar-agent-count" title={`共 ${accountsReadyCount} 个就绪账号`}>
                  {accountsReadyCount}
                </small>
              )}
            </button>
            {onInstallAgents && (
              <button
                className="icon-button sidebar-claude-shortcut"
                onClick={onInstallAgents}
                aria-label="安装和管理 Agent CLI"
                title="安装或添加更多 Agent CLI（支持自定义如 Antigravity / Grok 等）"
              >
                <Plus size={15} />
              </button>
            )}
          </div>
        </div>
      </nav>
      <div className="sidebar-scroll">
        <div className="sidebar-section-heading">
          <span>项目</span>
          <span className="sidebar-section-actions">
            {onProjectRefresh && (
              <button
                className="icon-button"
                onClick={onProjectRefresh}
                aria-label="刷新项目文件"
                title="刷新项目文件：让环境立即看到在本机上所做的改动"
                disabled={busy || !running || projectRefreshing}
              >
                <RefreshCw size={14} className={projectRefreshing ? "spin" : undefined} />
              </button>
            )}
            <button
              className="icon-button"
              onClick={() => onOpenProject()}
              aria-label="添加项目"
              title={projectsAvailable ? "添加项目" : "当前环境尚未支持外部项目，文件保存在隔离工作目录中"}
              disabled={busy || !projectsAvailable}
            >
              <Plus size={15} />
            </button>
          </span>
        </div>
        {sessionError && (
          <div className="sidebar-session-error">
            <div role="alert">
              <div className="sidebar-session-error-heading">
                <CircleAlert size={16} aria-hidden="true" />
                <strong>会话列表暂未更新</strong>
              </div>
              <p>{sessionError}</p>
            </div>
            <button
              onClick={onSessionRefresh}
              disabled={sessionRefreshing || busy || !running}
              aria-busy={sessionRefreshing || undefined}
            >
              <RefreshCw size={14} aria-hidden="true" />
              {sessionRefreshing ? "正在读取…" : "重新读取会话"}
            </button>
          </div>
        )}
        {projects.map((path) => {
          const group = projectGroups.find(group => group.workspace === path);
          const folders = group?.folders || [path];
          const project = projectName(path, projectGroups);
          const rows = mixedSessionRows(tabs.filter(tab => tab.kind === "terminal" && folders.includes(tab.workspace)),
            sessions.filter(session => folders.includes(session.workspace)), saved, saved.sessionOrder?.[path], sessionBaseline.current)
            // A Codex session's account, on its history row or its open tab: "Codex · 工作".
            .map(row => {
              const session = row.historySession || (row.cli_session_provider === "codex" && sessions.find(item => item.id === row.cli_session_id));
              return session?.codexAccountLabel ? { ...row, claudeLabel: "Codex · " + session.codexAccountLabel } : row;
            });
          const rowMetadata = { ...saved };
          for (const row of rows) rowMetadata[row.id] = { ...saved[row.id], pinned: row.sessionPinned };
          const expanded = !collapsed[path];
          const showAllSessions = Boolean(allSessionsShown[path]);
          const visibleRows = showAllSessions ? rows : rows.slice(0, SESSION_PREVIEW_LIMIT);
          return (
            <section
              className="sidebar-project"
              key={path}
              aria-label={`项目 ${project}`}
            >
              <div
                className={`project-heading ${folders.includes(workspace) ? "current-project" : ""}`}
              >
                <button
                  className="project-disclosure"
                  title={folders.join("\n")}
                  aria-expanded={expanded}
                  onClick={() => {
                    setCollapsed((previous) => ({
                      ...previous,
                      [path]: expanded,
                    }));
                    setAllSessionsShown((previous) => ({ ...previous, [path]: false }));
                    onProjectSelect(path);
                  }}
                >
                  {expanded ? <FolderOpen size={16} /> : <Folder size={16} />}
                  <span>{project}</span>
                </button>
                <button
                  className="icon-button"
                  aria-label={`打开 ${project} 的编辑器`}
                  title="在 code-server 中打开项目目录"
                  disabled={!ready || busy || !editorAvailable}
                  onClick={() => onEditor(path)}
                >
                  <ExternalLink size={15} />
                </button>
                <button
                  className="icon-button"
                  aria-label={`在 ${project} 新建终端`}
                  title="新建终端"
                  aria-haspopup="menu"
                  data-terminal-location={path}
                  disabled={!running || busy}
                  onClick={(event) => {
                    setCollapsed((previous) => ({
                      ...previous,
                      [path]: false,
                    }));
                    onNewTerminal(path, event.currentTarget);
                  }}
                >
                  <SquareTerminal size={15} />
                </button>
              </div>
              {terminalMenuAt(path)}
              {expanded && folders.length > 1 && <div className="sidebar-project-folders" aria-label={`${project} 的文件夹`}>
                {folders.map((folder, index) => <button key={folder} title={folder} disabled={!ready || busy || !editorAvailable} onClick={() => onEditor(folder)} aria-label={`打开文件夹 ${projectName(folder)} 的编辑器`}><Folder size={13} /><span>{projectName(folder)}</span>{index === 0 && <small>默认</small>}</button>)}
              </div>}
              <div
                className={`project-children ${expanded ? "expanded" : ""}`}
                inert={!expanded}
              >
                <div>
                  {!!rows.length && (
                    <WorkspaceTabs
                      tabs={visibleRows}
                      saved={rowMetadata}
                      agentsById={agentsById}
                      openingSession={openingSession}
                      sessionOpenError={sessionOpenError}
                      active={home || desktopOpen ? "" : active}
                      label={`${project} 的终端`}
                      busy={busy}
                      ready={ready}
                      onSelect={(id, ...args) => {
                        const row = rows.find(row => row.id === id);
                        if (row?.historySession) onSessionOpen(row.historySession);
                        else onSelect(id, ...args);
                      }}
                      onClose={(row, anchor) => row.historySession ? onSessionArchive(row.historySession, anchor) : onClose(row, anchor)}
                      onRename={row => row.historySession ? onSessionRename(row.historySession) : onRename(row)}
                      onPin={id => {
                        const row = rows.find(row => row.id === id);
                        if (row?.historySession) onSessionPin(row.historySession);
                        else onPin(id);
                      }}
                      onReorder={(from, to) => {
                        const targetIndex = rows.findIndex(row => row.id === to);
                        const order = rows.map(row => row.id).filter(id => id !== from);
                        order.splice(targetIndex, 0, from);
                        onSessionReorder(path, order.map(id => sessionKey(rows.find(row => row.id === id))));
                      }}
                      splitDrag={splitDrag}
                    />
                  )}
                  {rows.length > SESSION_PREVIEW_LIMIT && (
                    <button
                      className="project-session-toggle"
                      aria-expanded={showAllSessions}
                      onClick={() => setAllSessionsShown((previous) => ({
                        ...previous,
                        [path]: !showAllSessions,
                      }))}
                    >
                      {showAllSessions ? "收起显示" : "展开显示"}
                    </button>
                  )}
                  {!rows.length && !running && (
                    <p className="sidebar-empty">启动后可新建终端</p>
                  )}
                </div>
              </div>
            </section>
          );
        })}
        {!projects.length && (
          <p className="sidebar-empty">{projectsAvailable ? "打开本地文件夹开始工作" : "使用 PowerShell 管理隔离工作目录中的文件"}</p>
        )}
      </div>
      <div className="sidebar-bottom">
        <button className="sidebar-status" onClick={onSafety}>
          <i className={`status-dot ${network.mode}`} />
          <span>{healthy ? "环境运行正常" : runtimeLabel}</span>
          <GuestSystemIcon system={guestSystem} />
          <EnvironmentShield size={17} healthy={healthy} />
        </button>
        <div className="sidebar-footer-actions">
          <AccountMenu onSettings={onSettings} onConfirm={onAccountConfirm} onStatus={onAccountStatus} busy={busy} />
          <button className="icon-button" aria-label="设置" title={shortcut("设置 ⌘,")} onClick={() => onSettings("keyboard")}><Settings size={17} /></button>
          {updateAction}
        </div>
      </div>
      {showVirtualMachines && <VirtualMachines onClose={() => setShowVirtualMachines(false)} />}
    </aside>
  );
}
