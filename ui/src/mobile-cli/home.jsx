import React, { useMemo, useState } from "react";
import { ChevronRight, FolderOpen, Globe2, Monitor, Sparkles, SquareTerminal, Folder, Power } from "lucide-react";
import { Doodle } from "./doodles";
import { Composer, useAttachments } from "./composer";
import { InlineSelect } from "./inline-select";
import { ProviderBadge } from "./session-row";
import { environmentSummary } from "./environment-state.mjs";
import { runningText } from "./tasks.mjs";

export function greeting(date = new Date()) {
  const h = date.getHours();
  return h < 5 ? "夜深了" : h < 11 ? "早上好" : h < 13 ? "中午好" : h < 18 ? "下午好" : "晚上好";
}

export const projectFolder = (project) => project?.folders?.[0] || project?.path || "";

export function Home({ catalog, project, onProject, provider, onProvider, creating, onStart, onSession, onTool, environment, onEnvironment, serverVM, formatTime }) {
  const [draft, setDraft] = useState("");
  const attachments = useAttachments(projectFolder(project));
  const projects = catalog?.projects || [];
  const recent = useMemo(() => [...(catalog?.sessions || [])]
    .sort((a, b) => String(b.updated_at).localeCompare(String(a.updated_at))).slice(0, 5), [catalog?.sessions]);
  const names = useMemo(() => new Map(projects.flatMap((p) => (p.folders || [p.path]).map((f) => [f, p.name]))), [projects]);
  const codex = !!catalog?.codex;
  const env = environmentSummary(environment);
  const ready = !!catalog?.ready;
  const stopped = environment ? !environment.ready && !environment.vm_running && !environment.job?.running : !ready;
  const text = draft.trim();
  const canSend = !!project && ready && !creating && !attachments.uploading && (!!text || attachments.items.some((i) => i.status === "done"));
  const submit = () => {
    if (!canSend) return;
    onStart({ project, provider: codex ? provider : "claude", text: (text || "请查看附件。") + attachments.suffix() });
    setDraft(""); attachments.clear();
  };
  return <section className="home page-scroll" aria-label="开始">
    <div className="home-hero">
      <Doodle name="logo" className="home-logo" />
      <h1 className="greeting">{greeting()}</h1>
      <Doodle name="squiggle" className="greeting-underline" />
      <p className="home-sub">{project ? <>今天想在 <b>{project.name}</b> 做点什么？</> : "先在电脑上打开一个项目，就能在这里开始。"}</p>
    </div>
    {stopped && <button type="button" className="env-nudge" onClick={onEnvironment}>
      <Doodle name="moon" className="nudge-doodle" />
      <span><strong>环境还没有运行</strong><small>点一下启动，就能对话和用终端。</small></span>
      <ChevronRight size={18} />
    </button>}
    <Composer value={draft} onChange={setDraft} onSubmit={submit} canSend={canSend} busy={creating}
      placeholder={provider === "codex" && codex ? "让 Codex 做点什么…" : "让 Claude Code 做点什么…"}
      disabled={!project || creating} attachments={attachments}
      chips={<>
        {projects.length > 0 && <InlineSelect label="选择项目" icon={<FolderOpen size={15} />} value={project?.path || ""} disabled={creating}
          onChange={onProject} options={projects.map((p) => ({ value: p.path, label: p.name, optionLabel: p.name + " · " + p.path }))} />}
        {codex && <div className="segmented" role="radiogroup" aria-label="选择助手">
          {[["claude", "Claude"], ["codex", "Codex"]].map(([id, label]) => <button key={id} type="button" role="radio" aria-checked={provider === id}
            onClick={() => onProvider(id)}>{label}</button>)}
        </div>}
      </>} />
    <nav className="home-tools" aria-label="工具">
      <button type="button" onClick={() => onTool("terminal")}><SquareTerminal size={20} /><span>终端</span></button>
      <button type="button" onClick={() => onTool("files")}><Folder size={20} /><span>文件</span></button>
      <button type="button" onClick={onEnvironment}><Power size={20} /><span>环境<i className={"env-dot " + env.tone} aria-label={env.short} /></span></button>
      {!serverVM && <>
        <button type="button" onClick={() => onTool("browser")}><Globe2 size={20} /><span>浏览器</span></button>
        <button type="button" onClick={() => onTool("claude")}><Sparkles size={20} /><span>Claude.ai</span></button>
        <button type="button" onClick={() => onTool("desktop")}><Monitor size={20} /><span>Desktop</span></button>
      </>}
    </nav>
    {recent.length > 0 && <section className="recent" aria-label="最近的对话">
      <h2>最近</h2>
      <ul>{recent.map((s) => <li key={s.id}><button type="button" aria-label={(s.title || "新会话") + (s.running_tasks > 0 ? "，" + runningText(s.running_tasks) : "")} onClick={() => onSession(s.workspace, s.id)}>
        <span className="recent-title">{s.title || "新会话"}</span>
        <small><ProviderBadge provider={s.provider} />{names.get(s.workspace) || ""}{s.updated_at ? " · " + formatTime(s.updated_at) : ""}</small>
        {s.running_tasks > 0 && <small className="task-marker">{runningText(s.running_tasks)}</small>}
        {s.active && <i className="live-dot" aria-label="正在运行" />}
      </button></li>)}</ul>
    </section>}
  </section>;
}
