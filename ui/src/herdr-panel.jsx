import React, { useEffect, useState } from "react";
import {
  Check,
  Download,
  LoaderCircle,
  Play,
  RefreshCw,
  RotateCw,
  Square,
  TerminalSquare,
  Trash2,
} from "lucide-react";
import { AgentIcon } from "./agent-icon";
import { api } from "./api";
import "./t3code.css";
import "./magpie-herdr.css";

export function isHerdrManagedSupported(herdr, status = null) {
  if (herdr?.supports_managed === false || herdr?.data?.supports_managed === false) {
    return false;
  }
  if (
    (status?.guest_system === "windows-11" || status?.guest_system === "windows-remote") &&
    herdr?.supports_managed !== true &&
    herdr?.data?.supports_managed !== true
  ) {
    return false;
  }
  return true;
}

// The Herdr TUI that Super Cage 0.1.21-0.1.23 installed: one quiet line while
// it is still there, and its removal once it holds no terminals.
function HerdrLegacyLine({ busy, onConfirm }) {
  const [legacy, setLegacy] = useState(null);
  const [error, setError] = useState("");
  const [removing, setRemoving] = useState(false);
  useEffect(() => {
    let live = true;
    api("/herdr/legacy").then((result) => { if (live) setLegacy(result); }).catch(() => {});
    return () => { live = false; };
  }, []);
  if (!legacy?.installed) return null;
  const panes = Number(legacy.panes) || 0;
  const holding = Boolean(legacy.busy);
  const remove = async () => {
    const confirmed = onConfirm
      ? await onConfirm({
          title: "卸载旧版 Herdr？",
          body: "将停止旧版 Herdr 服务并删除它的程序，~/.config/herdr 里的数据会保留。",
          confirm: "卸载旧版",
        })
      : true;
    if (!confirmed) return;
    setRemoving(true);
    setError("");
    try {
      setLegacy(await api("/herdr/legacy/uninstall", {}));
    } catch (e) {
      setError(e.message);
    } finally {
      setRemoving(false);
    }
  };
  return (
    <div className="herdr-legacy-line" role="status">
      <span>
        {legacy.server_running && panes > 0
          ? `旧版 Herdr 仍在运行，里面有 ${panes} 个终端`
          : legacy.server_running ? "旧版 Herdr 仍在运行" : "旧版 Herdr 还留在环境里"}
      </span>
      <button
        type="button"
        disabled={busy || removing || holding}
        title={holding ? "旧版里还有终端在运行，卸载会结束它们。请先在旧版 Herdr 里关掉这些终端。" : undefined}
        onClick={() => void remove()}
      >
        {removing ? <LoaderCircle size={13} className="spin" /> : <Trash2 size={13} />}
        卸载旧版
      </button>
      {holding && <small>先在旧版里关掉这些终端，才能卸载。</small>}
      {error && <small className="herdr-legacy-error">{error}</small>}
    </div>
  );
}

