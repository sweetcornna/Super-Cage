import React, { useEffect, useRef, useState } from "react";
import { ArrowLeft, Minus, Plus, X } from "lucide-react";
import { Terminal } from "../../.generated/xterm.mjs";
import { FitAddon } from "@xterm/addon-fit";
import { Unicode11Addon } from "@xterm/addon-unicode11";
import { WebLinksAddon } from "@xterm/addon-web-links";
import "@xterm/xterm/css/xterm.css";
import { base64ToBytes, terminals } from "./services.mjs";
import { accessDenied } from "./transport.mjs";
import { deviceCommand, haptic } from "./device-navigation.mjs";
import { openExternal } from "./web-shell.mjs";
import { Confirm } from "./sheet";
import { kindInfo } from "./terminals";
import { takeInput } from "../terminal-input-chunks.mjs";
import { attachTouchScroll } from "../terminal-touch-scroll.mjs";

const light = { background: "#faf9f5", foreground: "#1f1e1d", cursor: "#d97757", cursorAccent: "#faf9f5", selectionBackground: "#e9d3c6",
  black: "#1f1e1d", red: "#b3261e", green: "#2e7d4f", yellow: "#8a5a00", blue: "#2f5d8a", magenta: "#8c3f72", cyan: "#2c6e6e", white: "#d9d4c7",
  brightBlack: "#6b655b", brightRed: "#d0483e", brightGreen: "#3f9a64", brightYellow: "#a87412", brightBlue: "#3f74a8", brightMagenta: "#a8558b", brightCyan: "#3a8a8a", brightWhite: "#f3efe5" };
const dark = { background: "#1f1e1d", foreground: "#f3efe5", cursor: "#d97757", cursorAccent: "#1f1e1d", selectionBackground: "#5a4034",
  black: "#2a2926", red: "#f2867c", green: "#7bc79a", yellow: "#e7c07b", blue: "#8fb5de", magenta: "#d79ac4", cyan: "#86c9c6", white: "#d9d4c7",
  brightBlack: "#7d776c", brightRed: "#ffa39a", brightGreen: "#98dcb2", brightYellow: "#f2d29a", brightBlue: "#acc9ea", brightMagenta: "#e6b5d6", brightCyan: "#a3dcd9", brightWhite: "#ffffff" };
