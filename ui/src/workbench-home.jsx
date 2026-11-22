import React, { useMemo } from "react";
import {
  FileCode2,
  FolderOpen,
  Globe2,
  LoaderCircle,
  Power,
  RefreshCw,
  TerminalSquare,
} from "lucide-react";
import { useAccount } from "./account";
import { Drawing, EmptyState } from "./drawings";
import { EnvironmentShield } from "./environment-shield";
import { GuestSystemPicker } from "./guest-system-picker";
import { MacOSLogin } from "./macos-login";
import { ProjectRecovery } from "./project-recovery";
import { ProviderMark } from "./provider-mark";
import { NetworkExplanation, networkTone } from "./network-explanation";
import { explanationView } from "./safety-state.mjs";
import { isCodexSession, ownerState } from "./session-provider.mjs";
import { SafetyMessage } from "./safety-message";
import { SetupProgress } from "./setup-progress";
import { isCloudWorkbench, isRemoteClient } from "./client-context.mjs";
import { projectName } from "./workspace-state.mjs";

const RECENT_LIMIT = 5;

export function greeting(date = new Date()) {
  const hour = date.getHours();
  return hour < 5 ? "夜深了" : hour < 11 ? "早上好" : hour < 13 ? "中午好" : hour < 18 ? "下午好" : "晚上好";
}

const dayFormat = new Intl.DateTimeFormat("zh-CN", { month: "numeric", day: "numeric" });
export function relativeTime(value, now = Date.now()) {
  const time = Date.parse(value);
  if (!time) return "";
  const minutes = Math.floor((now - time) / 60000);
  if (minutes < 1) return "刚刚";
  if (minutes < 60) return minutes + " 分钟前";
  if (minutes < 24 * 60) return Math.floor(minutes / 60) + " 小时前";
  if (minutes < 48 * 60) return "昨天";
  return dayFormat.format(time);
}

