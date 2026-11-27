import React, { Suspense, lazy, useEffect, useState } from "react";
import { ChevronRight, FolderOpen, LoaderCircle, SquareTerminal } from "lucide-react";
import { ProviderMark } from "../provider-mark";
import { Doodle } from "./doodles";
import { InlineSelect } from "./inline-select";
import { terminals } from "./services.mjs";
import { accessDenied } from "./transport.mjs";
import { projectFolder } from "./home";
import { haptic } from "./device-navigation.mjs";

const TerminalScreen = lazy(() => import("./terminal-screen"));
export const kindInfo = {
  shell: { label: "Shell", name: "终端", icon: SquareTerminal, hint: "Linux 命令行" },
  claude: { label: "Claude", name: "Claude 终端", icon: ClaudeMark, hint: "Claude Code 界面" },
  codex: { label: "Codex", name: "Codex 终端", icon: CodexMark, hint: "Codex 界面" },
};

function ClaudeMark({ size }) {
  return <ProviderMark provider="claude" size={size} decorative />;
}

function CodexMark({ size }) {
  return <ProviderMark provider="codex" size={size} decorative />;
}

export function TerminalsView({ catalog, project, onProject, terminalID, onOpen, onBack, onDenied, visible }) {
  const [data, setData] = useState(null);
  const [error, setError] = useState("");
  const [creating, setCreating] = useState("");
  const [tick, setTick] = useState(0);
  useEffect(() => {
    if (!visible || terminalID) return;
    const controller = new AbortController();
    let timer;
    const load = async () => {
      try { const next = await terminals.list({ signal: controller.signal }); if (!controller.signal.aborted) { setData(next); setError(""); } }
      catch (err) {
        if (controller.signal.aborted) return;
        if (accessDenied(err)) { onDenied?.(err); return; }
        setError(err.status === 404 ? "电脑上的 Super Cage 版本较旧，暂不支持手机终端。请先升级电脑端。" : err.message);
      }
      timer = setTimeout(load, 5000);
    };
    void load();
    return () => { clearTimeout(timer); controller.abort(); };
  }, [visible, terminalID, tick, onDenied]);
  const current = data?.terminals?.find((t) => t.id === terminalID);
  if (terminalID) return <Suspense fallback={<p className="note center"><LoaderCircle size={16} className="spin" /> 正在打开终端…</p>}>
    <TerminalScreen key={terminalID} id={terminalID} title={current?.title} kind={current?.kind} visible={visible} onBack={onBack} onDenied={onDenied} />
  </Suspense>;
  const create = async (kind) => {
    setCreating(kind); setError("");
    try { const t = await terminals.create(kind, projectFolder(project)); haptic("success"); onOpen(t.id); }
    catch (err) { if (accessDenied(err)) onDenied?.(err); else setError(err.message); }
    finally { setCreating(""); setTick((n) => n + 1); }
  };
  const kinds = data?.kinds || ["shell"];
  const list = (data?.terminals || []).filter((t) => !t.closed).concat((data?.terminals || []).filter((t) => t.closed));
  const projects = catalog?.projects || [];
  return <section className="terminals page-scroll" aria-label="终端">
    <p className="note page-lede">在手机上直接操作环境里的终端，和电脑共享同一个会话。</p>
    {error && <div className="notice" role="alert">{error}</div>}
    <div className="card new-terminal">
      {projects.length > 0 && <InlineSelect label="工作目录" icon={<FolderOpen size={15} />} value={project?.path || ""} onChange={onProject}
        options={projects.map((p) => ({ value: p.path, label: "在 " + p.name + " 中打开", optionLabel: p.name + " · " + p.path }))} />}
      <div className="kind-grid">{kinds.filter((k) => kindInfo[k]).map((kind) => {
        const { label, icon: Icon, hint } = kindInfo[kind];
        return <button key={kind} type="button" disabled={!!creating || !catalog?.ready} onClick={() => create(kind)}>
          {creating === kind ? <LoaderCircle size={20} className="spin" /> : <Icon size={20} />}<strong>{label}</strong><small>{hint}</small>
        </button>;
      })}</div>
      {!catalog?.ready && <p className="note">环境启动后才能新建终端。</p>}
    </div>
    {data && !list.length && <div className="empty-art"><Doodle name="terminal" className="empty-doodle" /><p>还没有打开的终端</p></div>}
    {list.length > 0 && <ul className="terminal-list">{list.map((t) => {
      const Icon = kindInfo[t.kind]?.icon || SquareTerminal;
      return <li key={t.id}><button type="button" onClick={() => onOpen(t.id)}>
        <Icon size={18} /><span><strong>{t.title}</strong><small>{t.closed ? "已结束" + (t.exit_code != null ? "（退出码 " + t.exit_code + "）" : "") : (kindInfo[t.kind]?.name || "终端") + (t.workspace ? " · " + t.workspace.split("/").pop() : "")}</small></span>
        {!t.closed && <i className="live-dot" aria-label="运行中" />}<ChevronRight size={16} />
      </button></li>;
    })}</ul>}
    {!data && !error && <p className="note center"><LoaderCircle size={16} className="spin" /> 正在读取终端…</p>}
    {data && <button type="button" className="link refresh" onClick={() => setTick((n) => n + 1)}>刷新列表</button>}
  </section>;
}
