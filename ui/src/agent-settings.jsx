import React, { useEffect, useMemo, useState } from "react";
import {
  Check,
  Download,
  LoaderCircle,
  Pencil,
  Plus,
  RefreshCw,
  SlidersHorizontal,
  TerminalSquare,
  Trash2,
  UsersRound,
} from "lucide-react";
import { AgentIcon } from "./agent-icon";
import { InlineSelect } from "./inline-select";
import { T3CodeSettingsCard } from "./t3code-panel";
import { MagpieSettingsCard } from "./magpie-panel";
import { HerdrSettingsCard } from "./herdr-panel";
import { OrcaSettingsCard } from "./orca-panel";
import "./agent-settings.css";

const CATEGORY_LABELS = {
  all: "全部",
  "first-party": "官方原生",
  "multi-provider": "多模型",
  "open-source": "开源生态",
  custom: "自定义",
};

const ISOLATION_LABELS = {
  claude_native: "原生隔离配置目录",
  env_dir: "独立配置目录隔离",
  env_home: "独立 Home 目录隔离",
  xdg: "XDG 目录隔离",
  flag_dir: "命令行配置目录隔离",
  api_key_only: "隔离凭证密钥",
};

const AUTH_LABELS = {
  oauth: "OAuth 浏览器登录",
  device_code: "设备码登录",
  api_key: "API Key / 网关",
};

function emptyCustomForm() {
  return {
    id: "",
    name: "",
    short_name: "",
    description: "",
    glyph: "",
    accent_color: "#4f46e5",
    binary: "",
    default_launch_command: "",
    channel_kind: "npm",
    channel_package: "",
    channel_script_url: "",
    isolation_mode: "env_dir",
    isolation_env_var: "",
    isolation_config_subdir: "",
    shared_resources: "",
    auth_oauth: true,
    auth_device: false,
    auth_api_key: true,
    oauth_domains: "",
    api_key_env_vars: "",
    base_url_env_var: "",
    model_env_var: "",
    login_args: "login",
    image_paste_shortcut: true,
    shift_enter_mode: "bracketed_newline",
    preset_flags: "",
    editing: false,
  };
}

function formFromSpec(spec) {
  const ch = spec.channels?.[0] || { kind: "existing" };
  return {
    id: spec.id || "",
    name: spec.name || "",
    short_name: spec.short_name || "",
    description: spec.description || "",
    glyph: spec.glyph || "",
    accent_color: spec.accent_color || "#4f46e5",
    binary: spec.binary || "",
    default_launch_command: spec.default_launch_command || spec.binary || "",
    channel_kind: ch.kind || "existing",
    channel_package: ch.package || "",
    channel_script_url: ch.script_url || "",
    isolation_mode: spec.isolation?.mode || "env_dir",
    isolation_env_var: spec.isolation?.env_var || "",
    isolation_config_subdir: spec.isolation?.config_subdir || "",
    shared_resources: (spec.isolation?.shared_resources || []).join(", "),
    auth_oauth: (spec.auth?.methods || []).includes("oauth"),
    auth_device: (spec.auth?.methods || []).includes("device_code"),
    auth_api_key: (spec.auth?.methods || []).includes("api_key"),
    oauth_domains: (spec.auth?.oauth_domains || []).join(", "),
    api_key_env_vars: (spec.auth?.api_key_env_vars || []).join(", "),
    base_url_env_var: spec.auth?.base_url_env_var || "",
    model_env_var: spec.auth?.model_env_var || "",
    login_args: (spec.auth?.login_args || []).join(" "),
    image_paste_shortcut: spec.terminal?.image_paste_shortcut !== false,
    shift_enter_mode: spec.terminal?.shift_enter_mode || "bracketed_newline",
    preset_flags: (spec.terminal?.preset_flags || []).join(", "),
    editing: true,
  };
}

function splitCSV(text) {
  return String(text || "")
    .split(/[,，\n]/)
    .map((s) => s.trim())
    .filter(Boolean);
}

