import React, { useEffect, useRef, useState } from "react";
import {
  Check,
  Download,
  ExternalLink,
  LoaderCircle,
  PanelTop,
  Play,
  RefreshCw,
  RotateCw,
  Settings,
  SlidersHorizontal,
  Square,
  TerminalSquare,
  Trash2,
  X,
  Zap,
} from "lucide-react";
import { isRemoteClient } from "./client-context.mjs";
import { AgentIcon } from "./agent-icon";
import { InlineSelect } from "./inline-select";
import "./t3code.css";
import "./magpie-herdr.css";

const DEFAULT_WEB_PORT = 3430;
const DEFAULT_GATEWAY_PORT = 3425;

export function isMagpieManagedSupported(magpie, status = null) {
  if (magpie?.supports_managed === false || magpie?.data?.supports_managed === false) {
    return false;
  }
  if (
    (status?.guest_system === "windows-11" || status?.guest_system === "windows-remote") &&
    magpie?.supports_managed !== true &&
    magpie?.data?.supports_managed !== true
  ) {
    return false;
  }
  return true;
}

function isDesktopWebView() {
  if (typeof window === "undefined") return false;
  return Boolean(window.webkit?.messageHandlers?.cage || window.chrome?.webview);
}

export function isMagpieDirectAvailable(magpie) {
  if (magpie?.direct_available === false || magpie?.data?.direct_available === false) {
    return false;
  }
  if (isDesktopWebView()) {
    return false;
  }
  return !isRemoteClient();
}

function urlOrigin(value) {
  if (!value) return "";
  try {
    const parsed = new URL(value);
    if (parsed.protocol !== "http:" && parsed.protocol !== "https:") return "";
    return parsed.origin.toLowerCase();
  } catch {
    return "";
  }
}

// Recognizes whether a browser tab is displaying the configured Magpie Web UI.
export function isMagpieTab(tab, magpie) {
  if (!tab || !magpie?.enabled) return false;
  if (tab.kind === "magpie") return true;
  if (
    tab.kind === "terminal" ||
    tab.kind === "editor" ||
    tab.kind === "browser-settings" ||
    tab.kind === "t3code"
  ) {
    return false;
  }
  const tabOrigin = urlOrigin(tab.url);
  if (!tabOrigin) return false;
  const configured = new Set();
  const activeOrigin = urlOrigin(magpie.url);
  if (activeOrigin) configured.add(activeOrigin);
  if (magpie.mode === "external") {
    const extOrigin = urlOrigin(magpie.external_url);
    if (extOrigin) configured.add(extOrigin);
  }
  if (magpie.mode !== "external") {
    const port = Number(magpie.port) || DEFAULT_WEB_PORT;
    configured.add(`http://127.0.0.1:${port}`);
    configured.add(`http://localhost:${port}`);
  }
  return configured.has(tabOrigin);
}