export function HerdrSettingsCard({
  store,
  status,
  onConfirm,
  onOpenHerdr,
}) {
  const supportsManaged = isHerdrManagedSupported(store, status);
  const [submitting, setSubmitting] = useState("");
  const [actionError, setActionError] = useState("");
  const [savedNotice, setSavedNotice] = useState("");

  const vmRunning = Boolean(status?.vm_running);
  const envReady = Boolean(status?.ready);
  const jobRunning = Boolean(status?.job?.running);
  const isInstallJob = jobRunning && status?.job?.kind === "herdr-install";
  const isUninstallJob = jobRunning && status?.job?.kind === "herdr-uninstall";
  const busy = jobRunning || Boolean(submitting);

  if (!store) return null;

  const terminals = Number(store.terminals_count) || 0;

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

  // Stopping the core (and uninstalling, which stops it) ends every program
  // in it, Super Cage's or another Herdr client's: say so first.
  const confirmEnding = async (title, confirm) => {
    if (!terminals || !onConfirm) return true;
    return await onConfirm({
      title,
      body: `Herdr 里还有 ${terminals} 个终端在运行（包括其他 Herdr 客户端打开的），继续会结束它们。`,
      confirm,
    });
  };

  const toggleEnabled = (checked) => {
    if (!supportsManaged && checked) return;
    void runAction("toggle", () => store.saveConfig({ enabled: checked }));
  };

  const saveTakeover = (takeover_terminals) => {
    void runAction(
      "takeover",
      () => store.saveConfig({ takeover_terminals }),
      takeover_terminals ? "新终端会放进 Herdr" : "新终端恢复直连",
    );
  };

  const handleStop = async () => {
    if (!(await confirmEnding("停止 Herdr 服务？", "停止服务"))) return;
    void runAction("stop", () => store.stop(terminals > 0), "Herdr 服务已停止");
  };

  const handleUninstall = async () => {
    const confirmed = terminals
      ? await confirmEnding("卸载 Herdr？", "确认卸载")
      : onConfirm
        ? await onConfirm({
            title: "卸载 Herdr？",
            body: "将删除环境里的 herdr-core；终端记录和设置会保留，新终端恢复直连。",
            confirm: "确认卸载",
          })
        : true;
    if (!confirmed) return;
    void runAction("uninstall", () => store.uninstall(terminals > 0));
  };

  const expanded = Boolean(store.enabled && supportsManaged);
  const canOpen = Boolean(store.enabled && supportsManaged && store.installed && vmRunning);

  return (
    <section className="t3code-card herdr-card" aria-label="Herdr 持久终端">
      <div className="t3code-card-top">
        <AgentIcon agentId="herdr" size={36} />
        <div className="t3code-card-info">
          <div className="t3code-card-title">
            <strong>Herdr 持久终端</strong>
            <span className="agent-pill">可选增强</span>
            {!supportsManaged ? (
              <span className="agent-pill">当前客机暂不支持</span>
            ) : store.enabled ? (
              store.server_running ? (
                <span className="agent-pill agent-pill--ok">运行中</span>
              ) : store.installed ? (
                <span className="agent-pill">已安装 · 按需启动</span>
              ) : (
                <span className="agent-pill">尚未安装</span>
              )
            ) : (
              <span className="agent-pill">未启用</span>
            )}
            {supportsManaged && store.version && (
              <span className="agent-pill">v{store.version.replace(/^v/i, "")}</span>
            )}
            {supportsManaged && store.server_running && (
              <span className="agent-pill">{terminals} 个终端</span>
            )}
          </div>
          <p className="agent-card-desc">
            终端运行在环境里的 Herdr（herdr-core）中：关掉页面或断线后程序继续运行，再打开就接上。环境里其他 Herdr 客户端开的终端也会出现在终端列表里。
          </p>
        </div>
        <label className="t3code-optin-switch">
          <input
            type="checkbox"
            role="switch"
            aria-label="启用 Herdr"
            checked={Boolean(store.enabled)}
            disabled={busy || !store.data || (!supportsManaged && !store.enabled)}
            onChange={(e) => toggleEnabled(e.target.checked)}
          />
        </label>
      </div>

      {!supportsManaged && (
        <div className="inline-info t3code-info-banner" role="status">
          <span>当前客机暂不支持 Herdr（需要 Linux 或 macOS 客机）。</span>
        </div>
      )}

      {(isInstallJob || isUninstallJob) && (
        <div className="agent-job-banner" role="status">
          <LoaderCircle size={16} className="spin" />
          <span>
            {status.job.message || (isUninstallJob ? "正在卸载 Herdr…" : "正在安装 Herdr…")}
          </span>
        </div>
      )}

      {(actionError || store.error || store.serverError) && (
        <div className="error-message" role="alert">
          {actionError || store.error || store.serverError}
        </div>
      )}

      {!store.data && <button type="button" disabled={busy || store.loading} onClick={() => void store.refresh()}>
        <RefreshCw size={14} />刷新 Herdr 状态
      </button>}
      {store.data && supportsManaged && (
        <div className="t3code-card-body">
          <div className="t3code-quick-actions">
            {expanded && store.installed && onOpenHerdr && (
              <button
                type="button"
                className="primary"
                title="打开 Herdr 的终端选择器"
                disabled={!canOpen || busy}
                onClick={() => void runAction("open", () => onOpenHerdr?.())}
              >
                {submitting === "open" ? <LoaderCircle size={14} className="spin" /> : <TerminalSquare size={14} />}
                打开 Herdr
              </button>
            )}
            {expanded && store.installed && (
              store.server_running ? (
                <>
                  <button
                    type="button"
                    title="重启服务，终端里的程序继续运行"
                    disabled={!vmRunning || busy}
                    onClick={() => void runAction("restart", () => store.restart(), "Herdr 服务已重启")}
                  >
                    {submitting === "restart" ? <LoaderCircle size={14} className="spin" /> : <RotateCw size={14} />}
                    重启服务
                  </button>
                  <button type="button" disabled={!vmRunning || busy} onClick={() => void handleStop()}>
                    <Square size={13} />
                    停止服务
                  </button>
                </>
              ) : (
                <button
                  type="button"
                  disabled={!vmRunning || busy}
                  onClick={() => void runAction("start", () => store.start(), "Herdr 服务已启动")}
                >
                  {submitting === "start" ? <LoaderCircle size={14} className="spin" /> : <Play size={13} />}
                  启动服务
                </button>
              )
            )}
            <button
              type="button"
              disabled={store.loading || busy}
              onClick={() => void store.refresh(true)}
              title="重新检测 Herdr 的安装与运行状态"
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

          {expanded && <div className="herdr-install-row">
            <div className="herdr-install-actions">
              <button
                type="button"
                className={store.installed ? "" : "primary"}
                disabled={!envReady || busy}
                onClick={() => void runAction("install", () => store.install(store.installed))}
              >
                {submitting === "install" ? <LoaderCircle size={14} className="spin" /> : <Download size={14} />}
                {store.installed ? "检查更新" : "一键安装 Herdr"}
              </button>
              {store.installed && (
                <button
                  type="button"
                  className="herdr-uninstall-btn"
                  disabled={!envReady || busy}
                  onClick={() => void handleUninstall()}
                >
                  {submitting === "uninstall" ? <LoaderCircle size={14} className="spin" /> : <Trash2 size={14} />}
                  卸载 Herdr
                </button>
              )}
            </div>
            <p className="herdr-install-note">
              从 <code>releases.herdr.dev</code> 官方发布渠道安装 herdr-core，并核对 SHA-256；环境里已有可用的 herdr-core 时直接沿用。
              {store.binary_path && (
                <>
                  {" "}当前路径：<code>{store.binary_path}</code>
                </>
              )}
            </p>
            {store.data?.legacy_installed && <HerdrLegacyLine busy={busy} onConfirm={onConfirm} />}
          </div>}

          <label
            className="toggle-row herdr-takeover-row"
            data-active={store.takeover_terminals !== false ? "true" : undefined}
          >
            <span>
              <strong>新建终端都放进 Herdr</strong>
              <small>
                启用 Herdr 后，普通终端、Claude、Codex 和各 Agent CLI 终端都在 Herdr 里运行。账号登录终端仍然直连。
              </small>
            </span>
            <input
              type="checkbox"
              role="switch"
              checked={store.takeover_terminals !== false}
              disabled={busy}
              onChange={(e) => saveTakeover(e.target.checked)}
            />
          </label>
        </div>
      )}
    </section>
  );
}