function AgentConfigEditor({ entry, busy, onSave }) {
  const { spec, config = {} } = entry;
  const [launchCommand, setLaunchCommand] = useState(config.launch_command || spec.default_launch_command || spec.binary);
  const [extraArgs, setExtraArgs] = useState(config.extra_args || "");
  const [imagePaste, setImagePaste] = useState(
    typeof config.image_paste === "boolean" ? config.image_paste : spec.terminal?.image_paste_shortcut !== false,
  );
  const [shiftEnterMode, setShiftEnterMode] = useState(
    config.shift_enter_mode || spec.terminal?.shift_enter_mode || "bracketed_newline",
  );
  const [hiddenInMenu, setHiddenInMenu] = useState(Boolean(config.hidden_in_menu));
  const [saving, setSaving] = useState(false);
  const [savedMsg, setSavedMsg] = useState("");
  const [err, setErr] = useState("");

  useEffect(() => {
    setLaunchCommand(config.launch_command || spec.default_launch_command || spec.binary);
    setExtraArgs(config.extra_args || "");
    setImagePaste(
      typeof config.image_paste === "boolean" ? config.image_paste : spec.terminal?.image_paste_shortcut !== false,
    );
    setShiftEnterMode(config.shift_enter_mode || spec.terminal?.shift_enter_mode || "bracketed_newline");
    setHiddenInMenu(Boolean(config.hidden_in_menu));
  }, [
    spec.id,
    spec.default_launch_command,
    spec.binary,
    spec.terminal?.image_paste_shortcut,
    spec.terminal?.shift_enter_mode,
    config.launch_command,
    config.extra_args,
    config.image_paste,
    config.shift_enter_mode,
    config.hidden_in_menu,
  ]);

  const togglePreset = (flag) => {
    const parts = extraArgs.trim() ? extraArgs.trim().split(/\s+/) : [];
    const tokens = flag.trim().split(/\s+/);
    const has = extraArgs.includes(flag);
    if (has) {
      setExtraArgs(extraArgs.replace(flag, "").replace(/\s{2,}/g, " ").trim());
    } else {
      setExtraArgs([...parts, ...tokens].join(" "));
    }
    setSavedMsg("");
  };

  const submit = async (e) => {
    e.preventDefault();
    setSaving(true);
    setErr("");
    setSavedMsg("");
    try {
      await onSave({
        agent_id: spec.id,
        launch_command: launchCommand.trim(),
        extra_args: extraArgs.trim(),
        image_paste: imagePaste,
        shift_enter_mode: shiftEnterMode,
        hidden_in_menu: hiddenInMenu,
      });
      setSavedMsg("已保存，将应用于新建终端");
    } catch (error) {
      setErr(error.message);
    } finally {
      setSaving(false);
    }
  };

  return (
    <form className="agent-config-panel" onSubmit={submit}>
      <div className="agent-config-grid">
        <label>
          启动命令
          <input
            value={launchCommand}
            onChange={(e) => { setLaunchCommand(e.target.value); setSavedMsg(""); }}
            placeholder={spec.default_launch_command || spec.binary}
            spellCheck="false"
            autoComplete="off"
          />
        </label>
        {spec.id !== "claude" && (
          <label>
            附加默认参数
            <input
              value={extraArgs}
              onChange={(e) => { setExtraArgs(e.target.value); setSavedMsg(""); }}
              placeholder="例如 --model o3"
              spellCheck="false"
              autoComplete="off"
            />
          </label>
        )}
        <label>
          Shift+Enter 换行行为
          <InlineSelect
            aria-label="Shift+Enter 换行行为"
            value={shiftEnterMode}
            onValueChange={(value) => { setShiftEnterMode(value); setSavedMsg(""); }}
          >
            <option value="bracketed_newline">智能换行（括号粘贴 / CSI-u）</option>
            <option value="csi_u">始终发送 CSI-u Shift+Enter</option>
            <option value="plain">普通换行符</option>
          </InlineSelect>
        </label>
      </div>

      {spec.id !== "claude" && spec.terminal?.preset_flags?.length > 0 && (
        <div className="agent-presets">
          <span>常用启动参数：</span>
          {spec.terminal.preset_flags.map((flag) => (
            <button
              type="button"
              key={flag}
              className="agent-preset-chip"
              data-active={extraArgs.includes(flag)}
              onClick={() => togglePreset(flag)}
            >
              {flag}
            </button>
          ))}
        </div>
      )}

      <label className="toggle-row">
        <span>
          <strong>启用 Ctrl+V 图片粘贴到终端</strong>
          <small>允许在终端内直接粘贴截图或图片文件，自动上传并插入客机路径。</small>
        </span>
        <input
          type="checkbox"
          role="switch"
          checked={imagePaste}
          onChange={(e) => { setImagePaste(e.target.checked); setSavedMsg(""); }}
        />
      </label>

      {spec.id !== "claude" && (
        <label className="toggle-row">
          <span>
            <strong>在“新建终端”菜单中隐藏</strong>
            <small>保留已安装程序和账号配置，但不在新建终端快捷菜单中显示。</small>
          </span>
          <input
            type="checkbox"
            role="switch"
            checked={hiddenInMenu}
            onChange={(e) => { setHiddenInMenu(e.target.checked); setSavedMsg(""); }}
          />
        </label>
      )}

      {err && <div className="error-message" role="alert">{err}</div>}
      <div className="agent-custom-actions">
        {savedMsg && <span className="agent-pill agent-pill--ok"><Check size={13} />{savedMsg}</span>}
        <button type="submit" className="primary" disabled={busy || saving}>
          {saving ? <LoaderCircle size={14} className="spin" /> : <Check size={14} />}
          保存终端设置
        </button>
      </div>
    </form>
  );
}

