import React, { useState } from "react";
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
import { InlineSelect } from "./inline-select";
import "./t3code.css";
import "./magpie-herdr.css";

const HERDR_THEME_OPTIONS = [
  { value: "default", label: "保持 Herdr 原生配置" },
  { value: "terminal", label: "跟随 Super Cage 终端主题" },
  { value: "catppuccin", label: "Catppuccin" },
  { value: "tokyo-night", label: "Tokyo Night" },
  { value: "dracula", label: "Dracula" },
  { value: "nord", label: "Nord" },
  { value: "gruvbox", label: "Gruvbox" },
  { value: "one-dark", label: "One Dark" },
  { value: "solarized", label: "Solarized" },
  { value: "kanagawa", label: "Kanagawa" },
  { value: "rose-pine", label: "Rose Pine" },
  { value: "vesper", label: "Vesper" },
];

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
    if (!supportsManaged && checked) return;
    void runAction("toggle", () => store.saveConfig({ enabled: checked }));
  };

  const saveTakeover = (takeover_terminals) => {
    void runAction(
      "takeover",
      () => store.saveConfig({ takeover_terminals }),
      takeover_terminals ? "已开启终端自动接管" : "已恢复默认终端启动方式",
    );
  };

  const saveTheme = (theme) => {
    void runAction("theme", () => store.saveConfig({ theme }), "Herdr 界面配色已保存");
  };

  const handleUninstall = async () => {
    const confirmed = onConfirm
      ? await onConfirm({
          title: "卸载 Herdr？",
          body: "将停止 Herdr 守护服务并卸载客机中的 Herdr 程序；您的 ~/.config/herdr 配置目录会被安全保留，工作台终端将自动恢复为默认方式。",
          confirm: "确认卸载",
        })
      : true;
    if (!confirmed) return;
    void runAction("uninstall", () => store.uninstall());
  };

  const expanded = Boolean(store.enabled && supportsManaged);
  const canOpen = Boolean(store.enabled && supportsManaged && store.installed && vmRunning);

  return (
    <section className="t3code-card herdr-card" aria-label="Herdr 多 Agent 持久终端工作区">
      <div className="t3code-card-top">
        <AgentIcon agentId="herdr" size={36} />
        <div className="t3code-card-info">
          <div className="t3code-card-title">
            <strong>Herdr 多 Agent 持久终端工作区</strong>
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
              <>
                <span className="agent-pill">{store.workspaces_count} 工作区</span>
                <span className="agent-pill">{store.agents_count} Agent</span>
                {store.blocked_agents_count > 0 && (
                  <span
                    className="agent-pill herdr-pill--blocked"
                    title="有 Agent 正在等待审批或用户输入"
                  >
                    {store.blocked_agents_count} 待处理
                  </span>
                )}
              </>
            )}
          </div>
          <p className="agent-card-desc">
            可选安装 Herdr 多 Agent 持久终端工作区（基于 Ghostty-VT）：安装启用后，工作台所有普通终端与 Agent CLI 终端均可自动在 Herdr 中运行，实时追踪 Working / Blocked / Done 状态，支持键盘前缀（Ctrl+B）与鼠标分屏，关闭标签后后台任务持续保活。
          </p>
        </div>
        <label className="t3code-optin-switch">
          <input
            type="checkbox"
            role="switch"
            aria-label="启用 Herdr 工作区"
            checked={Boolean(store.enabled)}
            disabled={busy || !supportsManaged}
            onChange={(e) => toggleEnabled(e.target.checked)}
          />
        </label>
      </div>

      {!supportsManaged && (
        <div className="inline-info t3code-info-banner" role="status">
          <span>当前客机环境暂不支持托管 Herdr 持久终端工作区（需 Linux / macOS 客机环境）。</span>
        </div>
      )}

      {(isInstallJob || isUninstallJob) && (
        <div className="agent-job-banner" role="status">
          <LoaderCircle size={16} className="spin" />
          <span>
            {status.job.message ||
              (isUninstallJob ? "正在卸载 Herdr 并恢复默认终端方式…" : "正在客机环境中安装 Herdr…")}
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
            {store.installed && onOpenHerdr && (
              <button
                type="button"
                className="primary"
                disabled={!canOpen || busy}
                onClick={() => void runAction("open", () => onOpenHerdr?.())}
              >
                {submitting === "open" ? <LoaderCircle size={14} className="spin" /> : <TerminalSquare size={14} />}
                立即打开 Herdr 工作区
              </button>
            )}
            {store.installed && (
              <>
                {store.server_running ? (
                  <>
                    <button
                      type="button"
                      disabled={!vmRunning || busy}
                      onClick={() => void runAction("restart", () => store.restart(), "Herdr 守护服务已重启")}
                    >
                      {submitting === "restart" ? <LoaderCircle size={14} className="spin" /> : <RotateCw size={14} />}
                      重启守护服务
                    </button>
                    <button
                      type="button"
                      disabled={!vmRunning || busy}
                      onClick={() => void runAction("stop", () => store.stop(), "Herdr 守护服务已停止")}
                    >
                      <Square size={13} />
                      停止守护服务
                    </button>
                  </>
                ) : (
                  <button
                    type="button"
                    disabled={!vmRunning || busy}
                    onClick={() => void runAction("start", () => store.start(), "Herdr 守护服务已启动")}
                  >
                    {submitting === "start" ? <LoaderCircle size={14} className="spin" /> : <Play size={13} />}
                    启动守护服务
                  </button>
                )}
              </>
            )}
            <button
              type="button"
              disabled={store.loading || busy}
              onClick={() => void store.refresh(true)}
              title="重新检测 Herdr 安装与守护服务状态"
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

          <div className="herdr-install-row">
            <div className="herdr-install-actions">
              <button
                type="button"
                className={store.installed ? "" : "primary"}
                disabled={!envReady || busy}
                onClick={() => void runAction("install", () => store.install())}
              >
                {submitting === "install" ? <LoaderCircle size={14} className="spin" /> : <Download size={14} />}
                {store.installed ? "更新 / 重装 Herdr" : "一键安装 Herdr"}
              </button>
              {store.installed && (
                <button
                  type="button"
                  className="herdr-uninstall-btn"
                  disabled={!envReady || busy}
                  onClick={handleUninstall}
                >
                  {submitting === "uninstall" ? <LoaderCircle size={14} className="spin" /> : <Trash2 size={14} />}
                  卸载 Herdr
                </button>
              )}
            </div>
            <p className="herdr-install-note">
              通过官方 <code>herdr.dev/install.sh</code> 安装并进行 SHA-256 完整性校验。
              {store.binary_path && (
                <>
                  {" "}当前路径：<code>{store.binary_path}</code>
                </>
              )}
            </p>
          </div>

          <label
            className="toggle-row herdr-takeover-row"
            data-active={store.takeover_terminals !== false ? "true" : undefined}
          >
            <span>
              <strong>用 Herdr 自动接管所有工作台终端</strong>
              <small>
                开启后，新建普通终端、Claude、Codex 及所有 Agent CLI 终端均自动在 Herdr 工作区中启动并持续保活；账号 OAuth 登录终端仍保持独立直连。
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

          <div className="herdr-theme-row">
            <label>
              Herdr 界面配色
              <InlineSelect
                aria-label="Herdr 界面配色"
                value={store.theme || "default"}
                disabled={busy}
                onValueChange={saveTheme}
              >
                {HERDR_THEME_OPTIONS.map((opt) => (
                  <option key={opt.value} value={opt.value}>
                    {opt.label}
                  </option>
                ))}
              </InlineSelect>
            </label>
          </div>
        </div>
      )}
    </section>
  );
}
