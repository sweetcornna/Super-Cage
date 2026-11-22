import React, { useEffect, useMemo, useRef, useState } from "react";
import { ArrowLeft, ChevronRight, File, FileCode, FileImage, FileText, Folder, FolderPlus, LoaderCircle, RefreshCw, Share, Upload, WrapText, Link2 } from "lucide-react";
import { Doodle } from "./doodles";
import { Sheet } from "./sheet";
import { InlineSelect } from "./inline-select";
import { bytesToBase64, files, formatSize, maxPreview } from "./services.mjs";
import { accessDenied } from "./transport.mjs";
import { haptic, shareFile } from "./device-navigation.mjs";
import { MessageBody, TranscriptContext } from "./transcript";
import { deviceCommand } from "./device-navigation.mjs";
import { openExternal } from "./web-shell.mjs";

const previewContext = { sessionID: "", openWeb: (url) => { if (!deviceCommand("open-url", { url })) openExternal(url); } };

const imageTypes = /\.(png|jpe?g|gif|webp|heic|heif|bmp|svg|avif)$/i;
const codeTypes = /\.(js|mjs|cjs|jsx|ts|tsx|go|py|rs|java|kt|swift|c|h|cc|cpp|hpp|cs|rb|php|sh|bash|zsh|fish|ps1|sql|toml|ya?ml|json|xml|html|css|scss|lua|r|jl|ipynb|lock|ini|conf|env)$/i;
const textNames = /^(Makefile|Dockerfile|LICENSE|README|AGENTS|CLAUDE|\.gitignore|\.gitattributes|\.editorconfig)$/i;
function iconFor(entry) {
  if (entry.type === "dir") return Folder;
  if (entry.type === "symlink") return Link2;
  if (imageTypes.test(entry.name)) return FileImage;
  if (codeTypes.test(entry.name)) return FileCode;
  if (/\.(md|txt|log|csv|tsv|rst)$/i.test(entry.name) || textNames.test(entry.name)) return FileText;
  return File;
}
const join = (base, name) => (base ? base + "/" : "") + name;
const parent = (path) => path.includes("/") ? path.slice(0, path.lastIndexOf("/")) : "";
const when = new Intl.DateTimeFormat("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit" });
const mtime = (value) => { const d = new Date(typeof value === "number" ? value * (value < 1e12 ? 1000 : 1) : value); return Number.isNaN(d.valueOf()) ? "" : when.format(d); };
function looksText(bytes) {
  const n = Math.min(bytes.length, 8192);
  for (let i = 0; i < n; i++) if (bytes[i] === 0) return false;
  return true;
}

