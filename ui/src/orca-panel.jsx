import React, { useEffect, useState } from "react";
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
import { useServiceAccounts } from "./use-service-accounts";
import { AgentIcon } from "./agent-icon";
import { InlineSelect } from "./inline-select";
import { Modal } from "./modal";
import "./t3code.css";
import "./orca.css";

function CodexSyncStatus({ bridge, accounts }) {
  if (!bridge || bridge.state === "inactive") return null;
  const profile = accounts.find(account => account.id === bridge.account_id);
  const name = profile?.label || profile?.email;
  const messages = {
    linked: name ? `Orca 与 Cage 已关联到 ${name}。` : "Orca 与 Cage 已关联到同一账号。",
    observing: "正在核对 Orca 与 Cage 的 Codex 账号。",
    unmatched: "Orca 当前账号尚未与 Cage 匹配。请登记已有 Codex 账号，或在两侧选择相同的 ChatGPT 账号和工作区。",
    unavailable: "暂时无法读取客机 Orca 的账号状态。",
  };
  return messages[bridge.state] ? <small className="field-help" role="status">{messages[bridge.state]}</small> : null;
}

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
  magpieEnabled = false,
  onConfirm,
  onOpenOrca,
  onShowPairModal,
  onOpenAgentSettings,
  onDirty,
}) {
  const supportsManaged = isOrcaManagedSupported(store, status);
  const hasConfig = Boolean(store?.data);
  const magpieTakeover = Boolean(magpieEnabled || store?.magpie_takeover);
  const savedName = store?.server_name || "";
  const savedGuestPort = String(store?.guest_port || 16768);
  const savedHostPort = store?.host_tunnel_port ? String(store.host_tunnel_port) : "";
  const savedMemLimit = store?.terminal_memory_max || "";
  const [draft, setDraft] = useState({});
  const serverName = draft.name ?? savedName;
  const guestPortInput = draft.guestPort ?? savedGuestPort;
  const hostPortInput = draft.hostPort ?? savedHostPort;
  const memLimitInput = draft.memory ?? savedMemLimit;
  const [submitting, setSubmitting] = useState("");
  const [actionError, setActionError] = useState("");
  const [savedNotice, setSavedNotice] = useState("");
  const [bindingRestartHint, setBindingRestartHint] = useState(false);
  const [registrationID, setRegistrationID] = useState("");

  const vmRunning = Boolean(status?.vm_running);
  const envReady = Boolean(status?.ready);
  const jobRunning = Boolean(status?.job?.running);
  const isInstallJob = jobRunning && status?.job?.kind === "orca-install";
  const isUninstallJob = jobRunning && status?.job?.kind === "orca-uninstall";
  const jobError = ["orca-install", "orca-uninstall"].includes(status?.job?.kind) ? status.job.error : "";
  const busy = jobRunning || Boolean(submitting);

  const edit = (field, value, saved) => setDraft(previous => ({ ...previous, [field]: value === saved ? undefined : value }));
  const dirty = serverName !== savedName || guestPortInput !== savedGuestPort || hostPortInput !== savedHostPort || memLimitInput !== savedMemLimit;
  useEffect(() => { onDirty?.(dirty); }, [dirty, onDirty]);
  useEffect(() => () => onDirty?.(false), [onDirty]);

  const { bindableAgents, accounts: accountOptions, errors: accountErrors, loading: accountsLoading, retry: retryAccounts } = useServiceAccounts(agentsStore, store?.account_bindings, {
    available: hasConfig && supportsManaged,
    service: "orca",
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

  const confirmInterruption = () => !store.running || onConfirm?.({
    title: "保存设置并断开 Orca 连接？",
    body: "地址变更或关闭接入会断开当前 Orca 连接。保存后可按需重新启动服务或配对。",
    confirm: "保存设置",
  });

  const registrationAccounts = (accountOptions.codex || []).filter(a => a.nativeCodex && a.status === "ready");
  const registration = registrationAccounts.find(a => a.id === registrationID);
  const registerAccount = async () => {
    if (!registration) return;
    const name = registration.label || registration.email || registration.id;
    if (!(await onConfirm?.({
      title: `将「${name}」登记到客机 Orca？`,
      body: "Orca 将在这台客机的当前系统用户下保存一份 Codex 登录资料。登记完成后，可在 Orca 的账号菜单中选择它。",
      confirm: "登记账号",
    }))) return;
    await runAction("register-codex", () => store.registerCodexAccount(registration.id), `「${name}」已登记到 Orca。`);
  };

  const toggleEnabled = async (checked) => {
    if (!supportsManaged && checked) return;
    if (!checked && !(await confirmInterruption())) return;
    void runAction("toggle", () => store.saveConfig({ enabled: checked }));
  };

  const saveNetworkSettings = async (e) => {
    e.preventDefault();
    if (busy || !dirty) return;
    setSavedNotice("");
    const gp = Number(guestPortInput);
    if (!Number.isInteger(gp) || gp < 1024 || gp > 65535) {
      setActionError("客机端口须为 1024 到 65535 之间的整数");
      return;
    }
    let hp = 0;
    if (hostPortInput.trim() !== "") {
      hp = Number(hostPortInput);
      if (!Number.isInteger(hp) || hp < 1024 || hp > 65535) {
        setActionError("本机隧道端口须留空（自动分配）或为 1024 到 65535 之间的整数");
        return;
      }
    }
    if ((gp !== store.guest_port || hp !== store.host_tunnel_port) && !(await confirmInterruption())) return;
    const next = await runAction(
      "save",
      () => store.saveConfig({
        ...(serverName !== savedName ? { server_name: serverName.trim() } : {}),
        ...(guestPortInput !== savedGuestPort ? { guest_port: gp } : {}),
        ...(hostPortInput !== savedHostPort ? { host_tunnel_port: hp } : {}),
        ...(memLimitInput !== savedMemLimit ? { terminal_memory_max: memLimitInput.trim().toUpperCase() } : {}),
      }),
      "Orca Server 设置已保存",
    );
    if (next) setDraft({});
  };

  const saveBindMode = async (bind_mode) => {
    if (bind_mode === store.bind_mode || !(await confirmInterruption())) return;
    void runAction("bind-mode", () => store.saveConfig({ bind_mode }), "监听模式已保存");
  };

  const changeBinding = (agentId, value) => {
    const next = { ...(store.account_bindings || {}), [agentId]: value };
    void runAction(`bind:${agentId}`, async () => {
      const res = await store.saveConfig({ account_bindings: next });
      if (res && store.running && !magpieTakeover) setBindingRestartHint(true);
      return res;
    }, "账号绑定已保存");
  };

  const doUninstall = async () => {
    const ok = await onConfirm?.({
          title: "卸载客机中的 Orca Server？",
          body: "将停止托管的 Orca Server 并删除程序文件，历史会话、工作区记录与配置数据会安全保留。",
          confirm: "确认卸载",
        });
    if (!ok) return;
    await runAction("uninstall", () => store.uninstall({ purgeData: false, removePaired: false }));
  };

  const doResetIdentity = async () => {
    const ok = await onConfirm?.({
          title: "重置 Orca Server 配对身份？",
          body: "将生成全新的服务端加密身份并作废所有已配对的电脑/手机凭证。重置后需重新一键连接或扫码配对。",
          confirm: "确认重置身份",
        });
    if (!ok) return;
    await runAction("reset-id", () => store.resetIdentity(), "已重置 Orca 配对身份");
  };

  const repairPairing = async () => {
    const ok = await onConfirm?.({
      title: "重新配对本机 Orca？",
      body: "原有的本机 Orca 连接将被替换，新连接会使用当前保存的服务器名称。",
      confirm: "重新配对",
    });
    if (ok) await runAction("repair", () => store.open({ force_repair: true }), "已重新配对本机 Orca 客户端");
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
            disabled={busy || !store.data || (!supportsManaged && !store.enabled)}
            onChange={(e) => void toggleEnabled(e.target.checked)}
          />
        </label>
      </div>

      {(actionError || jobError || store.error || store.serverError) && (
        <div className="error-message" role="alert">{actionError || jobError || store.error || store.serverError}</div>
      )}
      {store.loading && !store.data && <p className="field-help" role="status">正在读取 Orca Server 设置…</p>}
      <button type="button" disabled={busy || store.loading}
        onClick={() => { setActionError(""); void store.refresh(); }}>
        <RefreshCw size={14} />刷新 Orca Server 状态
      </button>

      {!supportsManaged && (
        <div className="inline-info t3code-info-banner">
          <span>当前 Windows 原生客机暂不支持托管 Orca Server（官方服务端仅提供 Linux 与 macOS 运行时）。</span>
        </div>
      )}

      {store.data && supportsManaged && (
        <div className="t3code-card-body">
          <div className="orca-security-strip">
            <ShieldCheck size={14} aria-hidden="true" />
            <span>
              隔离护栏已启用：<code>--refuse-public</code> 禁止公网监听 · OAuth 浏览器强制留在客机防 IP 串流 · 遥测关闭 · 锁屏立即断隧道
            </span>
          </div>

          {store.enabled && <div className="t3code-quick-actions">
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
          </div>}

          {store.enabled && !store.can_auto_pair && !store.local_desktop?.installed && (
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
                onClick={() => void saveBindMode("loopback")}
              >
                仅本机回环隧道（最安全）
              </button>
              <button
                type="button"
                role="radio"
                aria-checked={store.bind_mode === "tailscale"}
                className={`t3code-mode-btn ${store.bind_mode === "tailscale" ? "selected" : ""}`}
                disabled={busy}
                onClick={() => void saveBindMode("tailscale")}
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

          <form className="t3code-config-grid orca-network-form" onSubmit={saveNetworkSettings} noValidate>
            <label>
              <span className="t3code-field-label">配对时使用的服务器名称</span>
              <input
                type="text"
                value={serverName}
                onChange={(e) => edit("name", e.target.value, savedName)}
                placeholder="Super Cage"
                maxLength={64}
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
                onChange={(e) => edit("guestPort", e.target.value, savedGuestPort)}
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
                onChange={(e) => edit("hostPort", e.target.value, savedHostPort)}
                placeholder={store.host_tunnel_port === 0 && store.tunnel_port ? `自动分配（${store.tunnel_port}）` : "自动分配"}
                disabled={busy}
              />
            </label>
            <div className="t3code-port-form">
              <label>
                <span className="t3code-field-label">单终端内存上限（Linux 可选）</span>
                <input
                  type="text"
                  value={memLimitInput}
                  onChange={(e) => edit("memory", e.target.value, savedMemLimit)}
                  placeholder="例如 2G（留空不限）"
                  disabled={busy}
                />
              </label>
              {dirty && <button type="button" disabled={busy} onClick={() => { setDraft({}); setActionError(""); setSavedNotice(""); }}>放弃修改</button>}
              <button type="submit" disabled={busy || !dirty}>
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
                {magpieTakeover
                  ? "当前已启用 Magpie，账号调度与绑定已自动暂停并由 Magpie 统一接管；关闭 Magpie 后自动恢复下方配置。"
                  : "默认「跟随 Cage 自动调度」会在 Orca 的结构化对话、终端与子 Agent 中实时动态调度健康账号（额度耗尽或限流冷却自动避让，无需重启 Orca）。"}
              </small>
              {bindableAgents.some(entry => !entry.runtime?.installed) && (
                <small>可以预先保存绑定，安装对应 CLI 后使用。 {onOpenAgentSettings && <button type="button" className="text-button" onClick={onOpenAgentSettings}>前往 Agent CLI</button>}</small>
              )}
              {Object.keys(accountErrors).length > 0 && <button type="button" disabled={accountsLoading} onClick={retryAccounts}>重试账号列表</button>}
            </div>

            {magpieTakeover && (
              <div className="inline-info t3code-info-banner" role="status">
                <span>Magpie 接管生效中：Orca Server 中的 Claude、Codex 及其他 Agent CLI 均沿用客机原生配置由 Magpie 路由，暂不使用下方 Cage 账号绑定。</span>
              </div>
            )}

            {bindingRestartHint && !magpieTakeover && (
              <div className="inline-info t3code-restart-hint">
                <span>重启 Orca Server 后会使用最新保存的账号绑定。</span>
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
                const currentVal = Object.hasOwn(store.account_bindings, id) ? store.account_bindings[id] : "auto";
                const accounts = accountOptions[id] || [];
                const activeLabel = magpieTakeover ? "Magpie 接管" : store.active_bindings?.[id];
                return (
                  <div key={id} className="t3code-binding-row">
                    <div className="t3code-binding-agent">
                      <AgentIcon agentId={id} accentColor={entry.spec.accent_color} size={18} />
                      <span>{entry.spec.short_name || entry.spec.name}</span>
                      {activeLabel && (
                        <span className={`agent-pill ${magpieTakeover ? "agent-pill--ok" : ""}`}>
                          {activeLabel}
                        </span>
                      )}
                    </div>
                    <InlineSelect
                      aria-label={`${entry.spec.name} 账号绑定`}
                      value={currentVal}
                      disabled={busy}
                      onValueChange={(val) => changeBinding(id, val)}
                    >
                      <option value="auto">跟随 Cage 自动调度（推荐）</option>
                      <option value="default">使用客机默认环境登录</option>
                      {currentVal !== "auto" && currentVal !== "default" && !accounts.some(acc => acc.id === currentVal) && (
                        <option value={currentVal} disabled>已保存的固定账号（当前不可用）</option>
                      )}
                      {accounts.map((acc) => (
                        <option key={acc.id} value={acc.id}>
                          固定账号：{acc.label || acc.email || acc.username || acc.key_hint || acc.id.slice(0, 8)}
                        </option>
                      ))}
                    </InlineSelect>
                    {id === "codex" && currentVal === "auto" && !magpieTakeover &&
                      <CodexSyncStatus bridge={store.data?.codex_bridge} accounts={accounts} />}
                    {accountErrors[id] && <small className="field-help t3code-binding-error" role="status">{accountErrors[id]}</small>}
                  </div>
                );
              })}
            </div>
          </div>

          {store.data?.codex_registration_supported && registrationAccounts.length > 0 && <div className="t3code-bindings">
            <label>登记 Codex 账号到 Orca
              <InlineSelect aria-label="登记 Codex 账号到 Orca" value={registration?.id || ""} onValueChange={setRegistrationID} disabled={busy}>
                <option value="" disabled>选择 Cage 账号</option>
                {registrationAccounts.map(a => <option key={a.id} value={a.id}>{a.label || a.email || a.id}</option>)}
              </InlineSelect>
            </label>
            <button type="button" disabled={busy || !registration || !vmRunning} onClick={() => void registerAccount()}>登记到 Orca</button>
          </div>}
          {store.enabled && store.installed && <details className="orca-advanced">
            <summary>高级维护与重置选项</summary>
            <div className="orca-advanced-body">
              <div className="t3code-quick-actions">
                {store.can_auto_pair && store.installed && (
                  <button
                    type="button"
                    disabled={busy || !vmRunning || !envReady}
                    onClick={() => void repairPairing()}
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
          </details>}
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
