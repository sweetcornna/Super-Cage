import React, { useEffect, useRef, useState } from "react";
import { Download, ExternalLink, LogIn, Play, Power, RefreshCw, RotateCcw } from "lucide-react";
import { api, native, nativeAvailable } from "./api";
import { getClientContext } from "./client-context.mjs";
import { Drawing } from "./drawings";
import { SafetyMessage } from "./safety-message";
import { useWindowVisibility } from "./window-visibility";
import { hostFrameStatus, hostRenderGuide, hostRenderView, scienceView } from "./claude-science-state.mjs";
import "./claude-science.css";

// Claude Science (internal/workbench/claude_science.go): install, start, stop
// and update are Cage jobs followed through the workbench status; open mints a
// single-use link on the server and returns only the browser tab to show. The
// link and the response bodies never reach this file's logs or messages.
const confirmations = {
  stop: { title: "停止 Claude Science？", body: "正在进行的分析会中断，已打开的 Claude Science 页面将失效。项目和登录状态保留，之后可以再次启动。", confirm: "停止" },
  update: { title: "更新 Claude Science？", body: "更新会先停止 Claude Science，再安装新版本，已打开的页面将失效。项目和登录状态保留，更新后需要重新启动。", confirm: "更新" },
};
const icons = { install: Download, start: Play, open: ExternalLink, stop: Power, update: RefreshCw, refresh: RotateCcw };

// Host rendering (science_host.go, desktop/windows/ScienceFrame.cs). The
// window's bridge answers scienceFrame with op:
//   prepare -> {backstop}: drops any earlier identity; backstop says the
//              WebView runs with the network switches the frame needs.
//   arm {url} -> true: the guest's identity on the whole WebView, and frame
//              navigation allowed to that front only.
//   disarm -> true; restart -> true: the WebView again, with the switches.
const frameMessages = {
  restart: "本机渲染已开启，需要重新载入窗口一次才能生效。",
  reload: "窗口打开时本机渲染还没有就绪，重新载入窗口后生效。",
  timeout: "本机渲染页面没有在 20 秒内打开，已改用隔离浏览器。",
  dropped: "与本机渲染页面的连接已结束，已改用隔离浏览器。",
};
const frameFixes = { restart: "重新载入窗口", reload: "重新载入窗口", browser: "重试本机渲染", retry: "重试本机渲染" };
const frameSandbox = "allow-scripts allow-same-origin allow-forms allow-modals";
const hostMeasure = () => ({
  timezone: Intl.DateTimeFormat().resolvedOptions().timeZone || "",
  offset: new Date().getTimezoneOffset(),
  languages: [...(navigator.languages || [navigator.language])].filter(Boolean).slice(0, 8),
});
// Why the frame gave way, kept across visits to the pane until the switch
// changes or the window reloads, so a refused probe is not retried on every
// visit.
const remembered = { fallback: null };
class FrameFallback extends Error {
  constructor(message, fix) { super(message); this.fix = fix; }
}

function HostRenderSwitch({ toggle, pending, onChange }) {
  const windows = getClientContext().client_platform === "windows" && nativeAvailable();
  return (
    <label className="toggle-row science-host-render">
      <span>
        <strong>{toggle.label}</strong>
        <small>
          {toggle.explanation}{" "}
          <a href={hostRenderGuide} target="_blank" rel="noopener noreferrer" onClick={(event) => {
            if (!windows) return;
            event.preventDefault();
            void native("appReference", { kind: "claude-science-host-render" }).catch(() => {});
          }}>了解详情</a>
        </small>
      </span>
      <input type="checkbox" role="switch" checked={toggle.checked} disabled={toggle.disabled || pending}
        onChange={(event) => void onChange(event.target.checked)} />
    </label>
  );
}