export function FilesView({ catalog, project, onProject, path, onPath, onDenied, visible }) {
  const folders = project?.folders?.length ? project.folders : project?.path ? [project.path] : [];
  const [root, setRoot] = useState(folders[0] || "");
  useEffect(() => { if (!folders.includes(root)) setRoot(folders[0] || ""); }, [folders.join("\n")]);
  const [listing, setListing] = useState(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [tick, setTick] = useState(0);
  const [preview, setPreview] = useState(null);
  const [uploads, setUploads] = useState([]);
  const [mkdir, setMkdir] = useState(false);
  const picker = useRef(null);
  useEffect(() => {
    if (!visible || !root) return;
    const controller = new AbortController();
    setLoading(true);
    files.list(root, path, { signal: controller.signal })
      .then((next) => { setListing(next); setError(""); })
      .catch((err) => {
        if (controller.signal.aborted) return;
        if (accessDenied(err)) onDenied?.(err);
        else setError(err.status === 404 && !path ? "电脑上的 Super Cage 版本较旧，暂不支持在手机上浏览文件。请先升级电脑端。" : err.message);
      })
      .finally(() => { if (!controller.signal.aborted) setLoading(false); });
    return () => controller.abort();
  }, [root, path, tick, visible, onDenied]);
  const crumbs = useMemo(() => path ? path.split("/") : [], [path]);
  const upload = (list) => {
    for (const file of list) {
      const id = crypto.randomUUID();
      setUploads((old) => [...old, { id, name: file.name, progress: 0 }]);
      files.upload(root, path, file, { onProgress: (done, total) => setUploads((old) => old.map((u) => u.id === id ? { ...u, progress: total ? done / total : 1 } : u)) })
        .then(() => { haptic("success"); setUploads((old) => old.filter((u) => u.id !== id)); setTick((n) => n + 1); })
        .catch((err) => setUploads((old) => old.map((u) => u.id === id ? { ...u, error: err.status === 409 ? "已有同名文件" : err.message } : u)));
    }
  };
  if (!folders.length) return <section className="files page-scroll"><div className="empty-art"><Doodle name="folder" className="empty-doodle" /><p>还没有项目。先在电脑上打开一个项目。</p></div></section>;
  const projects = catalog?.projects || [];
  return <section className="files" aria-label="文件">
    <header className="files-head">
      <div className="files-pickers">
        <InlineSelect label="选择项目" icon={<Folder size={15} />} value={project?.path || ""} onChange={(value) => { onProject(value); onPath(""); }}
          options={projects.map((p) => ({ value: p.path, label: p.name, optionLabel: p.name + " · " + p.path }))} />
        {folders.length > 1 && <InlineSelect label="选择文件夹" value={root} onChange={(value) => { setRoot(value); onPath(""); }}
          options={folders.map((f) => ({ value: f, label: f.split("/").pop() || f, optionLabel: f }))} />}
      </div>
      {path && <nav className="crumbs" aria-label="路径">
        <button type="button" onClick={() => onPath("")} aria-current={!path ? "page" : undefined}>{root.split("/").pop() || root}</button>
        {crumbs.map((name, i) => <React.Fragment key={i}><ChevronRight size={14} />
          <button type="button" aria-current={i === crumbs.length - 1 ? "page" : undefined} onClick={() => onPath(crumbs.slice(0, i + 1).join("/"))}>{name}</button></React.Fragment>)}
      </nav>}
    </header>
    <div className="page-scroll files-body">
      {error && <div className="notice" role="alert">{error}<button type="button" className="action secondary" onClick={() => setTick((n) => n + 1)}><RefreshCw size={15} />重新读取</button></div>}
      {uploads.length > 0 && <ul className="upload-list">{uploads.map((u) => <li key={u.id} className={u.error ? "bad" : ""}>
        <Upload size={15} /><span>{u.name}</span><small>{u.error || Math.round(u.progress * 100) + "%"}</small>
        {u.error && <button type="button" className="link" onClick={() => setUploads((old) => old.filter((x) => x.id !== u.id))}>知道了</button>}
      </li>)}</ul>}
      {!listing && loading && <p className="note center"><LoaderCircle size={16} className="spin" /> 正在读取…</p>}
      {listing && <ul className="file-list">
        {path && <li><button type="button" onClick={() => onPath(parent(path))}><ArrowLeft size={18} /><span className="file-name">上一级</span></button></li>}
        {listing.entries.map((entry) => {
          const Icon = iconFor(entry);
          return <li key={entry.name}><button type="button" className={entry.type} disabled={entry.type === "other" || entry.type === "symlink"}
            onClick={() => entry.type === "dir" ? onPath(join(path, entry.name)) : setPreview(entry)}>
            <Icon size={19} /><span className="file-name">{entry.name}<small>{entry.type === "dir" ? mtime(entry.mtime) : formatSize(entry.size) + " · " + mtime(entry.mtime)}</small></span>
            {entry.type === "dir" && <ChevronRight size={16} />}
          </button></li>;
        })}
        {!listing.entries.length && <li className="empty-art small"><Doodle name="folder" className="empty-doodle" /><p>这个文件夹是空的</p></li>}
      </ul>}
      {listing?.truncated && <p className="note">只显示前 1000 项。</p>}
    </div>
    <footer className="files-actions">
      <button type="button" className="action secondary" disabled={!root} onClick={() => setMkdir(true)}><FolderPlus size={17} />新建文件夹</button>
      <button type="button" className="action primary" disabled={!root} onClick={() => picker.current?.click()}><Upload size={17} />上传</button>
      <input ref={picker} type="file" multiple hidden onChange={(e) => { upload([...e.target.files]); e.target.value = ""; }} />
    </footer>
    {preview && <Preview root={root} path={join(path, preview.name)} entry={preview} onClose={() => setPreview(null)} onDenied={onDenied} />}
    {mkdir && <MakeFolder onClose={() => setMkdir(false)} onCreate={async (name) => {
      await files.mkdir(root, join(path, name)); setMkdir(false); setTick((n) => n + 1); haptic("success");
    }} />}
  </section>;
}

function MakeFolder({ onClose, onCreate }) {
  const [name, setName] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const valid = name.trim() && !/[\\/\u0000-\u001f]/.test(name) && name.trim() !== "." && name.trim() !== "..";
  return <Sheet title="新建文件夹" onClose={busy ? undefined : onClose} className="confirm"
    actions={<><button type="button" className="action secondary" disabled={busy} onClick={onClose}>取消</button>
      <button type="button" className="action primary" disabled={busy || !valid} onClick={async () => {
        setBusy(true); setError("");
        try { await onCreate(name.trim()); } catch (err) { setError(err.status === 409 ? "已有同名文件或文件夹。" : err.message); } finally { setBusy(false); }
      }}>创建</button></>}>
    <label className="field">名称<input autoFocus value={name} maxLength={120} onChange={(e) => setName(e.target.value)} /></label>
    {error && <p className="session-action-error" role="alert">{error}</p>}
  </Sheet>;
}

function Preview({ root, path, entry, onClose, onDenied }) {
  const [state, setState] = useState({ loading: true });
  const [wrap, setWrap] = useState(true);
  useEffect(() => {
    const controller = new AbortController();
    let url = "";
    files.read(root, path, { signal: controller.signal, limit: maxPreview, onProgress: (done, total) => setState((s) => ({ ...s, progress: total ? done / total : 0 })) })
      .then(({ bytes, size, mime, complete }) => {
        const image = (mime || "").startsWith("image/") || imageTypes.test(entry.name);
        if (image && complete) { url = URL.createObjectURL(new Blob([bytes], { type: mime || "image/*" })); setState({ image: url, bytes, size, mime }); }
        else if (looksText(bytes)) setState({ text: new TextDecoder().decode(bytes), bytes, size, mime, complete });
        else setState({ binary: true, bytes, size, mime, complete });
      })
      .catch((err) => { if (controller.signal.aborted) return; if (accessDenied(err)) onDenied?.(err); else setState({ error: err.message }); });
    return () => { controller.abort(); if (url) URL.revokeObjectURL(url); };
  }, [root, path]);
  const share = () => {
    if (!state.bytes) return;
    const data = bytesToBase64(state.bytes);
    if (!shareFile(entry.name, state.mime || "application/octet-stream", data)) {
      const a = document.createElement("a");
      a.href = URL.createObjectURL(new Blob([state.bytes], { type: state.mime || "application/octet-stream" }));
      a.download = entry.name; a.click(); setTimeout(() => URL.revokeObjectURL(a.href), 4000);
    }
  };
  const markdown = /\.md$/i.test(entry.name) && state.text != null;
  return <Sheet title={entry.name} onClose={onClose} full className="file-preview"
    actions={<>
      {state.text != null && !markdown && <button type="button" className="action secondary" aria-pressed={wrap} onClick={() => setWrap((v) => !v)}><WrapText size={16} />{wrap ? "不换行" : "自动换行"}</button>}
      <button type="button" className="action primary" disabled={!state.bytes || state.complete === false} onClick={share}><Share size={16} />导出</button>
    </>}>
    <p className="note">{formatSize(entry.size)} · {path}</p>
    {state.loading && <p className="note center"><LoaderCircle size={16} className="spin" /> 正在读取{state.progress ? " " + Math.round(state.progress * 100) + "%" : "…"}</p>}
    {state.error && <div className="notice" role="alert">{state.error}</div>}
    {state.image && <img className="preview-image" src={state.image} alt={entry.name} />}
    {markdown && <div className="preview-markdown"><TranscriptContext.Provider value={previewContext}><MessageBody text={state.text} /></TranscriptContext.Provider></div>}
    {state.text != null && !markdown && <pre className={"preview-text" + (wrap ? " wrap" : "")}>{state.text}</pre>}
    {state.complete === false && <p className="note">文件较大，只显示前 8 MB。</p>}
    {state.binary && <div className="empty-art small"><Doodle name="folder" className="empty-doodle" /><p>这个文件无法直接预览，可以导出到其他应用打开。</p></div>}
  </Sheet>;
}