const darkQuery = matchMedia("(prefers-color-scheme: dark)");
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export default function TerminalScreen({ id, title, kind, visible, onBack, onDenied }) {
  const host = useRef(null);
  const term = useRef(null);
  const fit = useRef(null);
  const after = useRef(0);
  const queue = useRef("");
  const sending = useRef(false);
  const alive = useRef(true);
  const ctrl = useRef(false);
  const lastSize = useRef("");
  const [ctrlOn, setCtrlOn] = useState(false);
  const [state, setState] = useState({ connecting: true, closed: false, exit: null, error: "" });
  const [fontSize, setFontSize] = useState(() => Math.min(20, Math.max(10, Number(localStorage.getItem("super-cage-terminal-font")) || 13)));
  const [closing, setClosing] = useState(false);
  const [askClose, setAskClose] = useState(false);

  const pump = async () => {
    if (sending.current) return;
    sending.current = true;
    while (alive.current && queue.current) {
      const chunk = takeInput(queue.current);
      queue.current = queue.current.slice(chunk.length);
      try { await terminals.input(id, chunk); }
      catch (err) {
        if (err.status === 429) { queue.current = chunk + queue.current; await sleep(200); continue; }
        if (accessDenied(err)) onDenied?.(err);
        else setState((s) => ({ ...s, error: err.message }));
        queue.current = "";
      }
    }
    sending.current = false;
  };
  const send = (data) => { if (!data) return; queue.current += data; void pump(); };

  useEffect(() => {
    alive.current = true;
    const t = new Terminal({ fontFamily: 'ui-monospace, "SF Mono", Menlo, monospace', fontSize, lineHeight: 1.12, cursorBlink: true,
      scrollback: 5000, theme: darkQuery.matches ? dark : light, allowProposedApi: true, macOptionIsMeta: true });
    const f = new FitAddon();
    t.loadAddon(f);
    // Wide characters (emoji) take two cells, as the programs count them.
    t.loadAddon(new Unicode11Addon());
    t.unicode.activeVersion = "11";
    t.loadAddon(new WebLinksAddon((event, uri) => { if (!deviceCommand("open-url", { url: uri })) openExternal(uri); }));
    t.open(host.current);
    term.current = t; fit.current = f;
    // A finger dragged over the terminal scrolls it as a wheel would.
    const stopTouch = attachTouchScroll(t, host.current);
    const data = t.onData((value) => {
      if (ctrl.current && value.length === 1) {
        const code = value.toUpperCase().charCodeAt(0);
        ctrl.current = false; setCtrlOn(false);
        if (code >= 64 && code <= 95) { send(String.fromCharCode(code - 64)); return; }
      }
      send(value);
    });
    const binary = t.onBinary((value) => send(value));
    const theme = () => { t.options.theme = darkQuery.matches ? dark : light; };
    darkQuery.addEventListener("change", theme);
    let timer;
    const resize = () => {
      try { f.fit(); } catch { return; }
      const size = t.cols + "x" + t.rows;
      if (size === lastSize.current || t.cols < 2 || t.rows < 2) return;
      clearTimeout(timer);
      timer = setTimeout(() => { lastSize.current = size; terminals.resize(id, t.cols, t.rows).catch(() => {}); }, 250);
    };
    const observer = new ResizeObserver(resize);
    observer.observe(host.current);
    window.visualViewport?.addEventListener("resize", resize);
    requestAnimationFrame(resize);
    return () => {
      alive.current = false; clearTimeout(timer); observer.disconnect();
      stopTouch();
      window.visualViewport?.removeEventListener("resize", resize);
      darkQuery.removeEventListener("change", theme);
      data.dispose(); binary.dispose(); t.dispose();
    };
  }, [id]);

  useEffect(() => {
    if (!term.current) return;
    term.current.options.fontSize = fontSize;
    localStorage.setItem("super-cage-terminal-font", String(fontSize));
    try { fit.current.fit(); } catch { /* not visible yet */ }
  }, [fontSize]);

  useEffect(() => {
    if (!visible) return;
    const controller = new AbortController();
    (async () => {
      let failures = 0;
      while (!controller.signal.aborted) {
        try {
          const reply = await terminals.output(id, after.current, { signal: controller.signal });
          if (controller.signal.aborted) return;
          failures = 0;
          setState((s) => s.connecting || s.error ? { ...s, connecting: false, error: "" } : s);
          if (reply) {
            if (reply.reset) term.current?.reset();
            if (reply.data) term.current?.write(base64ToBytes(reply.data));
            after.current = reply.next;
            if (reply.closed) { setState((s) => ({ ...s, closed: true, exit: reply.exit_code ?? null })); return; }
          }
        } catch (err) {
          if (controller.signal.aborted) return;
          if (accessDenied(err)) { onDenied?.(err); return; }
          if (err.status === 404) { setState((s) => ({ ...s, connecting: false, closed: true, error: "终端已关闭。" })); return; }
          failures++;
          setState((s) => ({ ...s, connecting: false, error: "连接中断，正在重新连接（" + failures + "）…" }));
          await sleep(Math.min(1000 * failures, 5000));
        }
      }
    })();
    return () => controller.abort();
  }, [id, visible, onDenied]);

  const arrow = (dir) => send((term.current?.modes?.applicationCursorKeysMode ? "\x1bO" : "\x1b[") + dir);
  const keys = [
    ["Esc", () => send("\x1b")], ["Tab", () => send("\t")], ["Ctrl", null], ["^C", () => send("\x03")],
    ["↑", () => arrow("A")], ["↓", () => arrow("B")], ["←", () => arrow("D")], ["→", () => arrow("C")],
    ["/", () => send("/")], ["-", () => send("-")], ["|", () => send("|")], ["~", () => send("~")],
  ];
  const close = async () => {
    setClosing(true);
    try { await terminals.close(id); haptic("success"); onBack(); }
    catch (err) { if (accessDenied(err)) onDenied?.(err); else setState((s) => ({ ...s, error: err.message })); }
    finally { setClosing(false); setAskClose(false); }
  };
  const Icon = kindInfo[kind]?.icon;
  return <section className="terminal-screen" aria-label={title || "终端"}>
    <header className="terminal-head">
      <button type="button" className="icon-button" aria-label="返回终端列表" onClick={onBack}><ArrowLeft size={19} /></button>
      <span className="terminal-title">{Icon && <Icon size={16} />}<strong>{title || "终端"}</strong>
        {state.closed ? <small className="chip off">已结束{state.exit != null ? " · " + state.exit : ""}</small> : state.error ? <small className="chip busy">重连中</small> : null}</span>
      <button type="button" className="icon-button" aria-label="缩小字号" onClick={() => setFontSize((n) => Math.max(10, n - 1))}><Minus size={17} /></button>
      <button type="button" className="icon-button" aria-label="放大字号" onClick={() => setFontSize((n) => Math.min(20, n + 1))}><Plus size={17} /></button>
      {!state.closed && <button type="button" className="icon-button" aria-label="关闭终端" onClick={() => setAskClose(true)}><X size={18} /></button>}
    </header>
    {state.error && !state.closed && <p className="terminal-status" role="status">{state.error}</p>}
    <div className="terminal-host" ref={host} onClick={() => term.current?.focus()} />
    {!state.closed && <div className="key-bar" role="toolbar" aria-label="快捷按键" onPointerDown={(e) => e.preventDefault()}>
      {keys.map(([label, action]) => <button key={label} type="button" aria-pressed={label === "Ctrl" ? ctrlOn : undefined}
        className={label === "Ctrl" && ctrlOn ? "on" : undefined}
        onClick={() => { haptic("light"); if (action) action(); else { ctrl.current = !ctrl.current; setCtrlOn(ctrl.current); } term.current?.focus(); }}>{label}</button>)}
    </div>}
    {askClose && <Confirm title="关闭这个终端？" confirm="关闭终端" danger busy={closing} onCancel={() => setAskClose(false)} onConfirm={close}>
      终端里正在运行的程序会结束。</Confirm>}
  </section>;
}