// ScienceFrame is mounted only while hostRenderView says so; unmounting drops
// the window's overrides and the front's session.
function ScienceFrame({ data, toolbar, onFallback, onSignIn }) {
  const [url, setURL] = useState("");
  const [phase, setPhase] = useState("preparing");
  const [render, setRender] = useState(data.host_render);
  const disposed = useRef(false);
  const phaseRef = useRef("preparing");
  const retried = useRef(false);
  const seenSignIn = useRef(data.host_render?.signin?.at || "");
  const fallback = useRef(onFallback);
  fallback.current = onFallback;
  const signIn = useRef(onSignIn);
  signIn.current = onSignIn;
  const move = (next) => { phaseRef.current = next; setPhase(next); };
  const start = async () => {
    move("preparing");
    setURL("");
    try {
      const prepared = await native("scienceFrame", { op: "prepare" });
      if (disposed.current) return;
      if (!prepared?.backstop) throw new FrameFallback(frameMessages.restart, "restart");
      const ticket = await api("/claude-science/ticket", { host: hostMeasure() });
      if (disposed.current) return;
      await native("scienceFrame", { op: "arm", url: ticket.url });
      if (disposed.current) { void native("scienceFrame", { op: "disarm" }).catch(() => {}); return; }
      move("loading");
      setURL(ticket.url);
    } catch (error) {
      if (!disposed.current && error.status !== 423) fallback.current(error.message, error.fix || "retry");
    }
  };
  useEffect(() => {
    disposed.current = false;
    void start();
    return () => {
      disposed.current = true;
      void native("scienceFrame", { op: "disarm" }).catch(() => {});
      void api("/claude-science/release", {}).catch(() => {});
    };
  }, []);
  useEffect(() => {
    if (!url) return;
    const origin = new URL(url).origin;
    const blocked = (event) => {
      if (event.effectiveDirective === "frame-src" && event.blockedURI.startsWith(origin)) fallback.current(frameMessages.reload, "reload");
    };
    document.addEventListener("securitypolicyviolation", blocked);
    let timer;
    let stopped = false;
    let baseline = false;
    const opened = Date.now();
    const poll = async () => {
      try {
        const state = await api("/claude-science/host-render");
        if (stopped) return;
        setRender(state);
        const event = state.signin;
        // The first answer for this document only sets the baseline: its
        // page has not loaded far enough to ask for a sign-in.
        if (!baseline) { baseline = true; seenSignIn.current = event?.at || seenSignIn.current; }
        else if (event?.at && event.at !== seenSignIn.current) {
          seenSignIn.current = event.at;
          signIn.current(event);
        }
        if (state.refused) { fallback.current(state.refused, "browser"); return; }
        if (phaseRef.current === "loading" && state.session) move("ready");
        else if (phaseRef.current === "loading" && Date.now() - opened > 20000) { fallback.current(frameMessages.timeout, "browser"); return; }
        else if (phaseRef.current === "ready" && !state.session) {
          // The front restarted or dropped the session: one new ticket.
          if (retried.current) { fallback.current(frameMessages.dropped, "browser"); return; }
          retried.current = true;
          void start();
          return;
        }
      } catch (error) {
        if (stopped || error.status === 423) return;
      }
      timer = setTimeout(poll, phaseRef.current === "loading" ? 1000 : 2000);
    };
    timer = setTimeout(poll, 500);
    return () => {
      stopped = true;
      clearTimeout(timer);
      document.removeEventListener("securitypolicyviolation", blocked);
    };
  }, [url]);
  const status = hostFrameStatus({ ...data, host_render: render }, phase);
  return (
    <>
      <div className="science-frame-bar" role="toolbar" aria-label="Claude Science 本机渲染">
        <span className={"status-dot " + status.tone} aria-hidden="true" />
        {status.chips.map((chip) => <span key={chip.text} className={"chip" + (chip.tone ? " " + chip.tone : "")}>{chip.text}</span>)}
        <span className="science-frame-status" role="status">{status.text}</span>
        {toolbar}
      </div>
      <div className="science-frame-body">
        {url ? (
          <iframe key={url} src={url} title="Claude Science" sandbox={frameSandbox} allow="clipboard-write" referrerPolicy="no-referrer"
            data-phase={phase} />
        ) : <div className="science-frame-wait"><span className="live-dot" aria-hidden="true" />{status.text}</div>}
      </div>
    </>
  );
}