// Ultra-slim 36px header giving ~97% viewport to Magpie Web UI inside Cage.
export function MagpieHeader({
  tab,
  magpie,
  ready,
  busy,
  compact = false,
  headerRef,
  sidebarButton,
  exitButton,
  onReload,
  onOpenDirect,
  onOpenTUI,
  onRestart,
  onToggleBrowserBar,
  onOfficialSettings,
  onSettings,
  onClose,
  menu,
}) {
  const [restarting, setRestarting] = useState(false);
  const running = Boolean(magpie?.running || magpie?.mode === "external");
  const isManaged = magpie?.mode !== "external" && isMagpieManagedSupported(magpie);
  const directAvailable = isMagpieDirectAvailable(magpie);
  const gatewayPort = Number(magpie?.gateway_port) || DEFAULT_GATEWAY_PORT;
  const gatewayRunning = Boolean(magpie?.gateway_running);
  const gatewayUrl = magpie?.gateway_url || `http://127.0.0.1:${gatewayPort}`;

  const handleRestart = async () => {
    if (restarting || busy || !onRestart) return;
    setRestarting(true);
    try {
      await onRestart();
    } finally {
      setRestarting(false);
    }
  };

  return (
    <header
      ref={headerRef}
      className={`chrome t3code-chrome magpie-chrome ${compact ? "split-chrome" : ""}`}
      data-browser-tab={tab.id}
    >
      <div className="t3code-header-row">
        {sidebarButton}
        <div className="t3code-header-identity">
          <AgentIcon agentId="magpie" size={18} />
          <strong className="t3code-header-title">Magpie</strong>
          <span
            className={`t3code-status-chip ${running ? "t3code-status-chip--on" : ""}`}
            title={magpie?.url || "Magpie"}
          >
            <i className="t3code-status-dot" aria-hidden="true" />
            {magpie?.mode === "external"
              ? "外部实例"
              : magpie?.running
                ? (magpie.version ? `v${magpie.version.replace(/^v/i, "")}` : "运行中")
                : "待启动"}
          </span>
          {isManaged && (
            <span
              className={`t3code-status-chip magpie-gateway-chip ${gatewayRunning ? "t3code-status-chip--on magpie-gateway-chip--on" : ""}`}
              title={`Magpie 本地网关 ${gatewayUrl}（${gatewayRunning ? "运行中" : "未运行"}）`}
            >
              <i className="t3code-status-dot" aria-hidden="true" />
              网关 :{gatewayPort}
            </span>
          )}
        </div>

        <div className="t3code-header-actions">
          <button
            type="button"
            className="icon-button"
            aria-label="重新加载 Magpie"
            title="重新加载页面"
            disabled={!ready || busy}
            onClick={onReload}
          >
            <RefreshCw size={14} aria-hidden="true" />
          </button>
          {directAvailable && (
            <button
              type="button"
              className="t3code-header-pill-btn"
              aria-label="原生极速窗口"
              title="在新窗口中极速直连打开（0 VNC 延迟）"
              disabled={busy}
              onClick={onOpenDirect}
            >
              <Zap size={13} aria-hidden="true" />
              <span>原生极速窗口</span>
            </button>
          )}
          {isManaged && onOpenTUI && (
            <button
              type="button"
              className="t3code-header-pill-btn"
              aria-label="打开 Magpie TUI"
              title="在终端中打开 Magpie TUI"
              disabled={!ready || busy}
              onClick={onOpenTUI}
            >
              <TerminalSquare size={13} aria-hidden="true" />
              <span>TUI</span>
            </button>
          )}
          {isManaged && (
            <button
              type="button"
              className="icon-button"
              aria-label="重启 Magpie"
              title="重启 Magpie Web 与网关服务"
              disabled={!ready || busy || restarting}
              onClick={handleRestart}
            >
              <RotateCw size={14} aria-hidden="true" className={restarting ? "spin" : undefined} />
            </button>
          )}
          <button
            type="button"
            className="t3code-header-pill-btn t3code-header-pill-btn--quiet"
            aria-label="显示完整浏览器栏"
            data-magpie-chrome-toggle="true"
            title="切换为完整浏览器标签栏与地址栏"
            onClick={onToggleBrowserBar}
          >
            <PanelTop size={13} aria-hidden="true" />
            <span>显示浏览器栏</span>
          </button>
          {onOfficialSettings && (
            <button
              type="button"
              className="icon-button"
              aria-label="Magpie 官方设置"
              title="打开 Magpie 自己的设置页"
              disabled={!ready || busy}
              onClick={onOfficialSettings}
            >
              <SlidersHorizontal size={14} aria-hidden="true" />
            </button>
          )}
          <button
            type="button"
            className="icon-button"
            aria-label="Magpie 接入设置"
            title="在 Super Cage 中配置 Magpie 接入、模型路由与网关"
            onClick={onSettings}
          >
            <Settings size={14} aria-hidden="true" />
          </button>
          {exitButton}
          <button
            type="button"
            className="icon-button"
            aria-label="关闭 Magpie 标签"
            title="关闭标签"
            disabled={busy}
            onClick={onClose}
          >
            <X size={15} aria-hidden="true" />
          </button>
        </div>
      </div>
      {menu}
      {busy && <div className="progress-line" />}
    </header>
  );
}

