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
  X,
  Zap,
} from "lucide-react";
import { useServiceAccounts } from "./use-service-accounts";
import { isRemoteClient } from "./client-context.mjs";
import { AgentIcon } from "./agent-icon";
import { InlineSelect } from "./inline-select";
import "./t3code.css";

export function isT3CodeManagedSupported(t3code, status = null) {
  if (t3code?.supports_managed === false || t3code?.data?.supports_managed === false) {
    return false;
  }
  if (
    (status?.guest_system === "windows-11" || status?.guest_system === "windows-remote") &&
    t3code?.supports_managed !== true &&
    t3code?.data?.supports_managed !== true
  ) {
    return false;
  }
  return true;
}

function isDesktopWebView() {
  if (typeof window === "undefined") return false;
  return Boolean(window.webkit?.messageHandlers?.cage || window.chrome?.webview);
}

export function isT3CodeDirectAvailable(t3code) {
  if (t3code?.direct_available === false || t3code?.data?.direct_available === false) {
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

// Recognizes whether a browser tab is displaying the configured T3 Code UI.
export function isT3CodeTab(tab, t3code) {
  if (!tab) return false;
  if (tab.kind === "t3code") return true;
  if (tab.kind === "terminal" || tab.kind === "editor" || tab.kind === "browser-settings") return false;
  if (!t3code?.enabled) return false;
  const tabOrigin = urlOrigin(tab.url);
  if (!tabOrigin) return false;
  const configured = new Set();
  const activeOrigin = urlOrigin(t3code.url);
  if (activeOrigin) configured.add(activeOrigin);
  if (t3code.mode === "external") {
    const extOrigin = urlOrigin(t3code.external_url);
    if (extOrigin) configured.add(extOrigin);
  }
  if (t3code.mode === "managed" && Number(t3code.port) > 0) {
    configured.add(`http://127.0.0.1:${t3code.port}`);
    configured.add(`http://localhost:${t3code.port}`);
  }
  return configured.has(tabOrigin);
}

// Ultra-slim 36px header giving ~97% viewport to T3 Code Web UI inside Cage.
export function T3CodeHeader({
  tab,
  t3code,
  ready,
  busy,
  compact = false,
  headerRef,
  sidebarButton,
  exitButton,
  onReload,
  onOpenDirect,
  onRestart,
  onToggleBrowserBar,
  onOfficialSettings,
  onSettings,
  onClose,
  menu,
}) {
  const [restarting, setRestarting] = useState(false);
  const running = Boolean(t3code?.running || t3code?.mode === "external");
  const isManaged = t3code?.mode !== "external" && isT3CodeManagedSupported(t3code);
  const directAvailable = isT3CodeDirectAvailable(t3code);

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
      className={`chrome t3code-chrome ${compact ? "split-chrome" : ""}`}
      data-browser-tab={tab.id}
    >
      <div className="t3code-header-row">
        {sidebarButton}
        <div className="t3code-header-identity">
          <AgentIcon agentId="t3code" size={18} />
          <strong className="t3code-header-title">T3 Code</strong>
          <span
            className={`t3code-status-chip ${running ? "t3code-status-chip--on" : ""}`}
            title={t3code?.url || "T3 Code"}
          >
            <i className="t3code-status-dot" aria-hidden="true" />
            {t3code?.mode === "external"
              ? "外部实例"
              : t3code?.running
                ? (t3code.version ? `v${t3code.version.replace(/^v/i, "")}` : "运行中")
                : "待启动"}
          </span>
        </div>

        <div className="t3code-header-actions">
          <button
            type="button"
            className="icon-button"
            aria-label="重新加载 T3 Code"
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
          {isManaged && (
            <button
              type="button"
              className="icon-button"
              aria-label="重启 T3 Code"
              title="重启 T3 Code 服务（应用最新账号绑定）"
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
            data-t3code-chrome-toggle="true"
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
              aria-label="T3 Code 官方设置"
              title="打开 T3 Code 自己的设置页"
              disabled={!ready || busy}
              onClick={onOfficialSettings}
            >
              <SlidersHorizontal size={14} aria-hidden="true" />
            </button>
          )}
          <button
            type="button"
            className="icon-button"
            aria-label="T3 Code 接入设置"
            title="在 Super Cage 中配置 T3 Code 接入与账号绑定"
            onClick={onSettings}
          >
            <Settings size={14} aria-hidden="true" />
          </button>
          {exitButton}
          <button
            type="button"
            className="icon-button"
            aria-label="关闭 T3 Code 标签"
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

export function T3CodeSettingsCard({
  store,
  agentsStore,
  status,
  onOpenT3Code,
  onOpenAgentSettings,
  onConfirm,
  onDirty,
}) {
  const supportsManaged = isT3CodeManagedSupported(store, status);
  const directAvailable = isT3CodeDirectAvailable(store);
  const hasConfig = Boolean(store?.data);
  const restorePending = Boolean(store?.data?.restore_pending && !store.running);
  const savedMode = supportsManaged ? (store?.mode || "managed") : "external";
  const savedUrl = store?.external_url || "";
  const savedPort = String(store?.port || 13773);
  const [installChannel, setInstallChannel] = useState("npm");
  const [draftMode, setDraftMode] = useState(savedMode);
  const [pendingEnable, setPendingEnable] = useState(false);
  const [externalUrl, setExternalUrl] = useState(savedUrl);
  const [portInput, setPortInput] = useState(savedPort);
  const [submitting, setSubmitting] = useState("");
  const [actionError, setActionError] = useState("");
  const [savedNotice, setSavedNotice] = useState("");
  const [bindingRestartHint, setBindingRestartHint] = useState(false);
  const externalUrlRef = useRef(null);

  const vmRunning = Boolean(status?.vm_running);
  const envReady = Boolean(status?.ready);
  const jobRunning = Boolean(status?.job?.running);
  const isInstallJob = jobRunning && status?.job?.kind === "t3code-install";
  const jobError = status?.job?.kind === "t3code-install" ? status.job.error : "";
  const busy = jobRunning || Boolean(submitting);

  useEffect(() => { setDraftMode(savedMode); }, [savedMode]);

  useEffect(() => {
    if (store?.enabled) setPendingEnable(false);
  }, [store?.enabled]);

  useEffect(() => {
    setExternalUrl(savedUrl);
  }, [savedUrl]);

  useEffect(() => {
    setPortInput(savedPort);
  }, [savedPort]);

  const dirty = pendingEnable || draftMode !== savedMode || externalUrl !== savedUrl || portInput !== savedPort;
  useEffect(() => { onDirty?.(dirty); }, [dirty, onDirty]);
  useEffect(() => () => onDirty?.(false), [onDirty]);

  const { bindableAgents, accounts: accountOptions, errors: accountErrors, loading: accountsLoading, retry: retryAccounts } = useServiceAccounts(agentsStore, store?.account_bindings, {
    available: hasConfig && supportsManaged && draftMode === "managed",
    service: "t3code",
  });

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

  const confirmInterruption = () => store.mode !== "managed" || !store.running || onConfirm?.({
    title: "保存设置并停止托管 T3 Code？",
    body: "保存后会停止当前托管实例。请先保存会话中的工作。",
    confirm: "保存设置",
  });

  const toggleEnabled = async (checked) => {
    setActionError("");
    setSavedNotice("");
    if (!checked) {
      setPendingEnable(false);
      if (store.enabled) {
        if (!(await confirmInterruption())) return;
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
        setExternalUrl(res.external_url || "");
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
    void runAction("open-mode", () => store.saveConfig({ open_mode }), "默认打开方式已保存");
  };

  const saveFocusChrome = (focus_chrome) =>
    void runAction("focus-chrome", () => store.saveConfig({ focus_chrome }));

  const saveExternalUrl = async (e) => {
    e.preventDefault();
    const cleaned = externalUrl.trim();
    const enabled = store.enabled || pendingEnable;
    if (!cleaned && enabled) {
      setActionError("请输入有效的外部 T3 Code URL（例如 http://127.0.0.1:3773）。");
      externalUrlRef.current?.focus();
      return;
    }
    if (store.mode !== "external" && !(await confirmInterruption())) return;
    void runAction("ext-url", async () => {
      const res = await store.saveConfig({
        mode: "external",
        external_url: cleaned,
        ...(pendingEnable ? { enabled: true } : {}),
      });
      setExternalUrl(res.external_url || "");
      setPendingEnable(false);
      return res;
    }, enabled ? "外部地址已保存并接入" : "外部地址已保存");
  };

  const savePort = async (e) => {
    e.preventDefault();
    const port = Number(portInput);
    if (!Number.isInteger(port) || port < 1024 || port > 65535) {
      setActionError("端口须为 1024 - 65535 之间的整数。");
      return;
    }
    if (port === Number(savedPort) || !(await confirmInterruption())) return;
    void runAction("port", async () => {
      const result = await store.saveConfig({ port });
      setPortInput(String(result.port));
      return result;
    }, "服务端口已保存");
  };

  const updateBinding = (agentId, value) => {
    const nextBindings = { ...(store.account_bindings || {}), [agentId]: value };
    void runAction("binding", async () => {
      const res = await store.saveConfig({ account_bindings: nextBindings });
      if (store.running && store.mode === "managed") setBindingRestartHint(true);
      return res;
    }, "账号绑定已保存");
  };

  const expanded = Boolean(store.enabled || pendingEnable);
  const activeDraftMode = supportsManaged ? draftMode : "external";
  const effectiveOpenMode = directAvailable ? store.open_mode : "workbench";
  const canLaunch = store.enabled && (
    store.mode === "external"
      ? Boolean(store.external_url)
      : (supportsManaged && vmRunning && store.installed)
  );
  const externalUnchanged = !pendingEnable && store.mode === "external" && externalUrl === savedUrl;

  return (
    <section className="t3code-card" aria-label="T3 Code 可选集成">
      <div className="t3code-card-top">
        <AgentIcon agentId="t3code" size={36} />
        <div className="t3code-card-info">
          <div className="t3code-card-title">
            <strong>T3 Code 工作流集成</strong>
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
          </div>
          <p className="agent-card-desc">
            可选接入 T3 Code 可视化多 Agent 编排界面：自动注入你在 Cage 中托管的 Agent 账号，支持工作台分屏沉浸使用或零 VNC 延迟的原生直连窗口。关闭时零后台资源占用。
          </p>
        </div>
        <label className="t3code-optin-switch">
          <input
            type="checkbox"
            role="switch"
            aria-label="启用 T3 Code 集成"
            checked={expanded}
            disabled={busy || !hasConfig}
            onChange={(e) => void toggleEnabled(e.target.checked)}
          />
        </label>
      </div>

      {isInstallJob && (
        <div className="agent-job-banner" role="status">
          <LoaderCircle size={16} className="spin" />
          <span>{status.job.message || "正在客机环境中安装 T3 Code…"}</span>
        </div>
      )}

      {(actionError || jobError || store.error || store.serverError) && (
        <div className="error-message" role="alert">
          {actionError || jobError || store.error || store.serverError}
        </div>
      )}
      {store.loading && !hasConfig && <p className="field-help" role="status">正在读取 T3 Code 设置…</p>}
      <div className="t3code-quick-actions">
        <button type="button" disabled={store.loading || busy}
          onClick={() => { setActionError(""); void store.refresh(); }}>
          <RefreshCw size={14} className={store.loading ? "spin" : undefined} />
          刷新状态
        </button>
        {savedNotice && <span className="agent-pill agent-pill--ok" role="status"><Check size={12} />{savedNotice}</span>}
      </div>

      {hasConfig && (
        <div className="t3code-card-body">
          <div className="t3code-quick-actions">
            {canLaunch && (
              <button
                type="button"
                className="primary"
                disabled={busy || (effectiveOpenMode === "workbench" && !envReady)}
                onClick={() => void runAction("open", () => onOpenT3Code?.())}
              >
                {submitting === "open" ? <LoaderCircle size={14} className="spin" /> : <ExternalLink size={14} />}
                立即打开 T3 Code
              </button>
            )}
            {canLaunch && directAvailable && (
              <button
                type="button"
                disabled={busy}
                onClick={() => void runAction("open-direct", () => onOpenT3Code?.("direct"))}
              >
                {submitting === "open-direct" ? <LoaderCircle size={14} className="spin" /> : <Zap size={14} />}
                原生极速直连窗口
              </button>
            )}
            {canLaunch && (
              <button
                type="button"
                title="在工作台中打开 T3 Code 自己的设置页"
                disabled={busy || !envReady}
                onClick={() => void runAction("open-settings", () => onOpenT3Code?.("", "settings"))}
              >
                {submitting === "open-settings" ? <LoaderCircle size={14} className="spin" /> : <SlidersHorizontal size={14} />}
                T3 Code 官方设置
              </button>
            )}
            {supportsManaged && store.enabled && store.mode === "managed" && store.installed && (
              <>
                {store.running ? (
                  <>
                    <button
                      type="button"
                      disabled={!vmRunning || busy}
                      onClick={() => void runAction("restart", async () => {
                        const res = await store.restart();
                        setBindingRestartHint(false);
                        return res;
                      }, "T3 Code 已重启")}
                    >
                      {submitting === "restart" ? <LoaderCircle size={14} className="spin" /> : <RotateCw size={14} />}
                      重启
                    </button>
                    <button
                      type="button"
                      disabled={!vmRunning || busy}
                      onClick={() => void runAction("stop", () => store.stop(), "T3 Code 已停止")}
                    >
                      <Square size={13} />
                      停止
                    </button>
                  </>
                ) : (
                  <button
                    type="button"
                    disabled={!vmRunning || busy}
                    onClick={() => void runAction("start", async () => {
                      const res = await store.start();
                      setBindingRestartHint(false);
                      return res;
                    }, "T3 Code 已启动")}
                  >
                    {submitting === "start" ? <LoaderCircle size={14} className="spin" /> : <Play size={13} />}
                      {restorePending ? "用原账号启动" : "启动服务"}
                  </button>
                )}
                {restorePending && (
                  <>
                    <button
                      type="button"
                      disabled={!vmRunning || busy}
                      onClick={() => void runAction("restart", () => store.restart(), "T3 Code 已按最新绑定启动")}
                    >
                      <RotateCw size={14} />
                      用最新账号绑定启动
                    </button>
                    <button
                      type="button"
                      disabled={busy}
                      onClick={() => void runAction("stop", () => store.stop(), "T3 Code 恢复已取消")}
                    >
                      <Square size={13} />
                      取消恢复
                    </button>
                  </>
                )}
              </>
            )}
          </div>

          {!supportsManaged && (
            <div className="inline-info t3code-info-banner" role="status">
              <span>当前客机环境暂不支持 Cage 内置托管 T3 Code（例如 Windows 原生客机），请填写已有 T3 Code 实例地址以接入使用。</span>
            </div>
          )}

          {!directAvailable && (
            <div className="inline-info t3code-info-banner" role="status">
              <span>这个窗口只能在 Cage 工作台内打开 T3 Code。默认打开方式会保留供支持直连的窗口使用。</span>
            </div>
          )}

          <div className="t3code-config-grid">
            <div className="t3code-field">
              <span className="t3code-field-label" id="t3code-mode-label">运行模式</span>
              <div
                className="t3code-mode-switch"
                role="group"
                aria-labelledby="t3code-mode-label"
              >
                <button
                  type="button"
                  aria-pressed={activeDraftMode === "managed"}
                  className={`t3code-mode-btn ${activeDraftMode === "managed" ? "selected" : ""}`}
                  disabled={busy || !supportsManaged}
                  title={supportsManaged ? "在 Cage 客机内自动安装托管并注入账号" : "当前客机暂不支持内置托管模式"}
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
                aria-label="T3 Code 默认打开方式"
                value={store.open_mode}
                disabled={busy}
                onValueChange={saveOpenMode}
              >
                <option value="workbench">工作台沉浸网页（可分屏）</option>
                <option value="direct">原生极速直连窗口（0 VNC 延迟）</option>
              </InlineSelect>
            </label>
          </div>

          {supportsManaged && activeDraftMode === "managed" ? (
            <div className="t3code-managed-row">
              <div className="t3code-install-box">
                <label>
                  官方安装渠道
                  <InlineSelect
                    aria-label="T3 Code 官方安装渠道"
                    value={installChannel}
                    disabled={busy}
                    onValueChange={setInstallChannel}
                  >
                    <option value="npm">npm 官方包</option>
                    <option value="script">官方脚本</option>
                  </InlineSelect>
                </label>
                <button
                  type="button"
                  className={store.installed ? "" : "primary"}
                  disabled={!store.enabled || !envReady || busy}
                  onClick={() => void runAction("install", () => store.install(installChannel))}
                >
                  {submitting === "install" ? <LoaderCircle size={14} className="spin" /> : <Download size={14} />}
                  {store.installed ? "更新 / 重装 T3 Code" : "一键安装 T3 Code"}
                </button>
              </div>

              <form className="t3code-port-form" onSubmit={savePort} noValidate>
                <label>
                  客机服务端口
                  <input
                    type="number"
                    min={1024}
                    max={65535}
                    value={portInput}
                    disabled={busy}
                    onChange={(e) => setPortInput(e.target.value)}
                  />
                </label>
                {portInput !== savedPort && <button type="button" disabled={busy} onClick={() => { setPortInput(savedPort); setActionError(""); }}>放弃修改</button>}
                <button
                  type="submit"
                  disabled={busy || Number(portInput) === Number(store.port || 13773)}
                >
                  保存端口
                </button>
              </form>
            </div>
          ) : (
            <form className="t3code-external-form-wrap" onSubmit={saveExternalUrl}>
              <div className="t3code-external-form">
                <label>
                  外部 T3 Code 地址
                  <input
                    ref={externalUrlRef}
                    type="url"
                    required={expanded}
                    value={externalUrl}
                    disabled={busy}
                    onChange={(e) => setExternalUrl(e.target.value)}
                    placeholder="例如 http://127.0.0.1:3773 或 https://t3.example.com"
                    spellCheck="false"
                  />
                </label>
                {(pendingEnable || externalUrl !== savedUrl || draftMode !== savedMode) && (
                  <button type="button" disabled={busy} onClick={() => {
                    setPendingEnable(false);
                    setExternalUrl(savedUrl);
                    setDraftMode(savedMode);
                    setActionError("");
                  }}>放弃修改</button>
                )}
                <button
                  type="submit"
                  className="primary"
                  disabled={busy || externalUnchanged}
                >
                  {submitting === "ext-url" && <LoaderCircle size={14} className="spin" />}
                  {expanded ? "保存地址并接入" : "保存外部地址"}
                </button>
              </div>
              {(store.mode !== "external" || !store.enabled) && (
                <p className="t3code-mode-hint">
                  填写已有 T3 Code 实例的地址。非回环地址须使用 HTTPS。
                </p>
              )}
            </form>
          )}

          <label className="toggle-row t3code-toggle-row">
            <span>
              <strong>工作台极简沉浸顶栏</strong>
              <small>在 Cage 工作台中打开 T3 Code 时自动收起传统地址栏，释放约 97% 画面区域；随时可一键展开浏览器栏。</small>
            </span>
            <input
              type="checkbox"
              role="switch"
              checked={store.focus_chrome !== false}
              disabled={busy}
              onChange={(e) => saveFocusChrome(e.target.checked)}
            />
          </label>

          {supportsManaged && activeDraftMode === "managed" && bindableAgents.length > 0 && (
            <div className="t3code-bindings">
              <div className="t3code-bindings-head">
                <strong>Agent 账号绑定（自动注入到托管 T3 Code）</strong>
                <small>
                  为已安装的 Agent 指定 T3 Code 使用的 Cage 隔离账号，或交由智能调度器自动选择最优账号。 仅已确认的原生启动配置会跳过自动调度；固定账号不受影响。
                </small>
                {bindableAgents.some(entry => !entry.runtime?.installed) && (
                  <small>可以预先保存绑定，安装对应 CLI 后使用。 {onOpenAgentSettings && <button type="button" className="text-button" onClick={onOpenAgentSettings}>前往 Agent CLI</button>}</small>
                )}
                {Object.keys(accountErrors).length > 0 && <button type="button" disabled={accountsLoading} onClick={retryAccounts}>重试账号列表</button>}
              </div>
              {bindingRestartHint && (
                <div className="inline-info t3code-restart-hint">
                  <span>账号绑定已更新。重启 T3 Code 服务即可让运行中的实例立即切换至新账号。</span>
                  <button
                    type="button"
                    className="text-button"
                    disabled={!vmRunning || busy}
                    onClick={() => void runAction("restart", async () => {
                      const res = await store.restart();
                      setBindingRestartHint(false);
                      return res;
                    }, "T3 Code 已重启并应用最新绑定")}
                  >
                    立即重启应用
                  </button>
                </div>
              )}
              <div className="t3code-bindings-list">
                {bindableAgents.map((entry) => {
                  const id = entry.spec.id;
                  const currentValue = Object.hasOwn(store.account_bindings, id) ? store.account_bindings[id] : "auto";
                  const accounts = accountOptions[id] || [];
                  const activeBound = store.active_bindings?.[id];
                  return (
                    <div key={id} className="t3code-binding-row">
                      <div className="t3code-binding-agent">
                        <AgentIcon
                          agentId={id}
                          glyph={entry.spec.glyph}
                          accentColor={entry.spec.accent_color}
                          size={20}
                        />
                        <span>{entry.spec.name}</span>
                        {activeBound && (
                          <span className="agent-pill" title={`当前生效绑定：${activeBound}`}>
                            生效：{activeBound}
                          </span>
                        )}
                      </div>
                      <InlineSelect
                        aria-label={`${entry.spec.name} 的 T3 Code 账号绑定`}
                        value={currentValue}
                        disabled={busy}
                        onValueChange={(val) => updateBinding(id, val)}
                      >
                        <option value="auto">跟随 Cage 智能调度（自动）</option>
                        <option value="default">客机默认登录</option>
                        {currentValue !== "auto" && currentValue !== "default" && !accounts.some(account => account.id === currentValue) && (
                          <option value={currentValue} disabled>已保存的固定账号（当前不可用）</option>
                        )}
                        {accounts.map((acc) => (
                          <option key={acc.id} value={acc.id}>
                            {acc.label || acc.email || acc.username || acc.provider || acc.key_hint || acc.id}
                          </option>
                        ))}
                      </InlineSelect>
                      {accountErrors[id] && <small className="field-help t3code-binding-error" role="status">{accountErrors[id]}</small>}
                    </div>
                  );
                })}
              </div>
            </div>
          )}
        </div>
      )}
    </section>
  );
}