export function ClaudeSciencePanel({ status, onOpen, onConfirm, onDetails }) {
  const windowVisible = useWindowVisibility();
  const running = Boolean(status.vm_running);
  const [data, setData] = useState(null);
  const [readError, setReadError] = useState(null);
  const [actionError, setActionError] = useState("");
  const [pending, setPending] = useState("");
  const [jobID, setJobID] = useState("");
  const [fallback, setFallbackState] = useState(() => remembered.fallback);
  const setFallback = (value) => { remembered.fallback = value; setFallbackState(value); };
  const [hostPending, setHostPending] = useState(false);
  const [hostNotice, setHostNotice] = useState("");
  const mounted = useRef(true);
  const request = useRef(null);
  const revision = useRef(0);
  const submitted = useRef(null);
  const finishing = useRef("");
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);
  const refresh = async (valid = () => true) => {
    const current = revision.current;
    const read = request.current ??= api("/claude-science");
    try {
      const value = await read;
      if (!valid() || current !== revision.current) return null;
      setData(value);
      setReadError(null);
      return value;
    } catch (e) {
      if (valid() && current === revision.current) setReadError({ status: e.status, message: e.message });
      return null;
    } finally {
      if (request.current === read) request.current = null;
    }
  };
  useEffect(() => {
    if (!windowVisible || !running || status.job?.running) return;
    let disposed = false;
    let timer;
    const poll = async () => {
      await refresh(() => !disposed);
      if (disposed) return;
      // A job this pane started that the status feed never reported back.
      if (submitted.current && Date.now() - submitted.current > 30000) {
        submitted.current = null;
        setJobID("");
        setPending("");
      }
      timer = setTimeout(poll, 5000);
    };
    poll();
    return () => {
      disposed = true;
      clearTimeout(timer);
    };
  }, [windowVisible, running, status.job?.running]);
  useEffect(() => {
    if (!jobID || status.job?.id !== jobID || status.job.running || finishing.current === jobID) return;
    // Keep the job's state on screen until the re-read answers, so the pane
    // does not flash the state from before the job.
    finishing.current = jobID;
    void refresh().finally(() => {
      submitted.current = null;
      setJobID("");
      setPending("");
    });
  }, [status.job, jobID]);
  const submit = async (name) => {
    revision.current++;
    request.current = null;
    setPending(name);
    setActionError("");
    try {
      const job = await api(`/claude-science/${name}`, {});
      submitted.current = Date.now();
      setJobID(job.id);
    } catch (e) {
      setActionError(e.message);
      setPending("");
    }
  };
  const open = async (fresh = false) => {
    setPending("open");
    setActionError("");
    try {
      const tab = await api("/claude-science/open", fresh ? { fresh: true } : {});
      onOpen({ id: tab.id, kind: tab.kind, title: tab.title });
    } catch (e) {
      if (mounted.current) setActionError(e.message);
    } finally {
      if (mounted.current) setPending("");
    }
  };
  const act = async (item) => {
    if (item.action === "refresh") { setReadError(null); void refresh(); return; }
    if (item.action === "open") { await open(); return; }
    const ask = item.confirm && confirmations[item.action];
    if (ask && !await onConfirm(ask)) return;
    await submit(item.action);
  };
  const setHostRender = async (enabled) => {
    setHostPending(true);
    setActionError("");
    try {
      await api("/claude-science/host-render", { enabled });
      revision.current++;
      request.current = null;
      await refresh();
      // Only after the new state is on screen: the frame must not start
      // again from the old one.
      if (mounted.current) {
        setFallback(null);
        setHostNotice("");
      }
    } catch (e) {
      if (mounted.current) setActionError(e.message);
    } finally {
      if (mounted.current) setHostPending(false);
    }
  };
  const frameFallback = (reason, fix) => {
    if (!mounted.current) return;
    setFallback({ reason, fix });
    // The probe's refusal says the isolated browser opens: it does.
    if (fix === "browser") void open();
  };
  const frameSignIn = (event) => {
    if (event.tab) onOpen({ id: event.tab.id, kind: event.tab.kind, title: event.tab.title });
    else if (event.error) setHostNotice(event.error);
  };
  const fixFrame = () => {
    if (fallback?.fix === "restart") void native("scienceFrame", { op: "restart" }).catch((e) => setActionError(e.message));
    else if (fallback?.fix === "reload") location.reload();
    else setFallback(null);
  };
  // The details dialog reads status.job; offer it only when a job is on record.
  const details = status.job?.log ? onDetails : undefined;
  const view = scienceView({ data, error: readError, actionError, job: status.job, running, ready: Boolean(status.ready), pending });
  const scienceJob = Boolean(data?.operation) || (status.job?.running && String(status.job.kind || "").startsWith("claude-science")) || ["install", "update", "start", "stop"].includes(pending);
  const host = hostRenderView({ data, client: getClientContext(), bridge: nativeAvailable(), running, locked: readError?.status === 423, busy: scienceJob, fallback: fallback?.reason || "" });
  const button = (item, className) => {
    const Icon = icons[item.action];
    return (
      <button key={item.action} className={className} disabled={item.disabled} onClick={() => void act(item)}>
        {Icon && <Icon size={16} aria-hidden="true" />}
        {item.label}
      </button>
    );
  };
  const hostSwitch = host.toggle && <HostRenderSwitch toggle={host.toggle} pending={hostPending} onChange={setHostRender} />;
  if (host.frame) {
    const tool = (label, Icon, onClick, disabled = false) => (
      <button className="quiet-button" disabled={disabled || Boolean(pending)} onClick={onClick}>
        <Icon size={15} aria-hidden="true" />{label}
      </button>
    );
    return (
      <section className="claude-science" data-state="frame">
        {(actionError || hostNotice) && (
          <div className="science-notice" data-tone="error" role="alert"><span><SafetyMessage message={actionError || hostNotice} /></span></div>
        )}
        <ScienceFrame data={data} onFallback={frameFallback} onSignIn={frameSignIn} toolbar={<>
          {data.signed_in !== true && tool("登录", LogIn, () => void open(true))}
          {tool("在隔离浏览器中打开", ExternalLink, () => void open())}
          {tool("停止", Power, () => void act({ action: "stop", confirm: true }))}
          <label className="science-frame-switch">
            <input type="checkbox" role="switch" checked disabled={hostPending} aria-label={host.toggle.label}
              onChange={(event) => void setHostRender(event.target.checked)} />
            <span>{host.toggle.label}</span>
          </label>
        </>} />
      </section>
    );
  }
  return (
    <section className="claude-science" data-state={view.state} aria-busy={view.state === "loading" || undefined}>
      {host.fallback && (
        <div className="science-notice" data-tone="warn" role="status">
          <span><strong>本机渲染</strong><SafetyMessage message={host.fallback} /></span>
          {frameFixes[fallback?.fix] && <button className="quiet-button" onClick={fixFrame}>{frameFixes[fallback.fix]}</button>}
        </div>
      )}
      {host.notice && !host.fallback && (
        <div className="science-notice" data-tone="warn" role="status"><span><SafetyMessage message={host.notice} /></span></div>
      )}
      {view.notices.map((notice, index) => (
        <div key={index} className="science-notice" data-tone={notice.tone} role={notice.tone === "error" ? "alert" : "status"}>
          <span>
            {notice.label && <strong>{notice.label}</strong>}
            <SafetyMessage message={notice.text} />
          </span>
          {notice.details && details && <button className="quiet-button" onClick={details}>操作详情</button>}
        </div>
      ))}
      <div className={"science-card" + (view.drawing ? " hero" : "")}>
        {view.drawing && <Drawing name={view.drawing} />}
        <div className="science-head">
          {view.tone && <span className={"status-dot " + view.tone} aria-hidden="true" />}
          <h2 className="science-title">{view.title}</h2>
          {view.chips.length > 0 && (
            <div className="science-chips">
              {view.chips.map((chip) => <span key={chip.text} className={"chip" + (chip.tone ? " " + chip.tone : "")}>{chip.text}</span>)}
            </div>
          )}
        </div>
        {view.lead && <p className="science-lead">{view.lead}</p>}
        {view.facts.length > 0 && (
          <dl className="science-facts">
            {view.facts.map((fact) => (
              <div key={fact.label} data-tone={fact.tone || undefined}>
                <dt>{fact.label}</dt>
                <dd>{fact.value}</dd>
              </div>
            ))}
          </dl>
        )}
        {view.note && <p className="science-note">{view.note}</p>}
        {view.progress && (
          <div className="science-progress" role="status">
            <span className="live-dot" aria-hidden="true" />
            <span className="science-progress-text">{view.progress.message}</span>
            {details && <button className="text-button" onClick={details}>查看进度</button>}
          </div>
        )}
        {view.log.length > 0 && <pre className="science-log" aria-label="操作日志">{view.log.join("\n")}</pre>}
        {(view.primary || view.secondary.length > 0) && (
          <div className="science-actions">
            {view.primary && button(view.primary, "clay large")}
            {view.secondary.map((item) => button(item, "quiet-button"))}
          </div>
        )}
        {view.hint && <p className="science-hint">{view.hint}</p>}
      </div>
      {hostSwitch}
    </section>
  );
}