// The workbench home (brand/design-language.md §6): a greeting, one calm
// environment card with one primary action, then recent sessions and projects.
export function WorkbenchHome({
  status, storageRecovery, setupProgress, busy, running, setupProxy, network, runtimeLabel,
  nativeGuestLabel, guestSystem, onGuestSystem, account: guestAccount, connectionError, workspace,
  projectsAvailable, macGuest, editorAvailable, contentWorkspace, groups, projects, sessions, tabs,
  openingSession, onSessionOpen, onSettings, onDetails, onStop, onRetrySetup, onPrimary, primaryDisabled,
  onNewTerminal, onNewTerminalHere, onEditor, onOpenProject, terminalMenuAt,
  sessionOpenError, canRecheck, onRechecked, onNetworkDetails, onResources,
}) {
  const { account } = useAccount();
  const hour = new Date().getHours();
  const night = hour < 6 || hour >= 18;
  const ready = running && status.ready;
  const stopped = !busy && !running && !storageRecovery && !setupProgress;
  const recent = useMemo(() => [...sessions]
    .filter((session) => session.title)
    .sort((a, b) => Number(Boolean(b.active)) - Number(Boolean(a.active)) || String(b.updated_at || "").localeCompare(String(a.updated_at || "")))
    .slice(0, RECENT_LIMIT), [sessions]);
  const projectRows = useMemo(() => projects.map((path) => {
    const folders = groups.find((group) => group.workspace === path)?.folders || [path];
    const own = sessions.filter((session) => folders.includes(session.workspace));
    const live = tabs.filter((tab) => tab.kind === "terminal" && !tab.closed && folders.includes(tab.workspace));
    return { path, name: projectName(path, groups), folders, sessions: own.length,
      working: own.filter((session) => session.active).length, terminals: live.length };
  }), [projects, groups, sessions, tabs]);

  const title = storageRecovery ? "环境存储需要恢复"
    : setupProgress ? (status.job.error ? "环境安装未完成" : "正在安装 Super Cage 环境")
    : busy ? status.job.kind === "stop" ? "正在停止 Super Cage 环境"
      : status.job.kind === "storage" ? "正在调整环境存储"
      : status.job.kind === "project" ? "正在打开项目"
      : "正在准备 Super Cage 环境"
    : running ? !status.ready ? "Super Cage 环境正在连接" : network.mode === "closed" ? "Super Cage 环境已就绪 · 外网暂停" : "Super Cage 环境已就绪"
    : setupProxy ? "为 Claude 准备独立工作区" : "先启动 Super Cage 环境";
  const subtitle = busy ? status.job.message
    : running ? !status.ready
      ? status.environment_update_error || "浏览器连接尚未就绪，可以恢复连接或继续使用终端。"
      : network.mode === "open"
        ? status.guest_system === "windows-remote" ? "环境已连接，网络检查通过。可以浏览网页或使用隔离 PowerShell。" : "环境已连接，网络检查通过。可以浏览网页或打开本地项目。"
        : `${network.label}。终端与${status.guest_system === "windows-remote" ? "隔离工作目录" : "本地文件"}仍可使用。`
    : setupProxy ? "先连接你的代理，再启动浏览器、终端和编辑器。无需安装额外的代理软件。"
    : "准备独立的浏览器和终端，完成网络检查后开始工作。";
  const PrimaryIcon = busy ? null : !running && setupProxy ? Globe2 : !running ? Power
    : !status.ready ? RefreshCw : !workspace && projectsAvailable ? FolderOpen : TerminalSquare;
  const primaryLabel = busy ? "正在处理…"
    : !running ? setupProxy ? "配置网络与代理"
      : guestSystem?.activate ? "打开工作台" : guestSystem?.pending ? "继续安装"
      : guestSystem && guestSystem.guest_system !== "debian-13" ? "安装客户机" : "启动 Super Cage 环境"
    : !status.ready ? "恢复浏览器" : !workspace && projectsAvailable ? "打开项目" : "新建终端";
  // The server's explanation of a closed or uncertain gate (calm unless the
  // user must act); an older server sends none and the card stays as it was.
  const gateTone = running && !busy ? networkTone(network) : "";
  // Named as in the status bar once the gate explains itself.
  const statusLabel = busy ? runtimeLabel : running ? gateTone && explanationView(network.explanation)?.label || network.label
    : setupProxy ? "等待配置代理" : "环境已停止";
  const art = storageRecovery ? "plug" : ready ? null : "server";
  // A terminal can still be opened when the browser is down or no project is chosen.
  const terminalShortcut = running && (!status.ready || (!workspace && projectsAvailable));

  return (
    <div className="welcome">
      <div className="welcome-content">
        <header className="home-hero">
          <p className="home-greeting">{greeting()}{account?.username ? "，" + account.username : ""}</p>
          <Drawing name={night ? "moon" : "sun"} className="home-daylight" />
          <Drawing name="squiggle" className="home-underline" />
        </header>

        <section className={"home-environment" + (stopped ? " nudge-card" : "") + (ready ? " is-ready" : "")} aria-labelledby="home-environment-title">
          {art && <Drawing name={art} className="home-environment-art" />}
          <div className="home-environment-body">
            <div className="home-environment-title">
              {ready && <EnvironmentShield healthy={status.ready && network.mode === "open"} size={20} />}
              <h1 id="home-environment-title">{title}</h1>
            </div>
            {storageRecovery ? <>
              <p className="welcome-subtitle"><SafetyMessage message={status.storage_recovery.error} /></p>
              <p className="field-help">在设置中恢复环境存储后，再启动浏览器和终端。诊断包可以从同一页面导出。</p>
              <div className="start-project-footer">
                <span />
                <button className="primary" onClick={() => onSettings("project")}>恢复环境存储</button>
              </div>
            </> : setupProgress ? (
              <SetupProgress job={status.job} onDetails={onDetails} onStop={onStop} onRetry={onRetrySetup}
                disconnected={Boolean(connectionError)} />
            ) : <>
              <p className="welcome-subtitle"><SafetyMessage message={subtitle} /></p>
              {gateTone && <NetworkExplanation compact explanation={network.explanation} canRecheck={canRecheck}
                onRechecked={onRechecked} onDetails={onNetworkDetails} onResources={onResources} />}
              <p className="home-environment-meta">
                Super Cage 隔离环境 · {running ? runtimeLabel
                  : setupProxy ? "自备代理订阅、YAML 配置或节点"
                  : nativeGuestLabel ? `${nativeGuestLabel} 客机已安装` : "代理已配置，首次启动会下载开发环境"}
              </p>
              {!running && !nativeGuestLabel && (
                <ol className="onboarding-steps" aria-label="开始使用 Super Cage">
                  {["配置代理", "启动环境", "开始工作"].map((label, index) => (
                    <li key={label} aria-current={index === (setupProxy ? 0 : 1) ? "step" : undefined}>
                      <span>{index + 1}</span>
                      {label}
                    </li>
                  ))}
                </ol>
              )}
              {!busy && !running && !guestAccount?.guest_system && status.local_vm_storage && !isRemoteClient() &&
                <GuestSystemPicker system={guestSystem} onChange={onGuestSystem} />}
              {!running && !nativeGuestLabel && status.local_vm_storage && !isRemoteClient() &&
                <button className="storage-entry text-button" disabled={busy} onClick={() => onSettings("project")}>选择环境存储位置</button>}
              <div className="start-project-footer">
                <span className="home-status">
                  <i className={`status-dot ${gateTone && gateTone !== "alarm" ? "network-" + gateTone : network.mode}`} />
                  {statusLabel}
                </span>
                {terminalShortcut && (
                  <button className="home-secondary" onClick={onNewTerminalHere} data-terminal-location="shortcuts"
                    aria-haspopup="menu" disabled={busy}>
                    <TerminalSquare size={16} />
                    新建终端
                  </button>
                )}
                {running && (
                  <button className="home-secondary" onClick={() => onEditor(contentWorkspace)}
                    disabled={!status.ready || busy || !editorAvailable}
                    title={editorAvailable ? undefined : "当前环境尚未接通文件编辑器，可使用 PowerShell 管理隔离工作目录中的文件"}>
                    <FileCode2 size={16} />
                    浏览文件
                  </button>
                )}
                <button className={stopped ? "clay large" : "primary"} data-terminal-location="start"
                  disabled={primaryDisabled} onClick={onPrimary}>
                  {busy ? <LoaderCircle size={16} className="spin" /> : <PrimaryIcon size={16} />}
                  {primaryLabel}
                </button>
              </div>
              {terminalMenuAt("start")}
              {terminalMenuAt("shortcuts")}
              {running && macGuest && <div className="welcome-shortcuts"><MacOSLogin disabled={!status.ready || busy} /></div>}
            </>}
          </div>
        </section>

        {!running && !nativeGuestLabel && !storageRecovery && !setupProgress && (
          <p className="welcome-hint">
            {setupProxy
              ? "Super Cage 不提供代理或 Claude 账户。连接后，在 Super Cage 内完成 Claude 登录。"
              : "首次安装时可在操作详情中查看下载和配置进度；中断后可以继续安装。"}
          </p>
        )}

        {status.project_error && (
          <div className="start-error" role="alert">
            <strong>部分项目文件夹尚未连接</strong>
            <p>环境和浏览器可继续使用。</p>
            {status.project_errors?.length ? (
              <ul className="project-connection-errors">
                {status.project_errors.map(({ path, message, recovery }) => (
                  <li key={path}>
                    <strong>{projectName(path, groups)}</strong>
                    <span className="project-error-path">{path}</span>
                    <p>{message}</p>
                    <ProjectRecovery recovery={recovery} disabled={!status.ready || busy}
                      onRetry={() => onOpenProject(path)} onSettings={() => onSettings("project")} />
                  </li>
                ))}
              </ul>
            ) : <p>{status.project_error}</p>}
          </div>
        )}
        {!storageRecovery && !setupProgress && status.job?.error && (
          <div className="start-error" role="alert">
            <strong>{status.job.message || "本次操作未完成"}</strong>
            <p><SafetyMessage message={status.job.error} /></p>
            {status.can_restore && (
              <button className="text-button" onClick={() => onSettings("project")}>查看恢复选项</button>
            )}
          </div>
        )}
        {!setupProgress && status.job?.log && (
          <button className="text-button details-link" onClick={onDetails}>查看操作详情</button>
        )}

        {ready && (
          <div className="home-lists">
            <div className="home-section">
              <h2 className="section-label">最近会话</h2>
              {recent.length ? (
                <ul className="list home-recent">
                  {recent.map((session) => {
                    // Codex rows never grey out: another device's session opens read-only.
                    const codex = isCodexSession(session);
                    const owner = codex ? ownerState(session) : null;
                    const elsewhere = !codex && session.running && !session.active;
                    const opening = openingSession === session.id;
                    const openError = sessionOpenError?.id === session.id ? sessionOpenError.message : "";
                    return (
                      <li key={session.id}>
                        <button className="home-row" aria-label={`继续会话「${session.title}」`}
                          disabled={busy || opening || elsewhere}
                          title={elsewhere ? "其他窗口占用中，退出原会话后可接续" : owner?.muted ? `${session.title}\n${owner.tooltip}` : session.title}
                          aria-describedby={openError ? "home-open-error-" + session.id : undefined}
                          onClick={() => onSessionOpen(session)}>
                          <ProviderMark provider={codex ? "codex" : "claude"} accountNumber={session.claudeAccountNumber} className="home-row-icon" />
                          <span className="list-row-text">
                            <span className="list-row-title">{session.title}</span>
                            <span className="list-row-meta">
                              {[projectName(session.workspace, groups), session.codexAccountLabel, relativeTime(session.updated_at)].filter(Boolean).join(" · ")}
                            </span>
                          </span>
                          {opening ? <LoaderCircle size={14} className="spin" aria-hidden="true" />
                            : owner?.muted ? <span className="home-owner">{owner.live && <i className="live-dot" />}<span className="chip">{owner.label}</span></span>
                            : session.active ? <span className="home-working"><i className="live-dot" />正在工作</span>
                            : elsewhere ? <span className="chip">其他窗口</span> : null}
                        </button>
                        {openError && <p id={"home-open-error-" + session.id} className="home-row-error" role="alert">{openError}</p>}
                      </li>
                    );
                  })}
                </ul>
              ) : (
                <EmptyState compact drawing="cup" title="还没有会话">
                  新建一个 Claude 终端，对话会出现在这里，随时可以接着做。
                </EmptyState>
              )}
            </div>
            <div className="home-section">
              <h2 className="section-label">项目</h2>
              <ul className="list home-projects">
                {projectRows.map((project) => (
                  <li key={project.path}>
                    <div className="home-row home-project">
                      <FolderOpen size={16} className="home-row-icon" />
                      <span className="list-row-text" title={project.folders.join("\n")}>
                        <span className="list-row-title">{project.name}</span>
                        <span className="list-row-meta">
                          {project.sessions ? project.sessions + " 个会话" : "还没有会话"}
                          {project.working ? " · " + project.working + " 个正在工作" : ""}
                          {project.terminals ? " · " + project.terminals + " 个终端已打开" : ""}
                        </span>
                      </span>
                      <button className="icon-button" aria-label={`浏览 ${project.name} 的文件`} title="浏览文件"
                        disabled={!status.ready || busy || !editorAvailable} onClick={() => onEditor(project.path)}>
                        <FileCode2 size={16} />
                      </button>
                      <button className="icon-button" aria-label={`为 ${project.name} 新建终端`} title="新建终端"
                        aria-haspopup="menu" data-terminal-location={"home:" + project.path} disabled={busy}
                        onClick={(event) => onNewTerminal(project.path, event.currentTarget)}>
                        <TerminalSquare size={16} />
                      </button>
                    </div>
                    {terminalMenuAt("home:" + project.path)}
                  </li>
                ))}
              </ul>
              {projectsAvailable && (
                <button className="text-button home-add-project" disabled={busy} onClick={() => onOpenProject("")}>打开其他文件夹…</button>
              )}
            </div>
          </div>
        )}
        {ready && (
          <p className="welcome-hint">
            {status.guest_system === "windows-remote" ? "PowerShell 和浏览器无需 Claude 登录。只有使用 Claude 时才需要登录账户。" : <>打开项目后，在终端输入 <code>claude</code>，按提示登录你的账户。</>}
            {" "}
            {status.guest_system === "windows-remote" ? "文件保存在云端隔离工作目录中，不会挂载你的本地文件夹。" : isCloudWorkbench() ? "打开的项目位于云端，编辑和删除会直接影响云端文件。" : "打开的项目与本机共享，编辑和删除文件会同步。"}
          </p>
        )}
      </div>
    </div>
  );
}
