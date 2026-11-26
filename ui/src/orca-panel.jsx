import React, { useEffect, useMemo, useState } from "react";
import {
  Check,
  Copy,
  Download,
  ExternalLink,
  FolderSync,
  KeyRound,
  LoaderCircle,
  Play,
  QrCode,
  RefreshCw,
  RotateCw,
  ShieldCheck,
  Smartphone,
  Square,
  Trash2,
} from "lucide-react";
import { api } from "./api";
import { AgentIcon } from "./agent-icon";
import { InlineSelect } from "./inline-select";
import { Modal } from "./modal";
import "./t3code.css";
import "./orca.css";

const BINDING_PRIORITY = ["claude", "codex", "antigravity", "opencode"];

export function isOrcaManagedSupported(orca, status = null) {
  if (orca?.supports_managed === false || orca?.data?.supports_managed === false) {
    return false;
  }
  if (
    (status?.guest_system === "windows-11" || status?.guest_system === "windows-remote") &&
    orca?.supports_managed !== true &&
    orca?.data?.supports_managed !== true
  ) {
    return false;
  }
  return true;
}

export function OrcaPairModal({ open, onClose, store }) {
  const [scope, setScope] = useState("desktop");
  const [loading, setLoading] = useState(false);
  const [offer, setOffer] = useState(null);
  const [error, setError] = useState("");
  const [copied, setCopied] = useState(false);

  const fetchOffer = async (targetScope, rotate = false) => {
    if (!store) return;
    setLoading(true);
    setError("");
    setCopied(false);
    try {
      const res = await store.pair({ mobile: targetScope === "mobile", rotate });
      setOffer(res);
    } catch (e) {
      setOffer(null);
      setError(e.message);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (!open) return;
    void fetchOffer(scope, false);
  }, [open, scope]);

  if (!open) return null;

  const copyLink = async () => {
    if (!offer?.pairing_url) return;
    try {
      await navigator.clipboard.writeText(offer.pairing_url);
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    } catch {
      setError("复制失败，请手动选中上方配对链接复制");
    }
  };

  return (
    <Modal title="Orca Server 配对连接" onClose={onClose}>
      <div className="orca-pair-modal">
        <div className="t3code-mode-switch" role="tablist" aria-label="配对设备类型">
          <button
            type="button"
            role="tab"
            aria-selected={scope === "desktop"}
            className={`t3code-mode-btn ${scope === "desktop" ? "selected" : ""}`}
            disabled={loading}
            onClick={() => setScope("desktop")}
          >
            Orca Desktop（电脑端）
          </button>
          <button
            type="button"
            role="tab"
            aria-selected={scope === "mobile"}
            className={`t3code-mode-btn ${scope === "mobile" ? "selected" : ""}`}
            disabled={loading}
            onClick={() => setScope("mobile")}
          >
            <Smartphone size={13} aria-hidden="true" /> Orca Mobile（手机端）
          </button>
        </div>

        <p className="t3code-mode-hint">
          {scope === "desktop"
            ? "在 Orca Desktop 中打开 Settings → Remote Orca Servers → Add Server，粘贴下方配对链接即可端到端加密连接（如使用本机回环地址，请保持 Super Cage 处于解锁状态）。"
            : "在手机 Orca Mobile 中扫描下方二维码或粘贴链接。手机直连需客机已启用 Tailscale 私有网络且与手机在同一 tailnet 内。"}
        </p>

        {loading && (
          <div className="inline-info t3code-info-banner">
            <LoaderCircle size={15} className="spin" />
            <span>正在从客机 Orca Server 生成端到端加密配对码…</span>
          </div>
        )}

        {error && <div className="error-message" role="alert">{error}</div>}

        {!loading && offer?.pairing_url && (
          <div className="orca-pair-box">
            {offer.qr && (
              <pre className="orca-pair-qr" aria-label="Orca Mobile 配对二维码">
                {offer.qr}
              </pre>
            )}
            <div className="orca-pair-link-row">
              <input
                type="text"
                readOnly
                value={offer.pairing_url}
                aria-label="Orca 配对链接"
                onFocus={(e) => e.target.select()}
              />
              <button type="button" className="primary" onClick={copyLink}>
                {copied ? <Check size={14} /> : <Copy size={14} />}
                {copied ? "已复制" : "复制链接"}
              </button>
            </div>
            <div className="orca-pair-meta">
              {offer.endpoint && <span>目标地址：<code>{offer.endpoint}</code></span>}
              {offer.device_id && <span>设备标识：<code>{offer.device_id.slice(0, 12)}…</code></span>}
            </div>
          </div>
        )}

        <div className="orca-pair-actions">
          <button
            type="button"
            disabled={loading}
            onClick={() => void fetchOffer(scope, true)}
            title="作废未使用的旧配对码并生成全新一次性配对链接"
          >
            <RefreshCw size={14} className={loading ? "spin" : undefined} />
            轮换配对码
          </button>
          <button type="button" onClick={onClose}>关闭</button>
        </div>
      </div>
    </Modal>
  );
}