export function MagpieSettingsCard({
  store,
  status,
  onConfirm,
  onOpenMagpie,
  onOpenMagpieTUI,
}) {
  const supportsManaged = isMagpieManagedSupported(store, status);
  const directAvailable = isMagpieDirectAvailable(store);
  const [draftMode, setDraftMode] = useState(() =>
    supportsManaged ? (store?.mode || "managed") : "external",
  );
  const [pendingEnable, setPendingEnable] = useState(false);
  const [externalUrl, setExternalUrl] = useState(store?.external_url || "");
  const [portInput, setPortInput] = useState(String(store?.port || DEFAULT_WEB_PORT));
  const [submitting, setSubmitting] = useState("");
  const [actionError, setActionError] = useState("");
  const [savedNotice, setSavedNotice] = useState("");
  const externalUrlRef = useRef(null);

  const vmRunning = Boolean(status?.vm_running);
  const envReady = Boolean(status?.ready);
  const jobRunning = Boolean(status?.job?.running);
  const isInstallJob = jobRunning && status?.job?.kind === "magpie-install";
  const isUninstallJob = jobRunning && status?.job?.kind === "magpie-uninstall";
  const busy = jobRunning || Boolean(submitting);

  const gatewayPort = Number(store?.gateway_port) || DEFAULT_GATEWAY_PORT;
  const gatewayUrl = store?.gateway_url || `http://127.0.0.1:${gatewayPort}`;

  useEffect(() => {
    if (!supportsManaged) {
      setDraftMode("external");
    } else if (store?.mode) {
      setDraftMode(store.mode);
    }
  }, [store?.mode, supportsManaged]);

  useEffect(() => {
    if (store?.enabled) setPendingEnable(false);
  }, [store?.enabled]);

  useEffect(() => {
    setExternalUrl(store?.external_url || "");
  }, [store?.external_url]);

  useEffect(() => {
    if (store?.port) setPortInput(String(store.port));
  }, [store?.port]);

  if (!store) return null;

  const runAction = async (tag, fn, notice = "") => {
    setActionError("");
    setSavedNotice("");
    setSubmitting(tag);
    try {
      const res = await fn();
      if (notice) setSavedNotice(notice);
      return res;
    } catch (e) {
      setActionError(e.message);
      return null;
    } finally {
      setSubmitting("");
    }
  };

  const toggleEnabled = (checked) => {
    setActionError("");
    setSavedNotice("");
    if (!checked) {
      setPendingEnable(false);
      if (store.enabled) {
        void runAction("toggle", () => store.saveConfig({ enabled: false }));
      }
      return;
    }
    const targetMode = supportsManaged ? draftMode : "external";
    if (targetMode === "external") {
      const cleaned = externalUrl.trim();
      if (!cleaned) {
        setDraftMode("external");
        setPendingEnable(true);
        requestAnimationFrame(() => externalUrlRef.current?.focus());
        return;
      }
      void runAction("toggle", async () => {
        const res = await store.saveConfig({
          enabled: true,
          mode: "external",
          external_url: cleaned,
        });
        setPendingEnable(false);
        return res;
      }, "外部地址已保存并接入");
      return;
    }
    void runAction("toggle", () => store.saveConfig({ enabled: true, mode: "managed" }));
  };

  const selectMode = (nextMode) => {
    if (nextMode === "managed" && !supportsManaged) return;
    setActionError("");
    setSavedNotice("");
    setDraftMode(nextMode);
    if (nextMode === "external") {
      requestAnimationFrame(() => externalUrlRef.current?.focus());
      return;
    }
    if (store.mode !== "managed" || pendingEnable) {
      const patch = pendingEnable ? { enabled: true, mode: "managed" } : { mode: "managed" };
      void runAction("mode", async () => {
        const res = await store.saveConfig(patch);
        if (pendingEnable) setPendingEnable(false);
        return res;
      }, "已切换为客机内置托管模式").then((res) => {
        if (!res && store?.mode) setDraftMode(store.mode);
      });
    }
  };

  const saveOpenMode = (open_mode) => {
    if (!directAvailable && open_mode === "direct") return;
    void runAction("open-mode", () => store.saveConfig({ open_mode }), "默认打开方式已保存");
  };

  const saveFocusChrome = (focus_chrome) =>
    void runAction("focus-chrome", () => store.saveConfig({ focus_chrome }));

  const saveGatewayOnly = (gateway_only) => {
    const wasRunning = store.running && store.mode === "managed";
    void runAction(
      "gateway-only",
      () => store.saveConfig({ gateway_only }),
      wasRunning ? "仅网关模式已保存，原运行中的 Magpie 已停止" : "仅网关模式设置已保存",
    );
  };

  const saveExternalUrl = (e) => {
    e.preventDefault();
    const cleaned = externalUrl.trim();
    if (!cleaned) {
      setActionError("请输入有效的外部 Magpie 网页地址（例如 http://127.0.0.1:3430）。");
      externalUrlRef.current?.focus();
      return;
    }
    void runAction("ext-url", async () => {
      const res = await store.saveConfig({
        mode: "external",
        external_url: cleaned,
        enabled: true,
      });
      setPendingEnable(false);
      return res;
    }, "外部地址已保存并接入");
  };

  const savePort = (e) => {
    e.preventDefault();
    const port = Number(portInput);
    if (!Number.isInteger(port) || port < 1024 || port > 65535) {
      setActionError("Web 端口须为 1024 - 65535 之间的整数。");
      return;
    }
    if (port === gatewayPort) {
      setActionError(`Web 端口不能与本地网关端口（${gatewayPort}）相同，请选择其他端口。`);
      return;
    }
    const wasRunning = store.running && store.mode === "managed";
    void runAction(
      "port",
      () => store.saveConfig({ port }),
      wasRunning ? "Web 服务端口已保存，原运行中的 Magpie 已停止" : "Web 服务端口已保存",
    );
  };

  const handleUninstall = async () => {
    const confirmed = onConfirm
      ? await onConfirm({
          title: "卸载 Magpie？",
          body: "将停止 Magpie 服务并卸载客机中的 Magpie 程序，自动恢复各 Agent CLI 的原始配置，同时安全保留您的个人数据与配置目录。",
          confirm: "确认卸载",
        })
      : true;
    if (!confirmed) return;
    void runAction("uninstall", () => store.uninstall());
  };

  const expanded = Boolean(store.enabled || pendingEnable);
  const activeDraftMode = supportsManaged ? draftMode : "external";
  const effectiveOpenMode = directAvailable ? store.open_mode : "workbench";
  const canLaunch = store.enabled && (
    store.mode === "external"
      ? Boolean(store.external_url)
      : (supportsManaged && vmRunning && store.installed)
  );
  const externalUnchanged =
    store.enabled &&
    store.mode === "external" &&
    externalUrl.trim() === (store.external_url || "");

  return (
    <section className="t3code-card magpie-card" aria-label="Magpie 模型路由与本地网关">
      <div className="t3code-card-top">
        <AgentIcon agentId="magpie" size={36} />
        <div className="t3code-card-info">
          <div className="t3code-card-title">
            <strong>Magpie 模型路由与本地网关</strong>
            <span className="agent-pill">可选增强</span>
            {store.enabled ? (
              store.mode === "external" ? (
                <span className="agent-pill agent-pill--ok">
                  {store.external_url ? "外部实例" : "待配置地址"}
                </span>
              ) : !supportsManaged ? (
                <span className="agent-pill">需配置外部实例</span>
              ) : store.running ? (
                <span className="agent-pill agent-pill--ok">运行中</span>
              ) : store.installed ? (
                <span className="agent-pill">已安装 · 按需启动</span>
              ) : (
                <span className="agent-pill">尚未安装</span>
              )
            ) : pendingEnable ? (
              <span className="agent-pill">待配置地址</span>
            ) : (
              <span className="agent-pill">未启用</span>
            )}
            {supportsManaged && store.mode === "managed" && store.version && (
              <span className="agent-pill">v{store.version.replace(/^v/i, "")}</span>
            )}
            {supportsManaged && store.enabled && store.mode === "managed" && store.installed && (
              <span
                className={`agent-pill ${store.gateway_running ? "agent-pill--ok" : ""}`}
                title={`本地网关 ${gatewayUrl}`}
              >
                网关 :{gatewayPort}
              </span>
            )}
            {store.enabled && (
              <span
                className="agent-pill agent-pill--ok"
                title="已自动关闭 Super Cage 内置多账号调度并交由 Magpie 统一接管"
              >
                已接管账号调度
              </span>
            )}
          </div>
          <p className="agent-card-desc">
            可选安装与托管 Magpie 统一模型路由与本地网关：一键切换任意 Agent CLI 的模型与供应商，自动故障转移与配额统计；提供原版 Magpie 网页界面（工作台分屏或 0 VNC 延迟原生直连窗口）与 Magpie TUI。
          </p>
        </div>
        <label className="t3code-optin-switch">
          <input
            type="checkbox"
            role="switch"
            aria-label="启用 Magpie 集成"
            checked={expanded}
            disabled={busy}
            onChange={(e) => toggleEnabled(e.target.checked)}
          />
        </label>
      </div>

      {(isInstallJob || isUninstallJob) && (
        <div className="agent-job-banner" role="status">
          <LoaderCircle size={16} className="spin" />
          <span>
            {status.job.message ||
              (isUninstallJob ? "正在卸载 Magpie 并恢复 Agent 原配置…" : "正在客机环境中安装 Magpie…")}
          </span>
        </div>
      )}

      {(actionError || store.error || store.serverError) && (
        <div className="error-message" role="alert">
          {actionError || store.error || store.serverError}
        </div>
      )}

      {expanded && (
        <div className="t3code-card-body">
          <div className="t3code-quick-actions">
            {canLaunch && (
              <button
                type="button"
                className="primary"
                disabled={busy || (effectiveOpenMode === "workbench" && !envReady)}
                onClick={() => void runAction("open", () => onOpenMagpie?.())}
              >
                {submitting === "open" ? <LoaderCircle size={14} className="spin" /> : <ExternalLink size={14} />}
                立即打开 Magpie
              </button>
            )}
            {canLaunch && directAvailable && (
              <button
                type="button"
                disabled={busy}
                onClick={() => void runAction("open-direct", () => onOpenMagpie?.("direct"))}
              >
                {submitting === "open-direct" ? <LoaderCircle size={14} className="spin" /> : <Zap size={14} />}
                原生极速窗口
              </button>
            )}
            {canLaunch && (
              <button
                type="button"
                title="打开 Magpie 自己的设置页"
                disabled={busy || (effectiveOpenMode === "workbench" && !envReady)}
                onClick={() => void runAction("open-settings", () => onOpenMagpie?.("", "settings"))}
              >
                {submitting === "open-settings" ? <LoaderCircle size={14} className="spin" /> : <SlidersHorizontal size={14} />}
                Magpie 官方设置
              </button>
            )}
            {supportsManaged && store.enabled && store.mode === "managed" && store.installed && onOpenMagpieTUI && (
              <button
                type="button"
                disabled={!vmRunning || busy}
                onClick={() => void runAction("open-tui", () => onOpenMagpieTUI?.())}
              >
                {submitting === "open-tui" ? <LoaderCircle size={14} className="spin" /> : <TerminalSquare size={14} />}
                打开 Magpie TUI
              </button>
            )}
            {supportsManaged && store.enabled && store.mode === "managed" && store.installed && (
              <>
                {store.running ? (
                  <>
                    <button
                      type="button"
                      disabled={!vmRunning || busy}
                      onClick={() => void runAction("restart", () => store.restart(), "Magpie 已重启")}
                    >
                      {submitting === "restart" ? <LoaderCircle size={14} className="spin" /> : <RotateCw size={14} />}
                      重启
                    </button>
                    <button
                      type="button"
                      disabled={!vmRunning || busy}
                      onClick={() => void runAction("stop", () => store.stop(), "Magpie 已停止")}
                    >
                      <Square size={13} />
                      停止
                    </button>
                  </>
                ) : (
                  <button
                    type="button"
                    disabled={!vmRunning || busy}
                    onClick={() => void runAction("start", () => store.start(), "Magpie 已启动")}
                  >
                    {submitting === "start" ? <LoaderCircle size={14} className="spin" /> : <Play size={13} />}
                    启动服务
                  </button>
                )}
              </>
            )}
            <button
              type="button"
              disabled={store.loading || busy}
              onClick={() => void store.refresh(true)}
              title="重新检测 Magpie 与本地网关状态"
            >
              <RefreshCw size={14} className={store.loading ? "spin" : undefined} />
              刷新状态
            </button>
            {savedNotice && (
              <span className="agent-pill agent-pill--ok">
                <Check size={12} />
                {savedNotice}
              </span>
            )}
          </div>

          {store.enabled && (
            <div className="inline-info t3code-info-banner" role="status">
              <span>
                已启用 Magpie：Super Cage 内置多账号调度已自动关闭并交由 Magpie 统一接管，所有 Claude、Codex 及其他 Agent CLI（含终端、Shell、Herdr、T3 Code 与 Orca Server）均以原生配置运行以确保 Magpie 接管生效。您在 Cage 中保存的账号已保留，关闭或卸载 Magpie 后将自动恢复调度。
              </span>
            </div>
          )}

          {!supportsManaged && (
            <div className="inline-info t3code-info-banner" role="status">
              <span>当前客机环境暂不支持 Cage 内置托管 Magpie（例如 Windows 原生客机），请填写已有 Magpie 网页地址以接入使用。</span>
            </div>
          )}

          {!directAvailable && (
            <div className="inline-info t3code-info-banner" role="status">
              <span>当前通过远程客户端或桌面内嵌视图访问 Cage，本机回环直连窗口不可用，将自动使用 Cage 工作台内置沉浸视图打开 Magpie。</span>
            </div>
          )}

          <div className="t3code-config-grid">
            <div className="t3code-field">
              <span className="t3code-field-label" id="magpie-mode-label">运行模式</span>
              <div
                className="t3code-mode-switch"
                role="group"
                aria-labelledby="magpie-mode-label"
              >
                <button
                  type="button"
                  aria-pressed={activeDraftMode === "managed"}
                  className={`t3code-mode-btn ${activeDraftMode === "managed" ? "selected" : ""}`}
                  disabled={busy || !supportsManaged}
                  title={supportsManaged ? "在 Cage 客机内自动安装托管 Magpie Web 与本地网关" : "当前客机暂不支持内置托管模式"}
                  onClick={() => selectMode("managed")}
                >
                  客机内置托管（推荐）
                </button>
                <button
                  type="button"
                  aria-pressed={activeDraftMode === "external"}
                  className={`t3code-mode-btn ${activeDraftMode === "external" ? "selected" : ""}`}
                  disabled={busy}
                  onClick={() => selectMode("external")}
                >
                  接入已有实例
                </button>
              </div>
            </div>

            <label>
              默认打开方式
              <InlineSelect
                aria-label="Magpie 默认打开方式"
                value={effectiveOpenMode}
                disabled={busy || !directAvailable}
                onValueChange={saveOpenMode}
              >
                <option value="workbench">工作台沉浸网页（可分屏）</option>
                {directAvailable && (
                  <option value="direct">原生极速直连窗口（0 VNC 延迟）</option>
                )}
              </InlineSelect>
            </label>
          </div>

          {supportsManaged && activeDraftMode === "managed" ? (
            <>
              <div className="magpie-install-row">
                <div className="magpie-install-actions">
                  <button
                    type="button"
                    className={store.installed ? "" : "primary"}
                    disabled={!envReady || busy}
                    onClick={() => void runAction("install", () => store.install())}
                  >
                    {submitting === "install" ? <LoaderCircle size={14} className="spin" /> : <Download size={14} />}
                    {store.installed ? "更新 / 重装 Magpie" : "一键安装 Magpie"}
                  </button>
                  {store.installed && (
                    <button
                      type="button"
                      className="magpie-uninstall-btn"
                      disabled={!envReady || busy}
                      onClick={handleUninstall}
                    >
                      {submitting === "uninstall" ? <LoaderCircle size={14} className="spin" /> : <Trash2 size={14} />}
                      卸载 Magpie
                    </button>
                  )}
                </div>
                <p className="magpie-install-note">
                  通过官方安装脚本部署，Linux 客机自动使用轻量 <code>--tui</code> 构建（包含完整原版 Web UI 与本地统一网关）。
                </p>
              </div>

              <div className="magpie-port-gateway-grid">
                <form className="t3code-port-form" onSubmit={savePort}>
                  <label>
                    Magpie Web 端口
                    <input
                      type="number"
                      min={1024}
                      max={65535}
                      value={portInput}
                      disabled={busy}
                      onChange={(e) => setPortInput(e.target.value)}
                    />
                  </label>
                  <button
                    type="submit"
                    disabled={busy || Number(portInput) === Number(store.port || DEFAULT_WEB_PORT)}
                  >
                    保存端口
                  </button>
                </form>

                <div className="t3code-field">
                  <span className="t3code-field-label">本地网关地址</span>
                  <div className="magpie-gateway-readonly" title={`Magpie 本地网关监听地址 ${gatewayUrl}`}>
                    <code className="magpie-gateway-value">{gatewayUrl}</code>
                    <span className={`agent-pill ${store.gateway_running ? "agent-pill--ok" : ""}`}>
                      {store.gateway_running ? "运行中" : "待启动"}
                    </span>
                  </div>
                </div>
              </div>
            </>
          ) : (
            <form className="t3code-external-form-wrap" onSubmit={saveExternalUrl}>
              <div className="t3code-external-form">
                <label>
                  外部 Magpie 网页地址
                  <input
                    ref={externalUrlRef}
                    type="url"
                    required
                    value={externalUrl}
                    disabled={busy}
                    onChange={(e) => setExternalUrl(e.target.value)}
                    placeholder="例如 http://127.0.0.1:3430 或 https://magpie.example.com"
                    spellCheck="false"
                  />
                </label>
                <button
                  type="submit"
                  className="primary"
                  disabled={busy || externalUnchanged}
                >
                  {submitting === "ext-url" && <LoaderCircle size={14} className="spin" />}
                  保存地址并接入
                </button>
              </div>
              {(store.mode !== "external" || !store.enabled) && (
                <p className="t3code-mode-hint">
                  填写已有 Magpie 实例的 HTTP(S) 网页地址后，点击「保存地址并接入」即可一次性保存并启用外部实例模式。
                </p>
              )}
            </form>
          )}

          <div className="magpie-toggle-group">
            <label className="toggle-row t3code-toggle-row">
              <span>
                <strong>工作台极简沉浸顶栏</strong>
                <small>在 Cage 工作台中打开 Magpie 时自动收起传统地址栏，释放约 97% 画面区域；随时可一键展开浏览器栏。</small>
              </span>
              <input
                type="checkbox"
                role="switch"
                checked={store.focus_chrome !== false}
                disabled={busy}
                onChange={(e) => saveFocusChrome(e.target.checked)}
              />
            </label>

            {supportsManaged && activeDraftMode === "managed" && (
              <label className="toggle-row t3code-toggle-row magpie-toggle-row--sub">
                <span>
                  <strong>仅网关模式</strong>
                  <small>开启后向 <code>magpie web</code> 传递 <code>--gateway</code>，仅展示模型路由与网关控制台，隐藏 Agents / Sessions / Library 标签。</small>
                </span>
                <input
                  type="checkbox"
                  role="switch"
                  checked={Boolean(store.gateway_only)}
                  disabled={busy}
                  onChange={(e) => saveGatewayOnly(e.target.checked)}
                />
              </label>
            )}
          </div>
        </div>
      )}
    </section>
  );
}