export function AgentSettings({
  store,
  t3code,
  magpie,
  herdr,
  orca,
  status,
  onConfirm,
  onOpenAccounts,
  onLaunchAgent,
  onOpenT3Code,
  onOpenMagpie,
  onOpenMagpieTUI,
  onOpenHerdr,
  onOpenOrca,
  onShowOrcaPair,
  onSavedClaudeCommand,
}) {
  const [query, setQuery] = useState("");
  const [category, setCategory] = useState("all");
  const [selectedChannels, setSelectedChannels] = useState({});
  const [expandedConfig, setExpandedConfig] = useState({});
  const [showCustomForm, setShowCustomForm] = useState(false);
  const [customForm, setCustomForm] = useState(emptyCustomForm);
  const [actionError, setActionError] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const vmRunning = Boolean(status?.vm_running);
  const envReady = Boolean(status?.ready);
  const jobRunning = Boolean(status?.job?.running);
  const isAgentJob = jobRunning && (status?.job?.kind === "agent-install" || status?.job?.kind === "agent-uninstall");
  const busy = jobRunning || submitting;

  const installed = useMemo(
    () => store.agents.filter((entry) => entry.spec.id === "claude" || entry.runtime?.installed),
    [store.agents],
  );

  const catalog = useMemo(() => {
    const q = query.trim().toLowerCase();
    return store.agents.filter((entry) => {
      if (category !== "all" && entry.spec.category !== category) return false;
      if (!q) return true;
      const hay = `${entry.spec.id} ${entry.spec.name} ${entry.spec.short_name} ${entry.spec.description} ${entry.spec.binary}`.toLowerCase();
      return hay.includes(q);
    });
  }, [store.agents, query, category]);

  const channelFor = (entry) => {
    const chosen = selectedChannels[entry.spec.id] || entry.config?.preferred_channel;
    const channels = entry.spec.channels || [];
    if (chosen && channels.some((c) => c.id === chosen)) return chosen;
    return channels[0]?.id || "";
  };

  const startInstall = async (entry) => {
    setActionError("");
    const channel = channelFor(entry);
    setSubmitting(true);
    try {
      await store.install(entry.spec.id, channel);
    } catch (e) {
      setActionError(e.message);
    } finally {
      setSubmitting(false);
    }
  };

  const startUninstall = async (entry) => {
    if (!await onConfirm({
      title: `卸载 ${entry.spec.name}？`,
      body: "将卸载环境中的 CLI 程序和 Cage 垫片。已登记的账号凭据和项目文件会被安全保留。",
      confirm: "确认卸载",
    })) return;
    setActionError("");
    setSubmitting(true);
    try {
      await store.uninstall(entry.spec.id, channelFor(entry));
    } catch (e) {
      setActionError(e.message);
    } finally {
      setSubmitting(false);
    }
  };

  const saveConfig = async (patch) => {
    await store.saveConfig(patch);
    if (patch.agent_id === "claude") {
      onSavedClaudeCommand?.();
    }
  };

  const editCustom = (spec) => {
    setCustomForm(formFromSpec(spec));
    setShowCustomForm(true);
  };

  const submitCustom = async (e) => {
    e.preventDefault();
    setActionError("");
    const methods = [];
    if (customForm.auth_oauth) methods.push("oauth");
    if (customForm.auth_device) methods.push("device_code");
    if (customForm.auth_api_key) methods.push("api_key");
    if (!methods.length) {
      setActionError("请至少选择一种认证方式（OAuth / 设备码 / API Key）。");
      return;
    }

    const channel = {
      id: `${customForm.channel_kind}-custom`,
      label: customForm.channel_kind === "npm" ? "npm 安装"
        : customForm.channel_kind === "uv" ? "uv / Python 安装"
          : customForm.channel_kind === "pip" ? "pip 安装"
            : customForm.channel_kind === "script" ? "官方安装脚本"
              : "检测已有命令",
      kind: customForm.channel_kind,
      package: ["npm", "uv", "pip"].includes(customForm.channel_kind) ? customForm.channel_package.trim() : "",
      script_url: customForm.channel_kind === "script" ? customForm.channel_script_url.trim() : "",
    };

    const spec = {
      id: customForm.id.trim().toLowerCase(),
      name: customForm.name.trim(),
      short_name: customForm.short_name.trim() || customForm.name.trim(),
      description: customForm.description.trim() || "自定义 Agent CLI 适配",
      category: "custom",
      glyph: customForm.glyph.trim().toUpperCase(),
      accent_color: customForm.accent_color || "#4f46e5",
      built_in: false,
      binary: customForm.binary.trim(),
      default_launch_command: customForm.default_launch_command.trim() || customForm.binary.trim(),
      version_args: ["--version"],
      channels: [channel],
      isolation: {
        mode: customForm.isolation_mode,
        env_var: ["env_dir", "env_home"].includes(customForm.isolation_mode) ? customForm.isolation_env_var.trim() : "",
        config_subdir: customForm.isolation_mode === "env_home" ? customForm.isolation_config_subdir.trim() : "",
        shared_resources: splitCSV(customForm.shared_resources),
      },
      auth: {
        methods,
        login_args: customForm.login_args.trim() ? customForm.login_args.trim().split(/\s+/) : [],
        oauth_domains: splitCSV(customForm.oauth_domains),
        api_key_env_vars: splitCSV(customForm.api_key_env_vars),
        base_url_env_var: customForm.base_url_env_var.trim(),
        model_env_var: customForm.model_env_var.trim(),
        strip_env_on_oauth: splitCSV(customForm.api_key_env_vars),
      },
      terminal: {
        image_paste_shortcut: Boolean(customForm.image_paste_shortcut),
        shift_enter_mode: customForm.shift_enter_mode,
        preset_flags: splitCSV(customForm.preset_flags),
      },
    };

    setSubmitting(true);
    try {
      await store.saveCustom(spec);
      setShowCustomForm(false);
      setCustomForm(emptyCustomForm());
    } catch (error) {
      setActionError(error.message);
    } finally {
      setSubmitting(false);
    }
  };

  const removeCustom = async (entry) => {
    if (!await onConfirm({
      title: `删除自定义适配「${entry.spec.name}」？`,
      body: "将从 Agent 目录中移除该自定义适配定义。客机内已安装的程序和账号目录不会被删除。",
      confirm: "确认删除",
    })) return;
    setActionError("");
    setSubmitting(true);
    try {
      await store.deleteCustom(entry.spec.id);
    } catch (error) {
      setActionError(error.message);
    } finally {
      setSubmitting(false);
    }
  };

  const prereqOrder = [
    ["node", "Node.js"],
    ["npm", "npm"],
    ["python3", "Python"],
    ["uv", "uv"],
    ["git", "Git"],
    ["curl", "curl"],
  ];

  return (
    <div className="agent-settings">
      <div className="agent-settings-header">
        <div className="section-title">
          <h3>Agent CLI 与官方渠道</h3>
          <p>
            通过各家官方安装渠道一键安装主流 Agent CLI，或适配自定义 CLI。安装完成后即刻获得专属终端优化与智能多账号调度管理。
          </p>
        </div>
        <button
          type="button"
          disabled={store.loading || !vmRunning}
          onClick={() => store.refresh(true)}
        >
          <RefreshCw size={15} className={store.loading ? "spin" : undefined} />
          重新检测
        </button>
      </div>

      {!vmRunning && (
        <div className="inline-info">
          <span>当前 Cage 环境尚未启动。启动环境后可一键安装、更新并实时探测客机中的 Agent CLI。</span>
        </div>
      )}

      {isAgentJob && (
        <div className="agent-job-banner" role="status">
          <LoaderCircle size={16} className="spin" />
          <span>{status.job.message || "正在执行 Agent CLI 安装或卸载任务…"}</span>
        </div>
      )}

      {(actionError || store.error || store.probeError) && (
        <div className="error-message" role="alert">
          {actionError || store.error || store.probeError}
        </div>
      )}

      {vmRunning && Object.keys(store.prerequisites || {}).length > 0 && (
        <div className="agent-prereq-strip" aria-label="环境基础运行时">
          <span className="agent-prereq-label">客机运行时：</span>
          {prereqOrder.map(([key, label]) => {
            const state = store.prerequisites[key];
            if (!state) return null;
            return (
              <span key={key} className="agent-prereq-chip" data-ok={state.available}>
                {label}
                {state.available && state.version ? ` ${state.version}` : state.available ? " 就绪" : " 未安装"}
              </span>
            );
          })}
        </div>
      )}

      {t3code && (
        <T3CodeSettingsCard
          store={t3code}
          agentsStore={store}
          status={status}
          onOpenT3Code={onOpenT3Code}
        />
      )}

      {magpie && (
        <MagpieSettingsCard
          store={magpie}
          status={status}
          onConfirm={onConfirm}
          onOpenMagpie={onOpenMagpie}
          onOpenMagpieTUI={onOpenMagpieTUI}
        />
      )}

      {herdr && (
        <HerdrSettingsCard
          store={herdr}
          status={status}
          onConfirm={onConfirm}
          onOpenHerdr={onOpenHerdr}
        />
      )}

      {orca && (
        <OrcaSettingsCard
          store={orca}
          agentsStore={store}
          status={status}
          onConfirm={onConfirm}
          onOpenOrca={onOpenOrca}
          onShowPairModal={onShowOrcaPair}
        />
      )}

      <div className="agent-section-heading">
        <h4>已安装与就绪 ({installed.length})</h4>
        <small>每个已安装的 Agent 均已自动接入专属终端与独立账号调度池</small>
      </div>

      <div className="agent-installed-list">
        {installed.map((entry) => {
          const { spec, runtime, accounts } = entry;
          const openCfg = Boolean(expandedConfig[spec.id]);
          const channels = spec.channels || [];
          const isCustom = !spec.built_in;
          return (
            <article key={spec.id} className="agent-card">
              <div className="agent-card-top">
                <AgentIcon agentId={spec.id} glyph={spec.glyph} accentColor={spec.accent_color} size={34} />
                <div className="agent-card-info">
                  <div className="agent-card-title">
                    <strong>{spec.name}</strong>
                    {runtime?.version && <span className="agent-pill">v{runtime.version.replace(/^v/i, "")}</span>}
                    {runtime?.installed ? (
                      <span className="agent-pill agent-pill--ok">已安装</span>
                    ) : spec.id === "claude" ? (
                      <span className="agent-pill">内置核心</span>
                    ) : null}
                    {isCustom && <span className="agent-pill">自定义</span>}
                    {accounts?.total > 0 && (
                      <span className="agent-pill">
                        {accounts.ready}/{accounts.total} 账号就绪
                        {accounts.cooling_down > 0 ? ` · ${accounts.cooling_down} 冷却中` : ""}
                      </span>
                    )}
                    {accounts?.active > 0 && (
                      <span className="agent-pill agent-pill--ok">{accounts.active} 终端运行中</span>
                    )}
                  </div>
                  <p className="agent-card-desc">{spec.description}</p>
                  <div className="agent-card-meta">
                    <span>隔离：{ISOLATION_LABELS[spec.isolation?.mode] || spec.isolation?.mode}</span>
                    {runtime?.binary_path && <span>路径：<code>{runtime.binary_path}</code></span>}
                    <span>认证：{(spec.auth?.methods || []).map((m) => AUTH_LABELS[m] || m).join(" / ")}</span>
                  </div>
                </div>
              </div>

              <div className="agent-card-actions">
                <button
                  type="button"
                  className="primary"
                  disabled={!vmRunning || busy}
                  onClick={() => onLaunchAgent?.(spec.id)}
                >
                  <TerminalSquare size={14} />
                  打开终端
                </button>
                <button
                  type="button"
                  onClick={() => onOpenAccounts?.(spec.id)}
                >
                  <UsersRound size={14} />
                  账号管理
                </button>
                <button
                  type="button"
                  aria-expanded={openCfg}
                  onClick={() => setExpandedConfig((prev) => ({ ...prev, [spec.id]: !prev[spec.id] }))}
                >
                  <SlidersHorizontal size={14} />
                  {openCfg ? "收起终端设置" : "终端优化与启动设置"}
                </button>
                {spec.id !== "claude" && channels.length > 0 && channels[0].kind !== "existing" && (
                  <button
                    type="button"
                    disabled={!envReady || busy}
                    onClick={() => startInstall(entry)}
                  >
                    <RefreshCw size={14} />
                    更新到新版
                  </button>
                )}
                {spec.id !== "claude" && runtime?.installed && (
                  <button
                    type="button"
                    disabled={!envReady || busy}
                    onClick={() => startUninstall(entry)}
                  >
                    <Trash2 size={14} />
                    卸载
                  </button>
                )}
                {isCustom && (
                  <>
                    <button type="button" onClick={() => editCustom(spec)}>
                      <Pencil size={14} />
                      编辑适配
                    </button>
                    <button
                      type="button"
                      disabled={busy}
                      onClick={() => removeCustom(entry)}
                    >
                      删除适配
                    </button>
                  </>
                )}
              </div>

              {openCfg && (
                <AgentConfigEditor
                  entry={entry}
                  busy={busy}
                  onSave={saveConfig}
                />
              )}
            </article>
          );
        })}
      </div>

      <div className="agent-section-heading">
        <h4>官方渠道目录与一键安装</h4>
        <button
          type="button"
          onClick={() => {
            if (showCustomForm && customForm.editing) {
              setCustomForm(emptyCustomForm());
            } else {
              setShowCustomForm((v) => !v);
              setCustomForm(emptyCustomForm());
            }
          }}
        >
          <Plus size={14} />
          {showCustomForm ? "收起自定义适配" : "适配自定义 Agent CLI"}
        </button>
      </div>

      {showCustomForm && (
        <form className="agent-custom-form" onSubmit={submitCustom}>
          <h5>{customForm.editing ? `编辑自定义 Agent：${customForm.id}` : "适配自定义 Agent CLI"}</h5>
          <p className="field-help" style={{ margin: 0 }}>
            任何符合标准终端交互与环境变量隔离的 Agent CLI 均可零代码接入 Cage 的官方渠道安装、独立隔离目录、OAuth 隐私登录与多账号智能调度。
          </p>
          <div className="agent-custom-form-grid">
            <label>
              唯一标识 ID（小写字母/数字/短横线）
              <input
                required
                disabled={customForm.editing}
                value={customForm.id}
                onChange={(e) => setCustomForm((p) => ({ ...p, id: e.target.value }))}
                placeholder="例如 my-agent"
                spellCheck="false"
              />
            </label>
            <label>
              显示名称
              <input
                required
                value={customForm.name}
                onChange={(e) => setCustomForm((p) => ({ ...p, name: e.target.value }))}
                placeholder="例如 My Agent CLI"
              />
            </label>
            <label>
              简称（菜单显示）
              <input
                value={customForm.short_name}
                onChange={(e) => setCustomForm((p) => ({ ...p, short_name: e.target.value }))}
                placeholder="例如 MyAgent"
              />
            </label>
            <label>
              可执行命令名 (binary)
              <input
                required
                value={customForm.binary}
                onChange={(e) => setCustomForm((p) => ({ ...p, binary: e.target.value }))}
                placeholder="例如 my-agent"
                spellCheck="false"
              />
            </label>
            <label>
              默认启动命令
              <input
                value={customForm.default_launch_command}
                onChange={(e) => setCustomForm((p) => ({ ...p, default_launch_command: e.target.value }))}
                placeholder="留空默认与 binary 一致"
                spellCheck="false"
              />
            </label>
            <label>
              图标配色（#RRGGBB）
              <span style={{ display: "inline-flex", alignItems: "center", gap: 8 }}>
                <AgentIcon
                  agentId={customForm.id.trim().toLowerCase() || "custom-preview"}
                  accentColor={/^#[0-9a-fA-F]{6}$/.test(customForm.accent_color) ? customForm.accent_color : "#4f46e5"}
                  size={22}
                />
                <input
                  maxLength={7}
                  value={customForm.accent_color}
                  onChange={(e) => setCustomForm((p) => ({ ...p, accent_color: e.target.value }))}
                  placeholder="#4f46e5"
                  spellCheck="false"
                />
              </span>
            </label>
            <label>
              安装渠道类型
              <InlineSelect
                aria-label="安装渠道类型"
                value={customForm.channel_kind}
                onValueChange={(v) => setCustomForm((p) => ({ ...p, channel_kind: v }))}
              >
                <option value="npm">npm 官方包</option>
                <option value="uv">uv / Python 工具包</option>
                <option value="pip">pip 工具包</option>
                <option value="script">官方 HTTPS 安装脚本</option>
                <option value="existing">仅检测环境中已有命令</option>
              </InlineSelect>
            </label>
            {["npm", "uv", "pip"].includes(customForm.channel_kind) && (
              <label>
                包名
                <input
                  required
                  value={customForm.channel_package}
                  onChange={(e) => setCustomForm((p) => ({ ...p, channel_package: e.target.value }))}
                  placeholder={customForm.channel_kind === "npm" ? "@org/agent-cli" : "agent-cli"}
                  spellCheck="false"
                />
              </label>
            )}
            {customForm.channel_kind === "script" && (
              <label>
                官方安装脚本 URL（仅 HTTPS）
                <input
                  required
                  type="url"
                  value={customForm.channel_script_url}
                  onChange={(e) => setCustomForm((p) => ({ ...p, channel_script_url: e.target.value }))}
                  placeholder="https://example.com/install.sh"
                  spellCheck="false"
                />
              </label>
            )}
            <label>
              多账号隔离模式
              <InlineSelect
                aria-label="多账号隔离模式"
                value={customForm.isolation_mode}
                onValueChange={(v) => setCustomForm((p) => ({ ...p, isolation_mode: v }))}
              >
                <option value="env_dir">独立配置目录环境变量（如 CODEX_HOME）</option>
                <option value="env_home">独立 Home 环境变量 + 子目录（如 GEMINI_CLI_HOME）</option>
                <option value="xdg">XDG 配置/数据目录隔离</option>
                <option value="api_key_only">纯 API Key 环境变量隔离</option>
              </InlineSelect>
            </label>
            {["env_dir", "env_home"].includes(customForm.isolation_mode) && (
              <label>
                隔离目录环境变量名
                <input
                  required
                  value={customForm.isolation_env_var}
                  onChange={(e) => setCustomForm((p) => ({ ...p, isolation_env_var: e.target.value }))}
                  placeholder="例如 MY_AGENT_HOME"
                  spellCheck="false"
                />
              </label>
            )}
            {customForm.isolation_mode === "env_home" && (
              <label>
                配置子目录名
                <input
                  required
                  value={customForm.isolation_config_subdir}
                  onChange={(e) => setCustomForm((p) => ({ ...p, isolation_config_subdir: e.target.value }))}
                  placeholder="例如 .my-agent"
                  spellCheck="false"
                />
              </label>
            )}
            <label>
              API Key 环境变量（逗号分隔）
              <input
                value={customForm.api_key_env_vars}
                onChange={(e) => setCustomForm((p) => ({ ...p, api_key_env_vars: e.target.value }))}
                placeholder="例如 MY_API_KEY, OPENAI_API_KEY"
                spellCheck="false"
              />
            </label>
            <label>
              自定义网关 Base URL 环境变量
              <input
                value={customForm.base_url_env_var}
                onChange={(e) => setCustomForm((p) => ({ ...p, base_url_env_var: e.target.value }))}
                placeholder="例如 OPENAI_BASE_URL"
                spellCheck="false"
              />
            </label>
            <label>
              OAuth 授权域名白名单（逗号分隔）
              <input
                value={customForm.oauth_domains}
                onChange={(e) => setCustomForm((p) => ({ ...p, oauth_domains: e.target.value }))}
                placeholder="例如 auth.example.com"
                spellCheck="false"
              />
            </label>
            <label>
              登录子命令参数
              <input
                value={customForm.login_args}
                onChange={(e) => setCustomForm((p) => ({ ...p, login_args: e.target.value }))}
                placeholder="例如 login 或 auth login"
                spellCheck="false"
              />
            </label>
            <label>
              跨账号共享的非敏感文件（逗号分隔）
              <input
                value={customForm.shared_resources}
                onChange={(e) => setCustomForm((p) => ({ ...p, shared_resources: e.target.value }))}
                placeholder="例如 AGENTS.md, skills"
                spellCheck="false"
              />
            </label>
            <label>
              简介
              <input
                value={customForm.description}
                onChange={(e) => setCustomForm((p) => ({ ...p, description: e.target.value }))}
                placeholder="简短描述此 Agent CLI 的用途"
              />
            </label>
          </div>

          <div className="agent-auth-checks">
            <span style={{ fontSize: "0.85rem", fontWeight: 600 }}>支持认证方式：</span>
            <label className="check-line">
              <input
                type="checkbox"
                checked={customForm.auth_oauth}
                onChange={(e) => setCustomForm((p) => ({ ...p, auth_oauth: e.target.checked }))}
              />
              OAuth 浏览器登录
            </label>
            <label className="check-line">
              <input
                type="checkbox"
                checked={customForm.auth_device}
                onChange={(e) => setCustomForm((p) => ({ ...p, auth_device: e.target.checked }))}
              />
              设备码登录
            </label>
            <label className="check-line">
              <input
                type="checkbox"
                checked={customForm.auth_api_key}
                onChange={(e) => setCustomForm((p) => ({ ...p, auth_api_key: e.target.checked }))}
              />
              API Key / 自定义网关
            </label>
            <label className="check-line">
              <input
                type="checkbox"
                checked={customForm.image_paste_shortcut}
                onChange={(e) => setCustomForm((p) => ({ ...p, image_paste_shortcut: e.target.checked }))}
              />
              Ctrl+V 图片粘贴
            </label>
          </div>

          <div className="agent-custom-actions">
            <button type="button" onClick={() => { setShowCustomForm(false); setCustomForm(emptyCustomForm()); }}>
              取消
            </button>
            <button type="submit" className="primary" disabled={busy}>
              {submitting ? <LoaderCircle size={14} className="spin" /> : <Check size={14} />}
              {customForm.editing ? "保存自定义适配" : "添加到 Agent 目录"}
            </button>
          </div>
        </form>
      )}

      <div className="agent-catalog-toolbar">
        <input
          className="agent-catalog-search"
          type="search"
          aria-label="搜索 Agent CLI"
          placeholder="搜索 Codex、Antigravity、OpenCode、Copilot、Qwen、Aider…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
        <div className="agent-filter-pills" role="tablist" aria-label="Agent 分类筛选">
          {Object.entries(CATEGORY_LABELS).map(([id, label]) => (
            <button
              type="button"
              key={id}
              role="tab"
              aria-selected={category === id}
              className={category === id ? "selected" : ""}
              onClick={() => setCategory(id)}
            >
              {label}
            </button>
          ))}
        </div>
      </div>

      <div className="agent-catalog-grid">
        {catalog.map((entry) => {
          const { spec, runtime } = entry;
          const channels = spec.channels || [];
          const activeChannel = channelFor(entry);
          const isInstalled = spec.id === "claude" || Boolean(runtime?.installed);
          const isCustom = !spec.built_in;
          return (
            <article key={spec.id} className="agent-card">
              <div className="agent-card-top">
                <AgentIcon agentId={spec.id} glyph={spec.glyph} accentColor={spec.accent_color} size={32} />
                <div className="agent-card-info">
                  <div className="agent-card-title">
                    <strong>{spec.name}</strong>
                    {isInstalled && <span className="agent-pill agent-pill--ok">已安装</span>}
                    {isCustom && <span className="agent-pill">自定义</span>}
                    {runtime?.version && <span className="agent-pill">v{runtime.version.replace(/^v/i, "")}</span>}
                  </div>
                  <p className="agent-card-desc">{spec.description}</p>
                  <div className="agent-card-meta">
                    {(spec.auth?.methods || []).map((m) => (
                      <span key={m}>{AUTH_LABELS[m] || m}</span>
                    ))}
                  </div>
                </div>
              </div>

              <div className="agent-card-actions">
                {spec.id !== "claude" && channels.length > 1 && (
                  <div className="agent-channel-select">
                    <InlineSelect
                      aria-label={`${spec.name} 安装渠道`}
                      value={activeChannel}
                      onValueChange={(value) => setSelectedChannels((prev) => ({ ...prev, [spec.id]: value }))}
                    >
                      {channels.map((ch) => (
                        <option key={ch.id} value={ch.id}>
                          {ch.label}
                        </option>
                      ))}
                    </InlineSelect>
                  </div>
                )}
                {spec.id === "claude" ? (
                  <button type="button" onClick={() => onOpenAccounts?.("claude")}>
                    <UsersRound size={14} />
                    管理账号
                  </button>
                ) : (
                  <button
                    type="button"
                    className={isInstalled ? "" : "primary"}
                    disabled={!envReady || busy || !activeChannel}
                    onClick={() => startInstall(entry)}
                  >
                    {isInstalled ? <RefreshCw size={14} /> : <Download size={14} />}
                    {isInstalled ? "更新 / 重装" : "一键官方安装"}
                  </button>
                )}
                {isInstalled && spec.id !== "claude" && (
                  <button type="button" onClick={() => onOpenAccounts?.(spec.id)}>
                    <UsersRound size={14} />
                    账号管理
                  </button>
                )}
                {isCustom && (
                  <>
                    <button type="button" onClick={() => editCustom(spec)}>
                      <Pencil size={14} />
                      编辑适配
                    </button>
                    <button
                      type="button"
                      disabled={busy}
                      onClick={() => removeCustom(entry)}
                    >
                      <Trash2 size={14} />
                      删除适配
                    </button>
                  </>
                )}
              </div>
            </article>
          );
        })}
      </div>
    </div>
  );
}