export function OrcaSettingsCard({
  store,
  agentsStore,
  status,
  onConfirm,
  onOpenOrca,
  onShowPairModal,
}) {
  const supportsManaged = isOrcaManagedSupported(store, status);
  const [serverName, setServerName] = useState(store?.server_name || "");
  const [guestPortInput, setGuestPortInput] = useState(String(store?.guest_port || 16768));
  const [hostPortInput, setHostPortInput] = useState(store?.host_tunnel_port ? String(store.host_tunnel_port) : "");
  const [memLimitInput, setMemLimitInput] = useState(store?.terminal_memory_max || "");
  const [submitting, setSubmitting] = useState("");
  const [actionError, setActionError] = useState("");
  const [savedNotice, setSavedNotice] = useState("");
  const [bindingRestartHint, setBindingRestartHint] = useState(false);
  const [accountOptions, setAccountOptions] = useState({});

  const vmRunning = Boolean(status?.vm_running);
  const envReady = Boolean(status?.ready);
  const jobRunning = Boolean(status?.job?.running);
  const isInstallJob = jobRunning && status?.job?.kind === "orca-install";
  const isUninstallJob = jobRunning && status?.job?.kind === "orca-uninstall";
  const busy = jobRunning || Boolean(submitting);

  useEffect(() => {
    if (store?.server_name) setServerName(store.server_name);
  }, [store?.server_name]);

  useEffect(() => {
    if (store?.guest_port) setGuestPortInput(String(store.guest_port));
  }, [store?.guest_port]);

  useEffect(() => {
    setHostPortInput(store?.host_tunnel_port ? String(store.host_tunnel_port) : "");
  }, [store?.host_tunnel_port]);

  useEffect(() => {
    setMemLimitInput(store?.terminal_memory_max || "");
  }, [store?.terminal_memory_max]);

  const bindableAgents = useMemo(() => {
    const installed = agentsStore?.installedAgents || [];
    return [...installed].sort((a, b) => {
      const ia = BINDING_PRIORITY.indexOf(a.spec.id);
      const ib = BINDING_PRIORITY.indexOf(b.spec.id);
      if (ia !== -1 && ib !== -1) return ia - ib;
      if (ia !== -1) return -1;
      if (ib !== -1) return 1;
      return a.spec.name.localeCompare(b.spec.name);
    });
  }, [agentsStore?.installedAgents]);

  const bindableKey = bindableAgents.map((e) => e.spec.id).join(",");

  useEffect(() => {
    if (!store?.enabled || !supportsManaged || !bindableKey) return;
    let live = true;
    const ids = bindableKey.split(",");
    Promise.allSettled(
      ids.map(async (id) => {
        if (id === "claude") {
          const res = await api("/claude-accounts");
          return [id, res?.accounts || []];
        }
        if (id === "codex") {
          const [nativeRes, agentRes] = await Promise.allSettled([
            api("/codex-accounts"),
            api("/agent-clis/codex/accounts?skip_detect=1"),
          ]);
          const nativeAccts = nativeRes.status === "fulfilled" && nativeRes.value?.guest_supported
            ? (nativeRes.value?.accounts || [])
            : [];
          const agentAccts = agentRes.status === "fulfilled" ? (agentRes.value?.accounts || []) : [];
          return [id, nativeAccts.length > 0 ? nativeAccts : agentAccts];
        }
        const res = await api(`/agent-clis/${encodeURIComponent(id)}/accounts?skip_detect=1`);
        return [id, res?.accounts || []];
      }),
    ).then((results) => {
      if (!live) return;
      const next = {};
      for (const item of results) {
        if (item.status === "fulfilled") {
          const [id, accounts] = item.value;
          next[id] = accounts;
        }
      }
      setAccountOptions(next);
    });
    return () => { live = false; };
  }, [store?.enabled, supportsManaged, bindableKey]);

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

  const saveNetworkSettings = async (e) => {
    e.preventDefault();
    const gp = Number.parseInt(guestPortInput, 10);
    if (!Number.isInteger(gp) || gp < 1024 || gp > 65535) {
      setActionError("客机端口须为 1024 到 65535 之间的整数");
      return;
    }
    let hp = 0;
    if (hostPortInput.trim() !== "") {
      hp = Number.parseInt(hostPortInput, 10);
      if (!Number.isInteger(hp) || hp < 1024 || hp > 65535) {
        setActionError("本机隧道端口须留空（自动分配）或为 1024 到 65535 之间的整数");
        return;
      }
    }
    await runAction(
      "save",
      () => store.saveConfig({
        server_name: serverName.trim(),
        guest_port: gp,
        host_tunnel_port: hp,
        terminal_memory_max: memLimitInput.trim().toUpperCase(),
      }),
      "Orca Server 设置已保存",
    );
  };

  const changeBinding = (agentId, value) => {
    const next = { ...(store.account_bindings || {}), [agentId]: value };
    void runAction(`bind:${agentId}`, async () => {
      const res = await store.saveConfig({ account_bindings: next });
      if (res && store.running && value !== "auto") {
        setBindingRestartHint(true);
      } else {
        setBindingRestartHint(false);
      }
      return res;
    }, value === "auto" ? "已切换为动态自动调度（即时生效）" : "账号固定绑定已保存");
  };

  const doUninstall = async () => {
    const ok = onConfirm
      ? await onConfirm({
          title: "卸载客机中的 Orca Server？",
          body: "将停止托管的 Orca Server 并删除程序文件，历史会话、工作区记录与配置数据会安全保留。",
          confirm: "确认卸载",
        })
      : true;
    if (!ok) return;
    await runAction("uninstall", () => store.uninstall({ purgeData: false, removePaired: false }));
  };

  const doResetIdentity = async () => {
    const ok = onConfirm
      ? await onConfirm({
          title: "重置 Orca Server 配对身份？",
          body: "将生成全新的服务端加密身份并作废所有已配对的电脑/手机凭证。重置后需重新一键连接或扫码配对。",
          confirm: "确认重置身份",
        })
      : true;
    if (!ok) return;
    await runAction("reset-id", () => store.resetIdentity(), "已重置 Orca 配对身份");
  };

  const desktopVer = store.local_desktop?.version;
  const serverVer = store.version;
  const versionDiff = Boolean(desktopVer && serverVer && desktopVer !== serverVer);

  return (
    <section className="t3code-card" aria-label="Orca Server 可选服务端接入">
      <div className="t3code-card-top">
        <AgentIcon agentId="orca" size={34} />
        <div className="t3code-card-info">
          <div className="t3code-card-title">
            <strong>Orca Server（原生专业服务端）</strong>
            <span className="agent-pill">可选扩展</span>
            {store.enabled && (
              <>
                {store.installed && (
                  <span className="agent-pill">
                    {serverVer ? `v${serverVer.replace(/^v/i, "")}` : "已安装"}
                  </span>
                )}
                {store.running ? (
                  <span className="agent-pill agent-pill--ok">运行中</span>
                ) : store.installed ? (
                  <span className="agent-pill">已停止</span>
                ) : (
                  <span className="agent-pill">未安装</span>
                )}
                {store.can_auto_pair && (
                  <span className={`agent-pill ${store.paired ? "agent-pill--ok" : ""}`}>
                    {store.paired
                      ? `本机 Orca 已配对${store.paired_name ? ` · ${store.paired_name}` : ""}`
                      : `检测到本机 Orca${desktopVer ? ` v${desktopVer}` : ""}`}
                  </span>
                )}
                {store.tunnel_active && store.tunnel_port > 0 && (
                  <span className="agent-pill agent-pill--ok">
                    隧道 127.0.0.1:{store.tunnel_port}
                  </span>
                )}
              </>
            )}
          </div>
          <p className="agent-card-desc">
            在隔离客机内安装官方纯服务端版 Orca（<code>orca-server</code>，无 Electron/GUI 依赖），通过端到端加密通道直连本机原生 Orca Desktop，自动复用 Cage 全部 Agent CLI 多账号调度与隔离出口。默认关闭，开启后零侵入生效。
          </p>
        </div>
        <label className="t3code-optin-switch" title={store.enabled ? "关闭 Orca Server 接入" : "启用 Orca Server 接入"}>
          <input
            type="checkbox"
            role="switch"
            aria-label="启用 Orca Server 服务端接入"
            checked={store.enabled}
            disabled={busy || (!supportsManaged && !store.enabled)}
            onChange={(e) => toggleEnabled(e.target.checked)}
          />
        </label>
      </div>

      {!supportsManaged && (
        <div className="inline-info t3code-info-banner">
          <span>当前 Windows 原生客机暂不支持托管 Orca Server（官方服务端仅提供 Linux 与 macOS 运行时）。</span>
        </div>
      )}

      {store.enabled && supportsManaged && (
        <div className="t3code-card-body">
          <div className="orca-security-strip">
            <ShieldCheck size={14} aria-hidden="true" />
            <span>
              隔离护栏已启用：<code>--refuse-public</code> 禁止公网监听 · OAuth 浏览器强制留在客机防 IP 串流 · 遥测关闭 · 锁屏立即断隧道
            </span>
          </div>

          <div className="t3code-quick-actions">
            {!store.installed ? (
              <button
                type="button"
                className="primary"
                disabled={busy || !vmRunning || !envReady}
                onClick={() => void runAction("install", () => store.install("match_desktop"))}
              >
                {isInstallJob || submitting === "install"
                  ? <LoaderCircle size={14} className="spin" />
                  : <Download size={14} />}
                {store.is_macos_guest
                  ? "检测客机 Orca.app 运行时"
                  : desktopVer
                    ? `一键安装官方 Orca Server (v${desktopVer})`
                    : "一键安装官方 Orca Server（最新服务端版）"}
              </button>
            ) : (
              <>
                {store.can_auto_pair ? (
                  <button
                    type="button"
                    className="primary"
                    disabled={busy || !vmRunning || !envReady}
                    onClick={() => void runAction(
                      "open",
                      async () => {
                        const res = await (onOpenOrca ? onOpenOrca() : store.open());
                        if (res?.newly_paired) {
                          setSavedNotice(`已自动完成端到端配对并打开本机 Orca（${res.environment_name || store.server_name}）`);
                        } else if (res?.opened) {
                          setSavedNotice("已唤醒本机 Orca 客户端");
                        }
                        return res;
                      },
                    )}
                  >
                    {submitting === "open"
                      ? <LoaderCircle size={14} className="spin" />
                      : <ExternalLink size={14} />}
                    {store.paired ? "在 Orca 中打开" : "一键配对并在 Orca 中打开"}
                  </button>
                ) : (
                  <button
                    type="button"
                    className="primary"
                    disabled={busy || !vmRunning || !envReady}
                    onClick={() => onShowPairModal?.()}
                  >
                    <QrCode size={14} />
                    获取配对码 / 二维码
                  </button>
                )}

                {store.can_auto_pair && (
                  <button
                    type="button"
                    disabled={busy || !vmRunning || !envReady}
                    onClick={() => onShowPairModal?.()}
                  >
                    <QrCode size={14} />
                    配对码 / 手机扫码
                  </button>
                )}

                {store.running ? (
                  <>
                    <button
                      type="button"
                      disabled={busy || !vmRunning}
                      onClick={() => void runAction("restart", async () => {
                        const res = await store.restart();
                        if (res) setBindingRestartHint(false);
                        return res;
                      }, "Orca Server 已重启")}
                    >
                      {submitting === "restart" ? <LoaderCircle size={14} className="spin" /> : <RotateCw size={14} />}
                      重启
                    </button>
                    <button
                      type="button"
                      disabled={busy || !vmRunning}
                      onClick={() => void runAction("stop", () => store.stop(), "Orca Server 已停止")}
                    >
                      {submitting === "stop" ? <LoaderCircle size={14} className="spin" /> : <Square size={14} />}
                      停止
                    </button>
                  </>
                ) : (
                  <button
                    type="button"
                    disabled={busy || !vmRunning || !envReady}
                    onClick={() => void runAction("start", () => store.start(), "Orca Server 已启动")}
                  >
                    {submitting === "start" ? <LoaderCircle size={14} className="spin" /> : <Play size={14} />}
                    启动服务
                  </button>
                )}

                <button
                  type="button"
                  disabled={busy || !vmRunning || !envReady}
                  onClick={() => void runAction("sync", async () => {
                    const res = await store.syncProjects();
                    const addedCount = Array.isArray(res?.added) ? res.added.length : 0;
                    setSavedNotice(
                      addedCount > 0
                        ? `已将 ${addedCount} 个新授权项目同步到 Orca`
                        : `全部 ${res?.total || 0} 个授权项目已在 Orca 中就绪`,
                    );
                    return res;
                  })}
                >
                  {submitting === "sync" ? <LoaderCircle size={14} className="spin" /> : <FolderSync size={14} />}
                  同步项目
                </button>

                {!store.is_macos_guest && (
                  <button
                    type="button"
                    disabled={busy || !vmRunning || !envReady}
                    onClick={() => void runAction("update", () => store.install("match_desktop"))}
                    title="原子更新至与本机 Orca 匹配的版本（或最新官方版），保留上一版本用于回滚"
                  >
                    {isInstallJob || submitting === "update"
                      ? <LoaderCircle size={14} className="spin" />
                      : <RefreshCw size={14} />}
                    {versionDiff ? `对齐至 v${desktopVer}` : "更新服务端"}
                  </button>
                )}
              </>
            )}
          </div>

          {!store.can_auto_pair && !store.local_desktop?.installed && (
            <div className="inline-info t3code-info-banner">
              <span>
                未在本机检测到 Orca 桌面客户端。可先安装官方客户端（<code>{store.download_url}</code>）享受一键免密连接，或直接点击「配对码」在其他设备的 Orca 中添加此服务器。
              </span>
            </div>
          )}

          {versionDiff && (
            <div className="inline-info t3code-info-banner">
              <span>
                客机 Orca Server（v{serverVer}）与本机 Orca Desktop（v{desktopVer}）版本略有差异，建议点击「对齐至 v{desktopVer}」以获得最佳协议兼容性。
              </span>
            </div>
          )}

          <div className="t3code-field">
            <span className="t3code-field-label">监听网络模式</span>
            <div className="t3code-mode-switch" role="radiogroup" aria-label="Orca Server 监听模式">
              <button
                type="button"
                role="radio"
                aria-checked={store.bind_mode !== "tailscale"}
                className={`t3code-mode-btn ${store.bind_mode !== "tailscale" ? "selected" : ""}`}
                disabled={busy}
                onClick={() => void runAction("bind-loopback", () => store.saveConfig({ bind_mode: "loopback" }), "已切换为仅本机回环隧道模式")}
              >
                仅本机回环隧道（最安全）
              </button>
              <button
                type="button"
                role="radio"
                aria-checked={store.bind_mode === "tailscale"}
                className={`t3code-mode-btn ${store.bind_mode === "tailscale" ? "selected" : ""}`}
                disabled={busy}
                onClick={() => void runAction("bind-ts", () => store.saveConfig({ bind_mode: "tailscale" }), "已切换为 Tailscale 私有网络模式")}
              >
                Tailscale 私有网络{store.tailscale_ip ? ` (${store.tailscale_ip})` : ""}
              </button>
            </div>
            <p className="t3code-mode-hint">
              {store.bind_mode === "tailscale"
                ? "仅监听客机 Tailscale 100.x 私有地址，允许同 tailnet 下的多台电脑和手机 Orca Mobile 端到端加密直连。"
                : "客机仅监听 127.0.0.1，由 Super Cage 维护绑定解锁周期的本机加密 SSH 隧道，任何局域网/公网设备均无法触达。"}
            </p>
          </div>

          <form className="t3code-config-grid" onSubmit={saveNetworkSettings}>
            <label>
              <span className="t3code-field-label">在 Orca 中显示的服务器名称</span>
              <input
                type="text"
                value={serverName}
                onChange={(e) => setServerName(e.target.value)}
                placeholder="Super Cage"
                disabled={busy}
              />
            </label>
            <label>
              <span className="t3code-field-label">客机服务端口</span>
              <input
                type="number"
                min={1024}
                max={65535}
                value={guestPortInput}
                onChange={(e) => setGuestPortInput(e.target.value)}
                disabled={busy}
              />
            </label>
            <label>
              <span className="t3code-field-label">本机固定隧道端口（留空自动避让）</span>
              <input
                type="number"
                min={1024}
                max={65535}
                value={hostPortInput}
                onChange={(e) => setHostPortInput(e.target.value)}
                placeholder="自动分配（16768+）"
                disabled={busy}
              />
            </label>
            <div className="t3code-port-form">
              <label>
                <span className="t3code-field-label">单终端内存上限（Linux 可选）</span>
                <input
                  type="text"
                  value={memLimitInput}
                  onChange={(e) => setMemLimitInput(e.target.value)}
                  placeholder="例如 2G（留空不限）"
                  disabled={busy}
                />
              </label>
              <button type="submit" disabled={busy}>
                {submitting === "save" ? <LoaderCircle size={14} className="spin" /> : <Check size={14} />}
                保存
              </button>
            </div>
          </form>

          <label className="toggle-row t3code-toggle-row">
            <span>
              <strong>自动同步 Cage 授权项目到 Orca</strong>
              <small>启动或打开 Orca 时，自动将 Cage 已授权的项目目录注册到 Orca 侧栏，免去逐个手动添加。</small>
            </span>
            <input
              type="checkbox"
              role="switch"
              checked={store.auto_sync_projects}
              disabled={busy}
              onChange={(e) => void runAction(
                "autosync",
                () => store.saveConfig({ auto_sync_projects: e.target.checked }),
                e.target.checked ? "已开启自动同步项目" : "已关闭自动同步项目",
              )}
            />
          </label>

          <div className="t3code-bindings">
            <div className="t3code-bindings-head">
              <strong>Agent CLI 多账号调度绑定</strong>
              <small>
                默认「跟随 Cage 自动调度」会在 Orca 的结构化对话、终端与子 Agent 中实时动态调度健康账号（额度耗尽或限流冷却自动避让，无需重启 Orca）。
              </small>
            </div>

            {bindingRestartHint && (
              <div className="inline-info t3code-restart-hint">
                <span>固定指定具体账号的变更需重启 Orca Server 后对新会话生效。</span>
                <button
                  type="button"
                  className="text-button"
                  disabled={busy || !vmRunning}
                  onClick={() => void runAction("restart", async () => {
                    const res = await store.restart();
                    if (res) setBindingRestartHint(false);
                    return res;
                  }, "已重启 Orca Server 并应用新账号绑定")}
                >
                  立即重启
                </button>
              </div>
            )}

            <div className="t3code-bindings-list">
              {bindableAgents.map((entry) => {
                const id = entry.spec.id;
                const currentVal = store.account_bindings?.[id] || "auto";
                const accounts = (accountOptions[id] || []).filter((a) => a.status === "ready" || a.ready);
                const activeLabel = store.active_bindings?.[id];
                return (
                  <div key={id} className="t3code-binding-row">
                    <div className="t3code-binding-agent">
                      <AgentIcon agentId={id} accentColor={entry.spec.accent_color} size={18} />
                      <span>{entry.spec.short_name || entry.spec.name}</span>
                      {activeLabel && <span className="agent-pill">{activeLabel}</span>}
                    </div>
                    <InlineSelect
                      aria-label={`${entry.spec.name} 账号绑定`}
                      value={currentVal}
                      disabled={busy}
                      onValueChange={(val) => changeBinding(id, val)}
                    >
                      <option value="auto">跟随 Cage 自动调度（推荐）</option>
                      <option value="default">使用客机默认环境登录</option>
                      {accounts.map((acc) => (
                        <option key={acc.id} value={acc.id}>
                          固定账号：{acc.label || acc.email || acc.username || acc.key_hint || acc.id.slice(0, 8)}
                        </option>
                      ))}
                    </InlineSelect>
                  </div>
                );
              })}
            </div>
          </div>

          <details className="orca-advanced">
            <summary>高级维护与重置选项</summary>
            <div className="orca-advanced-body">
              <div className="t3code-quick-actions">
                {store.can_auto_pair && store.installed && (
                  <button
                    type="button"
                    disabled={busy || !vmRunning || !envReady}
                    onClick={() => void runAction("repair", () => store.open({ force_repair: true }), "已重新配对本机 Orca 客户端")}
                  >
                    <RefreshCw size={14} />
                    重新配对本机 Orca
                  </button>
                )}
                {store.installed && (
                  <button
                    type="button"
                    disabled={busy || !vmRunning}
                    onClick={() => void doResetIdentity()}
                  >
                    <KeyRound size={14} />
                    重置配对身份
                  </button>
                )}
                {store.installed && !store.is_macos_guest && (
                  <button
                    type="button"
                    disabled={busy || !vmRunning}
                    onClick={() => void doUninstall()}
                  >
                    {isUninstallJob || submitting === "uninstall"
                      ? <LoaderCircle size={14} className="spin" />
                      : <Trash2 size={14} />}
                    卸载 Orca Server
                  </button>
                )}
              </div>
            </div>
          </details>

          {(actionError || store.serverError || store.error) && (
            <div className="error-message" role="alert">
              {actionError || store.serverError || store.error}
            </div>
          )}
          {savedNotice && (
            <div className="inline-info t3code-info-banner" role="status">
              <Check size={14} />
              <span>{savedNotice}</span>
            </div>
          )}
        </div>
      )}
    </section>
  );
}
